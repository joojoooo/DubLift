package dublift

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) discardLocked(v *Session) {
	if s.playbackAlias == v {
		s.playbackAliasID, s.playbackAlias = "", nil
	}
	if s.nextEpisodeLast.State == "waiting" && s.nextEpisodeLast.parentID == v.ID {
		s.nextEpisodeLast = nextEpisodeDashboardStatus{}
	}
	if p := s.nextEpisode; p != nil && p.parent == v {
		s.cancelNextEpisodeWithStatusLocked("canceled", "The playing session expired or was discarded.")
	}
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
		if !playing && !v.handedOff {
			s.discardLocked(v)
		}
	}
	if len(s.sessions) == 0 {
		s.Engine.Cache.Clear()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.lookupCancel = cancel
	s.lookupStatus = "Fetching source streams and checking Italian audio…"
	return s.generation, ctx
}

// A GET of the selected master is the first signal a normal addon receives
// from a player. HEAD is a capability check and must not consume the result set.
func (s *Server) playerRequest(v *Session, r *http.Request) bool {
	_, ok := s.selectPlayerSession(v, r)
	return ok
}

func (s *Server) selectPlayerSession(v *Session, r *http.Request) (*Session, bool) {
	s.mu.Lock()
	// Another GET may already have adopted the prepared session while this
	// request held the old result pointer. Resolve the one active master alias.
	if alias := s.playbackAliasLocked(v.ID); alias != nil {
		v = alias
	}
	if r.Method == "GET" {
		v = s.adoptPreparedPlaybackLocked(v)
	}
	if s.sessions[v.ID] != v {
		s.mu.Unlock()
		return nil, false
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
		if p := s.nextEpisode; p != nil && p.content.Type == v.Content.Type && p.content.ID == v.Content.ID {
			// This episode started through a previously listed URL or a different
			// stream. Discard unused speculative work, then show the following episode.
			s.rememberNextEpisodeLocked(p, "selected", "The episode started; unused automatic preparation was discarded.")
			s.discardNextEpisodePrefetchLocked()
		} else {
			s.cancelNextEpisodeWithStatusLocked("canceled", "Automatic preparation canceled because playback switched to a different episode or title.")
		}
		v.handedOff = false
		s.advanceNextEpisodeDashboardLocked(v)
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
	return v, true
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
	if s.nextEpisodeLast.State != "waiting" && !s.nextEpisodeLast.Updated.IsZero() && now.Sub(s.nextEpisodeLast.Updated) >= nextEpisodeLifetime {
		s.nextEpisodeLast = nextEpisodeDashboardStatus{}
	}
	if p := s.nextEpisode; p != nil && !now.Before(p.expires) {
		s.cancelNextEpisodeWithStatusLocked("expired", "Automatic next-episode preparation expired.")
	}
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
	// Unused prefetch sessions stay detached from stream results, but still
	// own their saved timing until handoff, cancellation or expiry.
	if p := s.nextEpisode; p != nil && p.ctx.Err() == nil {
		for _, v := range p.sessions {
			v.mu.Lock()
			active[v.Key] = true
			v.mu.Unlock()
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
