package dublift

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileRangeCacheReusesOverlappingPlaybackAndAnalysisBytes(t *testing.T) {
	engine := testEngine(t)
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	var downloaded atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end, err := requestRange(r.Header.Get("Range"), int64(len(payload)))
		if err != nil {
			t.Error(err)
			w.WriteHeader(416)
			return
		}
		downloaded.Add(int32(end - start + 1))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(206)
		w.Write(payload[start : end+1])
	}))
	defer origin.Close()
	asset := &Asset{ID: "shared-file", File: &RemoteFile{Net: engine.Net, Origin: Origin{URL: origin.URL}, Size: int64(len(payload))}}
	ctx := context.WithValue(context.Background(), cacheOwnerKey{}, "session")
	u, _, cleanup, err := engine.job(ctx, asset, 0, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	read := func(start, end int64) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != 206 || !bytes.Equal(got, payload[start:end+1]) {
			t.Fatalf("range %d-%d: status %d, %d bytes, %v", start, end, resp.StatusCode, len(got), err)
		}
	}
	read(0, 63)  // playback
	read(32, 95) // alignment: only the missing tail should be fetched
	read(48, 79) // entirely cached, despite a different Range header
	if downloaded.Load() != 96 {
		t.Fatalf("overlapping source bytes fetched twice: %d", downloaded.Load())
	}
	engine.Cache.DeleteOwner("session")
	if engine.Cache.Used() != 0 {
		t.Fatal("session range data survived cleanup")
	}
}

func TestFileRangeStreamsBeforeBoundedOriginReadFinishes(t *testing.T) {
	for _, video := range []bool{true, false} {
		t.Run(fmt.Sprintf("video=%t", video), func(t *testing.T) {
			engine := testEngine(t)
			const total = 33 << 20
			firstChunk := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var requests atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start, end, err := requestRange(r.Header.Get("Range"), total)
				if err != nil {
					t.Error(err)
					w.WriteHeader(416)
					return
				}
				requests.Add(1)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
				w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
				w.WriteHeader(206)
				if start == 0 {
					w.Write(make([]byte, 1<<20))
					w.(http.Flusher).Flush()
					close(firstChunk)
					<-release
					w.Write(make([]byte, end-start+1-(1<<20)))
				} else {
					w.Write(make([]byte, end-start+1))
				}
			}))
			defer origin.Close()
			asset := &Asset{ID: "streaming-file", File: &RemoteFile{Net: engine.Net, Origin: Origin{URL: origin.URL}, Size: total}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			u, _, cleanup, err := engine.job(ctx, asset, 0, 2, video)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", total-1))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			<-firstChunk
			firstRead := make(chan error, 1)
			go func() {
				_, err := io.ReadFull(resp.Body, make([]byte, 1<<20))
				firstRead <- err
			}()
			select {
			case err := <-firstRead:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("video proxy waited for the full origin range before delivering its first MiB")
			}
			releaseOnce.Do(func() { close(release) })
			if n, err := io.Copy(io.Discard, resp.Body); err != nil || n != total-(1<<20) {
				t.Fatalf("remaining video range: %d bytes, %v", n, err)
			}
			if got := requests.Load(); got != 2 {
				t.Fatalf("video proxy opened %d origin ranges, want 2", got)
			}

		})
	}
}

func TestClosingDemuxerRequestCancelsOriginRead(t *testing.T) {
	engine := testEngine(t)
	originCanceled := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-8388607/8388608")
		w.Header().Set("Content-Length", "8388608")
		w.WriteHeader(206)
		w.Write(make([]byte, 1<<20))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(originCanceled)
	}))
	defer origin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	asset := &Asset{ID: "cancel-read", File: &RemoteFile{Net: engine.Net, Origin: Origin{URL: origin.URL}, Size: 8 << 20}}
	u, _, cleanup, err := engine.job(ctx, asset, 0, 6, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Range", "bytes=0-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadFull(resp.Body, make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case <-originCanceled:
	case <-time.After(time.Second):
		t.Fatal("closed demuxer request kept downloading from the origin")
	}
	if ctx.Err() != nil {
		t.Fatal("origin only stopped after the whole extraction was canceled")
	}
}

func TestFileRangeResumesStalledBodyWithoutTruncatingDemuxerInput(t *testing.T) {
	engine := testEngine(t)
	engine.rangeReadTimeout = 100 * time.Millisecond
	payload := bytes.Repeat([]byte("recover-the-missing-bytes"), 100000)
	const partial = (1 << 20) + 12345
	var requests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end, err := requestRange(r.Header.Get("Range"), int64(len(payload)))
		if err != nil {
			t.Error(err)
			w.WriteHeader(416)
			return
		}
		attempt := requests.Add(1)
		if attempt == 2 && start != partial {
			t.Errorf("resumed at %d, want first unread byte %d", start, partial)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(206)
		if attempt == 1 {
			w.Write(payload[:partial])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Write(payload[start : end+1])
	}))
	defer origin.Close()
	asset := &Asset{ID: "stalled-file", File: &RemoteFile{Net: engine.Net, Origin: Origin{URL: origin.URL}, Size: int64(len(payload))}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, _, cleanup, err := engine.job(ctx, asset, 0, 6, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Range", "bytes=0-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, payload) || requests.Load() != 2 {
		t.Fatalf("resumed input: %d bytes, %d origin requests, %v", len(got), requests.Load(), err)
	}
}

func TestFileRangeCacheSharesOverlappingReadsInFlight(t *testing.T) {
	cache := NewByteCache(256)
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})
	var fetched atomic.Int32
	build := func(off, size int64) ([]byte, error) {
		if off == 0 {
			close(started)
			<-release
		}
		fetched.Add(int32(size))
		return bytes.Repeat([]byte{7}, int(size)), nil
	}
	first := make(chan error, 1)
	go func() { _, err := cache.ReadRange(ctx, "file", 0, 64, build); first <- err }()
	<-started
	second := make(chan error, 1)
	go func() { _, err := cache.ReadRange(ctx, "file", 32, 64, build); second <- err }()
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if fetched.Load() != 96 {
		t.Fatalf("concurrent overlap fetched %d bytes", fetched.Load())
	}
}

func TestCachedFileAnalysisCannotFetchMissingOriginBytes(t *testing.T) {
	cache := NewByteCache(128)
	ctx := context.Background()
	var fetched atomic.Int32
	build := func(off, size int64) ([]byte, error) {
		fetched.Add(int32(size))
		return bytes.Repeat([]byte{9}, int(size)), nil
	}
	if _, err := cache.ReadRange(ctx, "file", 0, 32, nil); !errors.Is(err, errRangeNotCached) {
		t.Fatalf("uncached analysis read: %v", err)
	}
	if _, err := cache.ReadRange(ctx, "file", 0, 16, build); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ReadRange(ctx, "file", 16, 16, build); err != nil {
		t.Fatal(err)
	}
	if got, err := cache.ReadRange(ctx, "file", 0, 32, nil); err != nil || len(got) != 32 {
		t.Fatalf("cached analysis read: %d bytes, %v", len(got), err)
	}
	if fetched.Load() != 32 {
		t.Fatalf("analysis downloaded extra file bytes: %d", fetched.Load())
	}
}

func TestPlaybackFileReadPassesPausedAnalysisWithoutDeadlock(t *testing.T) {
	engine := testEngine(t)
	var reads atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.Header.Get("Range") != "bytes=0-31" {
			t.Error("unexpected unbounded read")
		}
		w.Header().Set("Content-Range", "bytes 0-31/64")
		w.WriteHeader(206)
		w.Write(make([]byte, 32))
	}))
	defer origin.Close()
	asset := &Asset{ID: "file", File: &RemoteFile{Net: engine.Net, Origin: Origin{URL: origin.URL}, Size: 64}, Index: FileIndex{Duration: 60}}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), cacheOwnerKey{}, "playing"), 2*time.Second)
	defer cancel()
	background := context.WithValue(ctx, backgroundWorkKey{}, true)
	bgURL, _, doneBG, err := engine.job(background, asset, 0, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	defer doneBG()
	fgURL, _, doneFG, err := engine.job(ctx, asset, 0, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	defer doneFG()
	finish := engine.foregroundFile(asset)
	get := func(url string) error {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Header.Set("Range", "bytes=0-31")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err == nil && len(b) != 32 {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	bgDone := make(chan error, 1)
	go func() { bgDone <- get(bgURL) }()
	select {
	case err := <-bgDone:
		finish()
		t.Fatal("analysis bypassed playback priority", err)
	case <-time.After(20 * time.Millisecond):
	}
	err = get(fgURL)
	finish()
	if err != nil {
		t.Fatal("playback blocked behind analysis", err)
	}
	if err = <-bgDone; err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 1 {
		t.Fatal("same finite range was downloaded repeatedly", reads.Load())
	}
}

func TestCanceledAnalysisCacheFlightDoesNotFailPlayback(t *testing.T) {
	cache := NewByteCache(100)
	ctx, cancel := context.WithCancel(context.Background())
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		cache.Get(ctx, "header", func() ([]byte, error) { close(started); <-release; return nil, context.Canceled })
	}()
	<-started
	fgDone := make(chan error, 1)
	fgCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	go func() {
		b, err := cache.Get(fgCtx, "header", func() ([]byte, error) { return []byte("video"), nil })
		if err == nil && string(b) != "video" {
			err = io.ErrUnexpectedEOF
		}
		fgDone <- err
	}()
	cancel()
	close(release)
	<-done
	if err := <-fgDone; err != nil {
		t.Fatal("canceled analysis poisoned playback", err)
	}
}
