package dublift

import (
	"context"
	"sync/atomic"
)

type speculativeWorkKey struct{}
type speculativeWork struct {
	engine *Engine
	active atomic.Bool
}

func isSpeculativeWork(ctx context.Context) bool {
	work, _ := ctx.Value(speculativeWorkKey{}).(*speculativeWork)
	return work != nil && work.active.Load()
}

type speculativeStageKey struct{}

// One background subprocess leaves two engine slots for foreground media.
// Pause new speculative reads/stages while player requests are outstanding.
// A stage already underway is bounded and may finish; the next stage yields.
func (e *Engine) foregroundRequest() func() {
	e.mu.Lock()
	if e.foregroundRequests == 0 {
		e.foregroundIdle = make(chan struct{})
	}
	e.foregroundRequests++
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.foregroundRequests--
		if e.foregroundRequests == 0 {
			close(e.foregroundIdle)
		}
	}
}
func waitSpeculativeWork(ctx context.Context) error {
	work, _ := ctx.Value(speculativeWorkKey{}).(*speculativeWork)
	if work == nil || !work.active.Load() || ctx.Value(speculativeStageKey{}) == true {
		return ctx.Err()
	}
	e := work.engine
	for {
		e.mu.Lock()
		active, idle := e.foregroundRequests, e.foregroundIdle
		e.mu.Unlock()
		if active == 0 {
			return ctx.Err()
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
