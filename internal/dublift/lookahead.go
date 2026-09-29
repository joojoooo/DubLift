package dublift

import (
	"context"
	"sync"
	"time"
)

// Keep one producer following a moving playback window. Requests extend that
// window instead of queuing independent batches behind a slow origin read.
// A seek cancels the old producer, including its in-flight HTTP/FFmpeg work.
type segmentLookahead struct {
	mu               sync.Mutex
	ctx              context.Context
	cancel           context.CancelFunc
	wake             chan struct{}
	first, next, end int
}

func (p *segmentLookahead) schedule(parent context.Context, first, end int, build func(context.Context, int) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil || p.ctx.Err() != nil || first < p.first-1 || first > p.first+3 {
		if p.cancel != nil {
			p.cancel()
		}
		p.ctx, p.cancel = context.WithCancel(parent)
		p.wake = make(chan struct{}, 1)
		p.first, p.next, p.end = first, first, end
		go p.run(p.ctx, p.wake, build)
		return
	}
	p.first, p.end = first, end
	// Revisit the upcoming cache entries on each request: a completed prefetch
	// may have been evicted. Failed builds must also remain eligible for retry.
	p.next = first
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *segmentLookahead) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *segmentLookahead) run(ctx context.Context, wake <-chan struct{}, build func(context.Context, int) error) {
	for ctx.Err() == nil {
		p.mu.Lock()
		if p.ctx != ctx {
			p.mu.Unlock()
			return
		}
		n, end := p.next, p.end
		p.mu.Unlock()
		if n >= end {
			select {
			case <-wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		job, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := build(job, n)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Do not spin on provider failures or permanently mark them ready.
			timer := time.NewTimer(time.Second)
			select {
			case <-timer.C:
			case <-wake:
			case <-ctx.Done():
			}
			timer.Stop()
			continue
		}
		p.mu.Lock()
		if p.ctx == ctx && p.next == n {
			p.next++
		}
		p.mu.Unlock()
	}
}

func (s *Server) lookaheadEnd(v *Session, n int) int {
	seconds := 60.0
	if v.video.File != nil && v.video.File.Size > 0 {
		// Budget for both compressed source bytes and remuxed output, leaving
		// room for the player's existing buffer, audio, and container metadata.
		seconds = min(seconds, float64(int64(s.Config.Get().CacheMB)<<20)/4*v.Duration/float64(v.video.File.Size))
	}
	end := n
	for end < len(v.boundaries)-1 && v.boundaries[end] < v.boundaries[n]+seconds {
		end++
	}
	return end
}
