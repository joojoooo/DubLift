package dublift

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	u, _, cleanup, err := engine.job(ctx, asset, 0, 2)
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
	bgURL, _, doneBG, err := engine.job(background, asset, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer doneBG()
	fgURL, _, doneFG, err := engine.job(ctx, asset, 0, 2)
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
