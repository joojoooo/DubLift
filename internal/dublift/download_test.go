package dublift

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestVideoDownloadCountsBytesBeforeRangeCompletes(t *testing.T) {
	firstHalf := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-7" {
			t.Errorf("unexpected range: %q", r.Header.Get("Range"))
			return
		}
		w.Header().Set("Content-Range", "bytes 0-7/8")
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("abcd"))
		w.(http.Flusher).Flush()
		select {
		case <-firstHalf:
		default:
			close(firstHalf)
		}
		<-release
		w.Write([]byte("efgh"))
	}))
	defer origin.Close()

	net := NewNetwork()
	file := &RemoteFile{Net: net, Origin: Origin{URL: origin.URL}, Size: 8}
	var video videoDownload
	ctx := context.WithValue(context.Background(), videoBytesKey{}, &video)
	finished := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		_, err := file.ReadAtContext(ctx, buf, 0)
		finished <- err
	}()
	select {
	case <-firstHalf:
	case <-time.After(time.Second):
		t.Fatal("first video bytes never arrived")
	}
	deadline := time.After(time.Second)
	for video.bytes.Load() != 4 {
		select {
		case err := <-finished:
			t.Fatalf("range finished before second half was released: %v", err)
		case <-deadline:
			t.Fatalf("video count did not update during range read: %d", video.bytes.Load())
		case <-time.After(time.Millisecond):
		}
	}
	if video.active.Load() != 1 {
		t.Fatalf("in-progress video reads = %d, want 1", video.active.Load())
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if got := video.bytes.Load(); got != 8 {
		t.Fatalf("complete range count = %d, want 8", got)
	}
	if video.active.Load() != 0 {
		t.Fatalf("completed video left %d active reads", video.active.Load())
	}
	buf := make([]byte, 8)
	if _, err := file.ReadAtContext(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if got := video.bytes.Load(); got != 8 {
		t.Fatalf("unmarked read changed video count to %d", got)
	}
}
