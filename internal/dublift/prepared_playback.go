package dublift

import "reflect"

// A player may fetch next-episode streams before automatic preparation starts.
// Its cached master URL can serve the warmed session's playlist (whose media
// URLs carry the warmed ID). Keep one master-only alias for repeated GET/HEAD
// requests, and release it with the selected session. This is not a stream-list
// cache: every later listing still performs ordinary discovery.
func (s *Server) playbackAliasLocked(id string) *Session {
	v := s.playbackAlias
	if v != nil && s.playbackAliasID == id && s.sessions[v.ID] == v && v.ctx.Err() == nil {
		return v
	}
	return nil
}

func (s *Server) mediaSession(id string, master bool) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if master {
		if v := s.playbackAliasLocked(id); v != nil {
			return v
		}
	}
	return s.sessions[id]
}

func samePreparedPlayback(a, b *Session) bool {
	if a == b {
		return true
	}
	if a.Content.Type != b.Content.Type || a.Content.ID != b.Content.ID ||
		!sameSource(a.source, b.source) || a.lookupKey != b.lookupKey ||
		a.stream.BehaviorHints.BingeGroup != b.stream.BehaviorHints.BingeGroup ||
		a.stream.BehaviorHints.Filename != b.stream.BehaviorHints.Filename ||
		!reflect.DeepEqual(a.stream.Subtitles, b.stream.Subtitles) {
		return false
	}
	if a.native == nil || b.native == nil {
		return a.native == nil && b.native == nil
	}
	return a.native.Quality == b.native.Quality && a.native.Italian == b.native.Italian
}

// Server.mu is held. Exact source/media identity is required; a matching
// episode or binge group alone must never substitute the selected video.
func (s *Server) adoptPreparedPlaybackLocked(requested *Session) *Session {
	var prepared *Session
	if p := s.readyNextEpisodeLocked(requested.Content); p != nil && p.expected >= 0 && p.expected < len(p.sessions) {
		expected := p.sessions[p.expected]
		if samePreparedPlayback(requested, expected) {
			s.handoffNextEpisodeLocked(p)
			prepared = expected
		}
	}
	if prepared == nil {
		// Another listing may already have consumed the response, while the player
		// still opens its earlier URL. Only ready transferred sessions are eligible.
		for _, candidate := range s.sessions {
			if !candidate.handedOff || candidate.ctx.Err() != nil || !samePreparedPlayback(requested, candidate) {
				continue
			}
			select {
			case <-candidate.ready:
				if candidate.prepareErr == nil && (prepared == nil || candidate.Order < prepared.Order || candidate.Order == prepared.Order && candidate.ID < prepared.ID) {
					prepared = candidate
				}
			default:
			}
		}
	}
	if prepared == nil || prepared == requested {
		return requested
	}
	s.playbackAliasID, s.playbackAlias = requested.ID, prepared
	return prepared
}
