package dublift

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) discardLocked(v *Session) {
	v.cancel()
	delete(s.sessions, v.ID)
	s.Engine.Cache.DeleteOwner(v.ID)
}

func (s *Server) beginLookup() (uint64, context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lookupCancel != nil {
		s.lookupCancel()
	}
	s.generation++
	for _, v := range s.sessions {
		v.mu.Lock()
		playing := v.Playing
		v.mu.Unlock()
		// Clients can refresh results or prefetch the next episode while a
		// stream is playing. Only a new playback selection replaces that stream.
		if !playing {
			s.discardLocked(v)
		}
	}
	if len(s.sessions) == 0 {
		s.Engine.Cache.Clear()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.lookupCancel = cancel
	s.lookupStatus = "Fetching upstream streams and checking Italian audio…"
	return s.generation, ctx
}

// A GET of the selected master is the first signal a normal addon receives
// from a player. HEAD is a capability check and must not consume the result set.
func (s *Server) playerRequest(v *Session, r *http.Request) bool {
	s.mu.Lock()
	if s.sessions[v.ID] != v {
		s.mu.Unlock()
		return false
	}
	v.mu.Lock()
	v.LastUsed, v.RequestAt, v.RequestMethod = time.Now(), time.Now(), r.Method
	first := !v.Playing && r.Method == "GET"
	if first {
		v.Playing = true
		v.PlaybackAt = time.Now()
		v.Status = "Playback detected · preparing selected stream"
	} else if !v.Playing {
		v.Status = "Player checked this stream · waiting for playback"
	}
	v.mu.Unlock()
	if first {
		// Invalidate unfinished lookup workers as well as already listed results.
		s.generation++
		if s.lookupCancel != nil {
			s.lookupCancel()
		}
		for _, other := range s.sessions {
			if other != v {
				s.discardLocked(other)
			}
		}
		s.lookupStatus = "Player selected a stream; other results discarded."
	}
	s.mu.Unlock()
	if first {
		s.event("Playback detected: " + v.Name)
	}
	return true
}

func (s *Server) collectLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.collectExpired(now)
		}
	}
}
func (s *Server) collectExpired(now time.Time) {
	active, removed := map[string]bool{}, map[string]bool{}
	s.mu.Lock()
	for _, v := range s.sessions {
		v.mu.Lock()
		idle, key := now.Sub(v.LastUsed), v.Key
		v.mu.Unlock()
		if idle >= time.Hour {
			s.discardLocked(v)
			removed[key] = true
		} else {
			active[key] = true
		}
	}
	if len(s.sessions) == 0 {
		s.Engine.Cache.Clear()
	}
	s.mu.Unlock()
	if err := s.Alignments.Prune(now, active, removed); err != nil {
		s.event("Cache cleanup: " + err.Error())
	}
}

func sessionRequestContext(parent context.Context, v *Session) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithValue(parent, cacheOwnerKey{}, v.ID))
	stop := context.AfterFunc(v.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}
