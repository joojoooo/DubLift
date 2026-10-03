package dublift

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const driveRateLimitBody = `{"error":{"code":403,"message":"private signed URL and credentials","errors":[{"reason":"rateLimitExceeded"}],"status":"PERMISSION_DENIED"}}`

func TestOriginRateLimitRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"Drive project quota", driveRateLimitBody, 403},
		{"Drive user quota", `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`, 403},
		{"Google error details", `{"error":{"details":[{"reason":"RATE_LIMIT_EXCEEDED"}]}}`, 403},
		{"HTTP rate limit", "too many requests", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, redirects atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=4-7" || r.Header.Get("X-Required") != "keep" || r.Header.Get("Referer") != "" {
					t.Error("retry changed range, explicit headers, or redirect referrer")
				}
				if calls.Add(1) <= 3 {
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
					return
				}
				w.Header().Set("Content-Range", "bytes 4-7/8")
				w.WriteHeader(206)
				io.WriteString(w, "4567")
			}))
			defer origin.Close()
			entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirects.Add(1)
				http.Redirect(w, r, origin.URL+"/worker?private=secret", http.StatusTemporaryRedirect)
			}))
			defer entry.Close()
			n := NewNetwork()
			var delays []time.Duration
			n.rateLimitWait = func(_ context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				return nil
			}
			f := &RemoteFile{Net: n, Origin: Origin{entry.URL + "/entry", http.Header{"X-Required": {"keep"}}}, Size: 8}
			got := make([]byte, 4)
			if _, err := f.ReadAtContext(context.Background(), got, 4); err != nil || string(got) != "4567" {
				t.Fatalf("quota recovery: %q, %v", got, err)
			}
			if calls.Load() != 4 || redirects.Load() != 4 || len(delays) != 3 {
				t.Fatalf("requests=%d redirects=%d delays=%v", calls.Load(), redirects.Load(), delays)
			}
			for i, delay := range delays {
				base := time.Second << i
				if delay < base || delay >= base+time.Second {
					t.Fatalf("retry %d delay=%s, expected %s plus jitter", i, delay, base)
				}
			}
		})
	}
}

func TestOriginPermissionErrorsAreNotRateLimits(t *testing.T) {
	for _, body := range []string{
		"permission denied",
		`{"error":{"message":"rateLimitExceeded","errors":[{"reason":"insufficientFilePermissions"}]}}`,
		`{"error":{"errors":[{"reason":"dailyLimitExceeded"}]}}`,
		strings.Repeat("access denied ", 2000),
	} {
		var calls atomic.Int32
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(403)
			io.WriteString(w, body)
		}))
		n := NewNetwork()
		n.rateLimitWait = func(context.Context, time.Duration) error {
			t.Error("permanent permission failure triggered quota backoff")
			return nil
		}
		resp, err := n.request(context.Background(), Origin{URL: origin.URL}, "GET", "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		origin.Close()
		if err != nil || resp.StatusCode != 403 || string(got) != body || calls.Load() != 1 {
			t.Fatalf("permission response changed: status=%d bytes=%d calls=%d err=%v", resp.StatusCode, len(got), calls.Load(), err)
		}
	}
}

func TestOriginRateLimitRetriesAreBoundedAndPrivate(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(403)
		io.WriteString(w, driveRateLimitBody)
	}))
	defer origin.Close()
	n := NewNetwork()
	n.rateLimitWait = func(context.Context, time.Duration) error { return nil }
	_, err := n.requestFileRange(context.Background(), Origin{URL: origin.URL + "/private?secret=token"}, "bytes=0-1")
	var status *HTTPError
	if !errors.Is(err, errOriginRateLimited) || !errors.As(err, &status) || status.Status != 403 || calls.Load() != originRateRetries+1 {
		t.Fatalf("unbounded or lost quota error: calls=%d error=%v", calls.Load(), err)
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "credentials") {
		t.Fatal("quota error exposed provider response or credentials", err)
	}
}

func TestOriginRateLimitWaitCanBeCanceled(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(403)
		io.WriteString(w, driveRateLimitBody)
	}))
	defer origin.Close()
	n := NewNetwork()
	waiting := make(chan struct{})
	n.rateLimitWait = func(ctx context.Context, delay time.Duration) error {
		close(waiting)
		return waitOriginRetry(ctx, delay)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := n.requestFileRange(ctx, Origin{URL: origin.URL}, "bytes=0-1")
		done <- err
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("retry did not enter quota backoff")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("canceled retry continued: calls=%d error=%v", calls.Load(), err)
	}
}

func TestOriginRateLimitBackoffOutlivesRangeHeaderTimer(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(403)
			io.WriteString(w, driveRateLimitBody)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-1/2")
		w.WriteHeader(206)
		io.WriteString(w, "ok")
	}))
	defer origin.Close()
	n := NewNetwork()
	n.rangeHeaderTimeout = 50 * time.Millisecond
	n.rateLimitWait = func(ctx context.Context, delay time.Duration) error {
		if delay != 3*time.Second {
			t.Errorf("ignored Retry-After: %s", delay)
		}
		return waitOriginRetry(ctx, 3*n.rangeHeaderTimeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f := &RemoteFile{Net: n, Origin: Origin{URL: origin.URL}, Size: 2}
	got := make([]byte, 2)
	if _, err := f.ReadAtContext(ctx, got, 0); err != nil || string(got) != "ok" || calls.Load() != 2 {
		t.Fatalf("header timer interrupted quota recovery: %q calls=%d error=%v", got, calls.Load(), err)
	}
}

func TestOriginRetryAfterCannotOverflowOrBypassBudget(t *testing.T) {
	for _, header := range []string{"9223372036854775807", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", header)
			w.WriteHeader(429)
		}))
		n := NewNetwork()
		n.rateLimitWait = func(context.Context, time.Duration) error {
			t.Error("wait exceeded the retry budget")
			return nil
		}
		_, err := n.request(context.Background(), Origin{URL: origin.URL}, "GET", "")
		origin.Close()
		if !errors.Is(err, errOriginRateLimited) {
			t.Fatal("Retry-After beyond budget was not handled", err)
		}
	}
}

func TestFileRateLimitDuringPreparationAndPlayback(t *testing.T) {
	dir := fixture(t)
	var mu sync.Mutex
	attempts := map[string]int{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		span := r.Header.Get("Range")
		mu.Lock()
		attempts[span]++
		attempt := attempts[span]
		mu.Unlock()
		if attempt <= 3 {
			w.WriteHeader(403)
			io.WriteString(w, driveRateLimitBody)
			return
		}
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	defer origin.Close()
	engine := testEngine(t)
	engine.Net.rateLimitWait = func(context.Context, time.Duration) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, err := engine.OpenAsset(ctx, Origin{URL: origin.URL + "/source.mkv"}, false)
	if err != nil {
		t.Fatal("quota prevented file indexing", err)
	}
	data, err := engine.Video(ctx, a, a.Index.Boundaries[1], a.Index.Boundaries[2]-a.Index.Boundaries[1])
	if err != nil || len(data) == 0 {
		t.Fatal("quota prevented seeked video remux", err)
	}
	path := filepath.Join(t.TempDir(), "recovered.mp4")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if got := command(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_type", "-of", "csv=p=0", path); !bytes.Contains(got, []byte("video")) {
		t.Fatalf("recovered remux has no video: %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts["bytes=0-65535"] != 4 || len(attempts) < 2 {
		t.Fatal("did not cover quota recovery for initial and later ranges", fmt.Sprint(attempts))
	}
}
