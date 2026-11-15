package dublift

import (
	"fmt"
	"math"
	"time"
)

// A read-only summary is separate from normal stream results. It contains no
// source connections, media URLs, resume tickets, or raw provider errors.
// Only one terminal summary is retained, for at most nextEpisodeLifetime.
type nextEpisodeDashboardStatus struct {
	parentID          string    // pending UI belongs to this selected session
	following         Content   // metadata hint; never serialized as a speculative result
	Enabled           bool      `json:"enabled"`
	LeadMinutes       int       `json:"leadMinutes"`
	State             string    `json:"state"`
	Message           string    `json:"message"`
	Trigger           string    `json:"trigger"`
	ParentContent     string    `json:"parentContent,omitempty"`
	Content           string    `json:"content,omitempty"`
	ContentName       string    `json:"contentName,omitempty"`
	SourceName        string    `json:"sourceName,omitempty"`
	StreamName        string    `json:"streamName,omitempty"`
	StreamCount       int       `json:"streamCount,omitempty"`
	PreparationStatus string    `json:"preparationStatus,omitempty"`
	SampleIndex       int       `json:"sampleIndex,omitempty"`
	SampleTotal       int       `json:"sampleTotal,omitempty"`
	SamplePhase       string    `json:"samplePhase,omitempty"`
	AudioTiming       string    `json:"audioTiming,omitempty"`
	Started           time.Time `json:"started"`
	Updated           time.Time `json:"updated"`
	Expires           time.Time `json:"expires"`
}

// Called under Server.mu. These snapshots never refresh a session's LastUsed,
// start work, consume results, or retain a discarded session's resources.
func (s *Server) nextEpisodeDashboardLocked(now time.Time) nextEpisodeDashboardStatus {
	view := s.nextEpisodeLast
	if view.State != "waiting" && !view.Updated.IsZero() && now.Sub(view.Updated) >= nextEpisodeLifetime {
		view = nextEpisodeDashboardStatus{}
	}
	if p := s.nextEpisode; p != nil {
		view = s.nextEpisodeProgressLocked(p)
	}
	view.Enabled = s.prefetchSettings.NextEpisodePrefetch
	view.LeadMinutes = s.prefetchSettings.NextEpisodeLeadMinutes
	view.Trigger = s.nextEpisodeTriggerMessageLocked()
	if view.State == "" {
		view.State = "waiting"
		if !view.Enabled {
			view.State = "disabled"
		}
		view.Message = view.Trigger
	}
	return view
}

// Handoff alone leaves the old episode selected. The first playback GET for
// that episode advances this panel, including a previously listed URL. Reuse metadata without
// starting a resolver, stream lookup, preparation, or another prefetch attempt.
func (s *Server) advanceNextEpisodeDashboardLocked(v *Session) {
	previous := s.nextEpisodeLast
	if v.Content.Type != "series" || previous.Content != v.Content.ID {
		return
	}
	view := nextEpisodeDashboardStatus{
		parentID: v.ID, State: "waiting", Message: "Waiting for the configured interval before preparing the next episode.",
		ParentContent: v.Content.ID, SourceName: nextEpisodeDashboardLabel(v.source.dashboardSource().Name), Updated: time.Now(),
	}
	if next := previous.following; next.ID != "" {
		view.Content, view.ContentName = next.ID, contentHeading(next, "Next episode")
	} else {
		view.Message = "The episode after the one now playing is not yet known; waiting for the normal preparation interval."
	}
	s.nextEpisodeLast = view
}

func (s *Server) nextEpisodeProgressLocked(p *nextEpisodePrefetch) nextEpisodeDashboardStatus {
	view := p.dashboard
	if p.expected < 0 || p.expected >= len(p.sessions) {
		return view
	}
	v := p.sessions[p.expected]
	v.mu.Lock()
	view.PreparationStatus = v.Status
	view.SampleIndex, view.SampleTotal, view.SamplePhase = v.SampleIndex, v.SampleTotal, v.SamplePhase
	if v.Aligning && view.State == "preparing" {
		view.State = "aligning"
		view.Message = "Matching English audio for the next episode."
	}
	key := v.Key
	v.mu.Unlock()
	select {
	case <-v.ready:
		if v.prepareErr != nil {
			return view
		}
		if v.native != nil {
			view.AudioTiming = "Original source audio and subtitle timing."
		} else {
			alignment := s.Alignments.Get(key)
			switch {
			case alignment.Manual != nil:
				view.AudioTiming = fmt.Sprintf("Saved manual audio delay: %+.3f seconds.", *alignment.Manual)
			case alignment.reusable(s.prefetchSettings):
				view.AudioTiming = fmt.Sprintf("Audio timing matched: %+.3f seconds · %.0f%% confidence.", alignment.Offset, alignment.Confidence*100)
			case view.State == "ready":
				view.AudioTiming = "Offset 0 fallback; reliable automatic alignment is unavailable."
			}
		}
	default:
	}
	return view
}

func (s *Server) nextEpisodeTriggerMessageLocked() string {
	cfg := s.prefetchSettings
	if !cfg.NextEpisodePrefetch {
		return "Automatic next-episode preparation is disabled in Settings."
	}
	for _, v := range s.sessions {
		v.mu.Lock()
		playing, c, group, attempted := v.Playing, v.Content, v.stream.BehaviorHints.BingeGroup, v.nextEpisodeAttempted
		position, known := v.Position, !v.PositionAt.IsZero()
		v.mu.Unlock()
		if !playing {
			continue
		}
		if c.Type != "series" {
			return "Waiting for series playback; movies have no next episode."
		}
		if group == "" {
			return "This playing stream has no binge group for next-episode matching."
		}
		if !configuredSource(cfg, v.source) {
			return "The playing stream's source is no longer enabled or available."
		}
		if attempted {
			return "One attempt per playback session; repeated media requests do not start another search."
		}
		duration := 0.0
		select {
		case <-v.ready:
			duration = v.Duration
		default:
		}
		if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
			return "Waiting for a known episode duration."
		}
		if !known || position < 0 || position >= duration || math.IsNaN(position) || math.IsInf(position, 0) {
			return "Waiting for a valid requested playback area."
		}
		return fmt.Sprintf("Starts when the last requested area reaches the final %d minutes of this episode.", cfg.NextEpisodeLeadMinutes)
	}
	return "Waiting for a series stream to be selected and played."
}

func (s *Server) nextEpisodeStage(p *nextEpisodePrefetch, state, message string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextEpisode != p || p.ctx.Err() != nil {
		return false
	}
	p.dashboard.State, p.dashboard.Message, p.dashboard.Updated = state, message, time.Now()
	return true
}

func (s *Server) rememberNextEpisodeLocked(p *nextEpisodePrefetch, state, message string) {
	view := s.nextEpisodeProgressLocked(p)
	view.State, view.Message, view.Updated = state, message, time.Now()
	s.nextEpisodeLast = view
}

func (s *Server) cancelNextEpisodeWithStatusLocked(state, message string) {
	if p := s.nextEpisode; p != nil {
		s.rememberNextEpisodeLocked(p, state, message)
	}
	s.discardNextEpisodePrefetchLocked()
}

func (s *Server) cancelNextEpisodeLocked() {
	s.cancelNextEpisodeWithStatusLocked("canceled", "Automatic preparation canceled.")
}

// Provider labels can be arbitrarily long even in a bounded listing. Keep the
// status event stream small without retaining the full stream object.
func nextEpisodeDashboardLabel(value string) string {
	count := 0
	for i := range value {
		if count == 200 {
			return value[:i] + "…"
		}
		count++
	}
	return value
}
