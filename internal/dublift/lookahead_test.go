package dublift

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestLookaheadFillsAndExtendsBoundedWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var p segmentLookahead
	built := make(chan int, 32)
	build := func(ctx context.Context, n int) error { built <- n; return nil }
	p.schedule(ctx, 1, 11, build)
	for n := 1; n < 11; n++ {
		select {
		case got := <-built:
			if got != n {
				t.Fatalf("built %d, want %d", got, n)
			}
		case <-ctx.Done():
			t.Fatal("lookahead did not fill the buffer")
		}
	}
	select {
	case n := <-built:
		t.Fatalf("downloaded beyond bounded window: %d", n)
	case <-time.After(20 * time.Millisecond):
	}
	p.schedule(ctx, 2, 12, build)
	for {
		select {
		case n := <-built:
			if n == 11 {
				return
			}
		case <-ctx.Done():
			t.Fatal("lookahead did not follow playback")
		}
	}
}

func TestLookaheadRetriesFailureAndCancelsSeek(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var p segmentLookahead
	var attempts atomic.Int32
	failed, retried, canceled, sought := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	build := func(job context.Context, n int) error {
		if n == 100 {
			close(sought)
			return nil
		}
		if attempts.Add(1) == 1 {
			close(failed)
			return errors.New("temporary origin failure")
		}
		close(retried)
		<-job.Done()
		close(canceled)
		return job.Err()
	}
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("producer failed to progress")
		}
	}
	p.schedule(ctx, 1, 2, build)
	wait(failed)
	p.schedule(ctx, 1, 2, build)
	wait(retried)
	p.schedule(ctx, 100, 101, build)
	wait(canceled)
	wait(sought)
}

func TestRequestedAreaFollowsVideoAcrossAudioRequestsAndSeeks(t *testing.T) {
	v := &Session{}
	v.audioPosition(30)
	if v.Position != 30 {
		t.Fatal("direct-video playback lost audio position tracking")
	}
	v.position(42)
	v.audioPosition(36)
	v.audioPosition(48)
	if v.Position != 42 {
		t.Fatal("audio requests moved the video position")
	}
	v.position(6)
	if v.Position != 6 {
		t.Fatal("backward video seek did not update position")
	}
}
