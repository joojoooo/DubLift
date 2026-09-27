package dublift

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

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
