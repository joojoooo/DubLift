package dublift

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

type windowClock struct {
	mu sync.Mutex
	packetClock
}

func (c *windowClock) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.packetClock.Write(b)
}

// One sequential FFmpeg extraction publishes each complete HLS segment as it
// arrives. The output and origin byte budgets still bound the entire window.
type videoWindowWriter struct {
	raw                    []byte
	parsed, complete, next int
	bounds                 []float64
	duration               float64
	sequence               uint32
	unmarked               bool
	fragments              int
	clock                  *windowClock
	publish                func(int, []byte) error
}

func (w *videoWindowWriter) Write(b []byte) (int, error) {
	if len(w.raw)+len(b) > segmentLimit {
		return 0, errors.New("video window exceeds byte budget")
	}
	w.raw = append(w.raw, b...)
	for w.parsed+8 <= len(w.raw) {
		box := w.raw[w.parsed:]
		size := uint64(binary.BigEndian.Uint32(box))
		head := uint64(8)
		if size == 1 {
			if len(box) < 16 {
				break
			}
			size, head = binary.BigEndian.Uint64(box[8:]), 16
		}
		if size < head || size > segmentLimit {
			return 0, errors.New("invalid streaming MP4 box")
		}
		if size > uint64(len(box)) {
			break
		}
		w.parsed += int(size)
		if string(box[4:8]) == "mdat" {
			w.complete = w.parsed
			w.fragments++
			// A file with missing key flags emits one fragment per frame. Check
			// in small batches instead of reparsing the whole window per frame.
			if w.unmarked && w.fragments%16 != 0 {
				continue
			}
			if err := w.flush(false); err != nil {
				return 0, err
			}
		}
	}
	return len(b), nil
}

func (w *videoWindowWriter) flush(final bool) error {
	w.clock.mu.Lock()
	first, known := w.clock.first, w.clock.known
	w.clock.mu.Unlock()
	if !known {
		if final {
			return errNoVideoPacketTimestamps
		}
		return nil
	}
	for w.next < len(w.bounds)-1 {
		start, end := w.bounds[w.next], w.bounds[w.next+1]
		completeEnd := !final || end < w.duration-.01
		data, err := placeFragmentsMode(w.raw[:w.complete], w.bounds[0]+first+1, start+1, end+1, completeEnd, w.sequence+uint32(w.next), w.unmarked)
		if err != nil {
			if final {
				return err
			}
			return nil // Wait for the remainder of this segment's GOPs.
		}
		if err = w.publish(w.next, data); err != nil {
			return err
		}
		w.next++
	}
	return nil
}

func (e *Engine) VideoWindow(ctx context.Context, a *Asset, bounds []float64, publish func(int, []byte) error) error {
	finish := e.foregroundFile(a)
	defer finish()
	for attempt := 0; attempt < 2; attempt++ {
		err := e.videoWindowAttempt(ctx, a, bounds, publish)
		if errors.Is(err, errNoVideoPacketTimestamps) && attempt == 0 && ctx.Err() == nil {
			a.unmarkedVideo.Store(true)
			continue
		}
		return err
	}
	return errNoVideoPacketTimestamps
}

func (e *Engine) videoWindowAttempt(ctx context.Context, a *Asset, bounds []float64, publish func(int, []byte) error) error {
	start, end := bounds[0], bounds[len(bounds)-1]
	inputFailure := &mediaInputFailure{video: true}
	ctx = context.WithValue(ctx, mediaInputFailureKey{}, inputFailure)
	u, skip, cleanup, err := e.job(ctx, a, start, end-start+1, true)
	if err != nil {
		return err
	}
	defer cleanup()
	unmarked := a.unmarkedVideo.Load()
	w := &videoWindowWriter{bounds: bounds, duration: a.Duration(), sequence: videoSequence(a, start), unmarked: unmarked, clock: &windowClock{}, publish: publish}
	err = e.runOutput(ctx, e.Config.Get().FFmpeg, videoArgsMode(u, skip, start, end-start, unmarked), w, w.clock)
	if originErr, metadata := inputFailure.failure(); originErr != nil {
		// Missing key flags can exhaust the read budget before any packet is
		// emitted. Preserve the -copyinkf retry before reporting that failure.
		if err == nil && !unmarked && !w.clock.known {
			return w.flush(true)
		}
		// FFmpeg can recover from unavailable seek metadata by reading media
		// directly. Let a successful remux validate and flush its output, but
		// keep media read failures fatal even for the shorter final video GOP.
		if !metadata || err != nil || !w.clock.known {
			return fmt.Errorf("upstream video download failed: %w", originErr)
		}
	}
	if err != nil {
		return err
	}
	return w.flush(true)
}

type fileVideoWindow struct {
	first, end int
	ctx        context.Context
	cancel     context.CancelFunc
	changed    chan struct{}
	done       bool
	err        error
	superseded bool
	// The last published segment remains available to waiters even if the
	// configured byte cache is smaller than one unusually large segment.
	last int
	data []byte
}

func videoCacheKey(v *Session, n int) string { return fmt.Sprintf("video:%s:%d", v.video.ID, n) }

func (s *Server) fileVideo(ctx context.Context, v *Session, n int) ([]byte, error) {
	var waiting *fileVideoWindow
	for attempt := 0; attempt < 2; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if data, ok := s.Engine.Cache.Lookup(ctx, videoCacheKey(v, n)); ok {
			return data, nil
		}
		v.mu.Lock()
		// A segment may have been published between the first cache check
		// and acquiring the producer lock.
		if data, ok := s.Engine.Cache.Lookup(ctx, videoCacheKey(v, n)); ok {
			v.mu.Unlock()
			return data, nil
		}
		if waiting != nil && waiting.superseded {
			if waiting.last == n {
				data := waiting.data
				v.mu.Unlock()
				return data, nil
			}
			v.mu.Unlock()
			return nil, context.Canceled
		}
		window := v.videoWindow
		if window != nil && window.first <= n && n < window.end && window.last == n {
			data := window.data
			v.mu.Unlock()
			return data, nil
		}
		if window == nil || window.done || window.ctx.Err() != nil || n < window.first || n >= window.end {
			if window != nil && !window.done {
				// The next boundary (as well as a seek) needs a new extraction.
				// Cancel the old producer before starting it, but retain its
				// already published segments in the cache.
				window.superseded = true
				window.cancel()
			}
			// A window covers up to 30 seconds / approximately 24 MiB of
			// source media. Publish its first segment without waiting for its end.
			seconds := min(30.0, float64(24<<20)*v.Duration/float64(v.video.File.Size))
			end := n + 1
			for end < len(v.boundaries)-1 && v.boundaries[end+1]-v.boundaries[n] <= seconds {
				end++
			}
			limit := s.lookaheadEnd(v, min(v.videoHighest+1, len(v.boundaries)-2))
			end = max(n+1, min(end, limit))
			windowCtx, cancel := context.WithTimeout(context.WithValue(v.ctx, videoBytesKey{}, &v.videoDownload), 60*time.Second)
			window = &fileVideoWindow{first: n, end: end, ctx: windowCtx, cancel: cancel, changed: make(chan struct{}), last: -1}
			v.videoWindow = window
			go s.fillVideoWindow(v, window)
		}
		changed := window.changed
		waiting = window
		v.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
		v.mu.Lock()
		done, err, superseded := window.done, window.err, window.superseded
		last, lastData := window.last, window.data
		v.mu.Unlock()
		if last == n {
			return lastData, nil
		}
		if superseded {
			if data, ok := s.Engine.Cache.Lookup(ctx, videoCacheKey(v, n)); ok {
				return data, nil
			}
			return nil, context.Canceled
		}
		if window.ctx.Err() != nil && !done {
			// A canceled or expired producer cannot publish this segment.
			if data, ok := s.Engine.Cache.Lookup(ctx, videoCacheKey(v, n)); ok {
				return data, nil
			}
			return nil, window.ctx.Err()
		}
		if done && err != nil {
			if errors.Is(err, context.Canceled) {
				if data, ok := s.Engine.Cache.Lookup(ctx, videoCacheKey(v, n)); ok {
					return data, nil
				}
				return nil, err
			}
			attempt++
			if attempt == 2 {
				return nil, err
			}
		}
	}
	return nil, errors.New("video window could not be prepared")
}

func (s *Server) fillVideoWindow(v *Session, window *fileVideoWindow) {
	defer window.cancel()
	err := s.Engine.VideoWindow(window.ctx, v.video, v.boundaries[window.first:window.end+1], func(i int, data []byte) error {
		if err := window.ctx.Err(); err != nil {
			return err
		}
		n := window.first + i
		s.Engine.Cache.Put(window.ctx, videoCacheKey(v, n), data)
		v.mu.Lock()
		window.last, window.data = n, data
		close(window.changed)
		window.changed = make(chan struct{})
		v.mu.Unlock()
		return nil
	})
	v.mu.Lock()
	window.done, window.err = true, err
	close(window.changed)
	window.changed = make(chan struct{})
	v.mu.Unlock()
}
