package dublift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	nextEpisodeLifetime           = 45 * time.Minute
	nextEpisodeStreamLimit        = 200
	nextEpisodeLookupCancellation = "Automatic preparation canceled because the player requested streams for a different episode or title."
)

// All fields except the worker's local variables are protected by Server.mu.
// Sessions stay detached until the one-use response is claimed.
type nextEpisodePrefetch struct {
	dashboard     nextEpisodeDashboardStatus
	expected      int
	priority      *speculativeWork
	parent        *Session
	source        Source
	settingsKey   string
	ctx           context.Context
	cancel        context.CancelFunc
	expires       time.Time
	lookupContent Content // first addon lookup while the expected episode is still resolving
	content       Content
	sessions      []*Session
	ready         bool
	done          chan struct{}
}

func sameSource(a, b Source) bool {
	return a.Type == b.Type && a.ManifestURL == b.ManifestURL && a.BaseURL == b.BaseURL
}
func configuredSource(cfg Settings, selected Source) bool {
	if selected.Type == "" {
		return false
	}
	for _, source := range cfg.Sources {
		if !source.Disabled && sameSource(source, selected) {
			return true
		}
	}
	return false
}
func nextEpisodeSettingsKey(cfg Settings, source Source) string {
	// Display names, ordering, public URLs and setup completion do not affect
	// prepared media. VixSrc remains an audio dependency when its videos are off.
	b, _ := json.Marshal([]any{cfg.NextEpisodePrefetch, cfg.NextEpisodeLeadMinutes,
		configuredSource(cfg, source), cfg.Source("vixsrc").BaseURL, cfg.TMDBToken,
		cfg.FFmpeg, cfg.FFprobe, cfg.MinConfidence, cfg.SearchRadius,
		cfg.AlignmentSampleSeconds, cfg.AlignmentSamples})
	return identity(string(b))
}
func (s *Server) settingsChanged(old, cfg Settings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prefetchSettings = cfg
	if reflect.DeepEqual(old, cfg) {
		return
	}
	if p := s.nextEpisode; p != nil && nextEpisodeSettingsKey(cfg, p.source) != p.settingsKey {
		message := "Automatic preparation canceled because relevant settings or sources changed."
		if !cfg.NextEpisodePrefetch {
			message = "Automatic preparation canceled because the feature was disabled."
		}
		s.cancelNextEpisodeWithStatusLocked("canceled", message)
	}
}
func (s *Server) discardNextEpisodePrefetchLocked() {
	p := s.nextEpisode
	if p == nil {
		return
	}
	s.nextEpisode = nil
	p.cancel()
	for _, v := range p.sessions {
		v.cancel()
		s.Engine.Cache.DeleteOwner(v.ID)
	}
}

// Called only after a player GET updates the existing requested-area tracker.
// Seeks back out of the interval cancel unused work, but never rearm this
// playback context. Consumption, failure and expiry also leave the latch set.
func (s *Server) maybePrefetchNextEpisode(v *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.prefetchSettings
	if s.sessions[v.ID] != v || v.ctx.Err() != nil || s.ctx.Err() != nil {
		return
	}
	v.mu.Lock()
	playing, c, position, duration, known := v.Playing, v.Content, v.Position, v.Duration, !v.PositionAt.IsZero()
	group := v.stream.BehaviorHints.BingeGroup
	v.mu.Unlock()
	eligible := cfg.NextEpisodePrefetch && playing && c.Type == "series" && group != "" && known &&
		duration > 0 && !math.IsNaN(duration) && !math.IsInf(duration, 0) &&
		position >= 0 && position < duration && !math.IsNaN(position) && !math.IsInf(position, 0) &&
		duration-position <= float64(cfg.NextEpisodeLeadMinutes)*60 && configuredSource(cfg, v.source)
	if p := s.nextEpisode; p != nil && p.parent == v && (!eligible || nextEpisodeSettingsKey(cfg, p.source) != p.settingsKey) {
		s.cancelNextEpisodeWithStatusLocked("canceled", "The requested playback area left the preparation interval.")
	}
	if !eligible || v.nextEpisodeAttempted {
		return
	}
	v.nextEpisodeAttempted = true
	s.cancelNextEpisodeLocked()
	expires := time.Now().Add(nextEpisodeLifetime)
	ctx, cancel := context.WithDeadline(s.ctx, expires)
	priority := &speculativeWork{engine: s.Engine}
	priority.active.Store(true)
	ctx = context.WithValue(ctx, speculativeWorkKey{}, priority)
	p := &nextEpisodePrefetch{expected: -1, priority: priority, parent: v, source: v.source, settingsKey: nextEpisodeSettingsKey(cfg, v.source), ctx: ctx, cancel: cancel, expires: expires, done: make(chan struct{})}
	p.dashboard = nextEpisodeDashboardStatus{State: "queued", Message: "Waiting for the automatic preparation worker.", ParentContent: c.ID, SourceName: nextEpisodeDashboardLabel(v.source.dashboardSource().Name), Started: time.Now(), Updated: time.Now(), Expires: expires}
	s.nextEpisode = p
	context.AfterFunc(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.nextEpisode == p {
			state, message := "canceled", "Automatic preparation canceled."
			if p.ctx.Err() == context.DeadlineExceeded {
				state, message = "expired", "Automatic next-episode preparation expired."
			}
			s.cancelNextEpisodeWithStatusLocked(state, message)
		}
	})
	go s.prefetchNextEpisode(p, c, group, cfg)
}

func (s *Server) prefetchNextEpisode(p *nextEpisodePrefetch, current Content, group string, cfg Settings) {
	defer close(p.done)
	// A canceled worker must release its reads/process before another begins.
	select {
	case s.nextEpisodeWorker <- struct{}{}:
		defer func() { <-s.nextEpisodeWorker }()
	case <-p.ctx.Done():
		return
	}
	succeeded := false
	failureState, failureMessage := "failed", "Automatic preparation did not finish; ordinary discovery remains available."
	defer func() {
		if !succeeded {
			s.mu.Lock()
			if s.nextEpisode == p {
				s.cancelNextEpisodeWithStatusLocked(failureState, failureMessage)
			}
			sessions := append([]*Session(nil), p.sessions...)
			s.mu.Unlock()
			// prepareSession and alignment own goroutines. Drain their cancellation
			// before releasing the single speculative-worker slot.
			for _, v := range sessions {
				v.mu.Lock()
				started := v.preparationStarted
				v.mu.Unlock()
				if !started {
					continue
				}
				<-v.ready
				v.mu.Lock()
				done := v.alignDone
				v.mu.Unlock()
				if done != nil {
					<-done
				}
			}
		}
	}()
	if !s.nextEpisodeStage(p, "resolving", "Resolving the next episode from episode metadata.") {
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, 40*time.Second)
	plan, err := s.resolveNextEpisode(ctx, current, cfg, p.source)
	if err != nil {
		failureState, failureMessage = "unavailable", "No released next episode is known, or episode metadata is unavailable."
		cancel()
		return
	}
	next := plan.Next
	s.mu.Lock()
	if s.nextEpisode == p {
		p.dashboard.following = plan.Following
		p.content = next
		p.dashboard.Content = next.ID
		p.dashboard.ContentName = contentHeading(next, "Next episode")
		if lookup := p.lookupContent; lookup.ID != "" && (lookup.Type != next.Type || lookup.ID != next.ID) {
			s.cancelNextEpisodeWithStatusLocked("canceled", nextEpisodeLookupCancellation)
		}
	}
	s.mu.Unlock()
	if !s.nextEpisodeStage(p, "searching", "Searching only the playing stream's source and checking Italian audio.") {
		cancel()
		return
	}
	// Keep the original VixSrc audio dependency; restrict only stream providers.
	restricted := cfg
	restricted.Sources = []Source{p.source}
	upstream, bundle := s.discoverStreams(ctx, next, restricted)
	cancel()
	if p.ctx.Err() != nil {
		return
	}
	if len(upstream) == 0 || len(upstream) > nextEpisodeStreamLimit {
		failureMessage = "The selected source returned no usable stream list; ordinary discovery remains available."
		return
	}
	// Stremio core next_stream_update uses iter().find(is_binge_match):
	// https://github.com/Stremio/stremio-core/blob/development/src/models/player.rs
	// Preserve result order. The first exact match is the expected stream; if
	// it is incompatible, do not silently choose a later or different group.
	expected := -1
	for i, item := range upstream {
		if item.stream.BehaviorHints.BingeGroup == group {
			expected = i
			break
		}
	}
	if expected < 0 {
		failureState, failureMessage = "unavailable", "No stream matches the current exact binge group."
		return
	}
	s.mu.Lock()
	if s.nextEpisode != p || p.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	p.content = next
	p.sessions = s.listedSessionsLocked(next, upstream, bundle, "", p.priority)
	v := p.sessions[expected]
	p.expected = expected
	p.dashboard.StreamCount, p.dashboard.StreamName = len(upstream), nextEpisodeDashboardLabel(v.stream.Name)
	p.dashboard.ContentName = nextEpisodeDashboardLabel(v.ContentName)
	p.dashboard.State, p.dashboard.Message, p.dashboard.Updated = "preparing", "Preparing the matching next-episode stream.", time.Now()
	s.mu.Unlock()
	if err := s.prepareSession(p.ctx, v); err != nil {
		failureMessage = "Playback preparation failed; ordinary discovery remains available."
		return
	}
	if err := s.awaitPreparedAlignment(p.ctx, v); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := s.prefetchSettings
	if s.nextEpisode != p || p.ctx.Err() != nil || nextEpisodeSettingsKey(latest, p.source) != p.settingsKey {
		return
	}
	p.ready = true
	p.dashboard.State, p.dashboard.Message, p.dashboard.Updated = "ready", "Prepared and waiting for the next episode's first stream request.", time.Now()
	succeeded = true
}

func (s *Server) consumeNextEpisode(c Content) []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observeStreamLookupLocked(c)
	p := s.readyNextEpisodeLocked(c)
	if p == nil {
		return nil
	}
	return s.handoffNextEpisodeLocked(p)
}

// Only addon GET /stream lookups call this path. HEAD probes and dashboard
// discovery do not express a new playback choice. Until episode metadata is
// resolved, keep one observed identity rather than guessing episode + 1; two
// different lookups cannot both be the expected next episode.
func (s *Server) observeStreamLookupLocked(c Content) {
	p := s.nextEpisode
	if p == nil {
		return
	}
	expected := p.content
	if expected.ID == "" {
		if p.lookupContent.ID == "" {
			p.lookupContent = c
			return
		}
		expected = p.lookupContent
	}
	if expected.Type != c.Type || expected.ID != c.ID {
		s.cancelNextEpisodeWithStatusLocked("canceled", nextEpisodeLookupCancellation)
	}
}

// Shared by the one-use stream response and a player selecting an older master
// URL. Neither path waits for unfinished speculative work.
func (s *Server) readyNextEpisodeLocked(c Content) *nextEpisodePrefetch {
	cfg := s.prefetchSettings
	p := s.nextEpisode
	if p == nil {
		return nil
	}
	if !time.Now().Before(p.expires) || p.ctx.Err() == context.DeadlineExceeded {
		s.cancelNextEpisodeWithStatusLocked("expired", "Automatic next-episode preparation expired.")
		return nil
	}
	if p.ctx.Err() != nil || s.sessions[p.parent.ID] != p.parent || nextEpisodeSettingsKey(cfg, p.source) != p.settingsKey {
		s.cancelNextEpisodeWithStatusLocked("canceled", "The automatic preparation context is no longer current.")
		return nil
	}
	if !p.ready || p.content.Type != c.Type || p.content.ID != c.ID {
		return nil
	}
	if len(p.sessions) == 0 {
		s.cancelNextEpisodeLocked()
		return nil
	}
	for _, v := range p.sessions {
		if v.ctx.Err() != nil {
			s.cancelNextEpisodeLocked()
			return nil
		}
	}
	return p
}

func (s *Server) handoffNextEpisodeLocked(p *nextEpisodePrefetch) []*Session {
	s.rememberNextEpisodeLocked(p, "handedOff", "Prepared results handed to the player; playback changes only when a stream is selected.")
	// Detach ownership before canceling the discovery deadline. Session contexts
	// belong to the server, so the old parent can now expire independently.
	s.nextEpisode = nil
	p.priority.active.Store(false)
	p.cancel()
	for _, v := range p.sessions {
		v.handedOff = true
		s.sessions[v.ID] = v
	}
	return p.sessions
}

type episodeMetadata struct {
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Released string `json:"released"`
}

func nextFromVideos(c Content, videos []episodeMetadata, now time.Time) (Content, error) {
	// Require the current episode in the same catalog. Sort explicit identities,
	// not array positions; specials stay within season zero rather than jumping
	// unexpectedly into/out of the regular episode run.
	sort.SliceStable(videos, func(i, j int) bool {
		if videos[i].Season != videos[j].Season {
			return videos[i].Season < videos[j].Season
		}
		return videos[i].Episode < videos[j].Episode
	})
	found := false
	for _, video := range videos {
		if video.Season == c.Season && video.Episode == c.Episode {
			found = true
			continue
		}
		if !found || video.Season < 0 || video.Episode <= 0 || (c.Season == 0 && video.Season != 0) {
			continue
		}
		if video.Season < c.Season || (video.Season == c.Season && video.Episode <= c.Episode) {
			continue
		}
		// An explicitly future or malformed release date is not an available next episode.
		if video.Released != "" {
			released, err := time.Parse(time.RFC3339, video.Released)
			if err != nil {
				released, err = time.Parse("2006-01-02", video.Released)
			}
			if err != nil || released.After(now) {
				return Content{}, errors.New("next episode has not been released")
			}
		}
		id := fmt.Sprintf("%s:%d:%d", c.BaseID, video.Season, video.Episode)
		if strings.HasPrefix(c.ID, "tmdb:") {
			id = "tmdb:" + fmt.Sprintf("%s:%d:%d", c.BaseID, video.Season, video.Episode)
		}
		// Preserve the requesting player's ID namespace even when metadata
		// was resolved through a different (validated) series identity.
		return ParseContent("series", id)
	}
	return Content{}, errors.New("no known next episode")
}

// The following identity is a dashboard hint from metadata already fetched for
// this prefetch. It never schedules discovery or assumes an episode exists.
type nextEpisodePlan struct {
	Next      Content
	Following Content
}

func nextEpisodePlanFromVideos(c Content, videos []episodeMetadata, now time.Time) (nextEpisodePlan, error) {
	next, err := nextFromVideos(c, videos, now)
	if err != nil {
		return nextEpisodePlan{}, err
	}
	following, _ := nextFromVideos(next, videos, now)
	return nextEpisodePlan{Next: next, Following: following}, nil
}

func (s *Server) resolveNextEpisode(ctx context.Context, c Content, cfg Settings, selected Source) (nextEpisodePlan, error) {
	id := c.BaseID
	if c.TMDB != "" {
		id = "tmdb:" + c.TMDB
	}
	// Prefer the selected addon, then other metadata providers. No other stream
	// endpoint is queried here. Each response is bounded by manifestLimit.
	sources := []Source{selected}
	for _, source := range cfg.Sources {
		if !sameSource(source, selected) {
			sources = append(sources, source)
		}
	}
	var origins []Origin
	for _, source := range sources {
		if source.Disabled || source.Type != "addon" {
			continue
		}
		u, err := httpURL(source.ManifestURL)
		if err != nil {
			continue
		}
		u.Path = strings.TrimSuffix(u.Path, "/manifest.json") + "/meta/series/" + id + ".json"
		u.RawPath = ""
		origins = append(origins, Origin{u.String(), http.Header{"User-Agent": {userAgent}}})
	}
	if c.TMDB == "" {
		origins = append(origins, Origin{URL: "https://v3-cinemeta.strem.io/meta/series/" + c.BaseID + ".json"})
	}
	for _, origin := range origins {
		work, cancel := context.WithTimeout(ctx, 3*time.Second)
		b, _, err := s.Net.Fetch(work, origin, manifestLimit)
		cancel()
		if err != nil {
			continue
		}
		var response struct {
			Meta struct {
				Videos []episodeMetadata `json:"videos"`
			} `json:"meta"`
		}
		if json.Unmarshal(b, &response) != nil || len(response.Meta.Videos) == 0 {
			continue
		}
		return nextEpisodePlanFromVideos(c, response.Meta.Videos, time.Now())
	}
	if cfg.TMDBToken != "" {
		tmdb, err := s.Net.ResolveTMDB(ctx, c, cfg.TMDBToken)
		if err == nil {
			return s.nextTMDBEpisode(ctx, c, tmdb, cfg.TMDBToken)
		}
	}
	// Built-in sources often start with TMDB IDs without a token. Reuse Movy's
	// validated title metadata to obtain the IMDb identity for Cinemeta.
	if c.TMDB != "" {
		base := strings.TrimRight(cfg.Source("movy").BaseURL, "/")
		page := fmt.Sprintf("%s/tv/%s/%d/%d?play=true", base, c.TMDB, c.Season, c.Episode)
		b, err := s.Net.fetchMovy(ctx, page, movyHeaders(base, base+"/"))
		if err == nil {
			details, err := parseMovyPage(b, "tv", c.TMDB)
			if err == nil && imdbRE.MatchString(details.IMDBID) {
				b, _, err = s.Net.Fetch(ctx, Origin{URL: "https://v3-cinemeta.strem.io/meta/series/" + details.IMDBID + ".json"}, manifestLimit)
				var response struct {
					Meta struct {
						Videos []episodeMetadata `json:"videos"`
					} `json:"meta"`
				}
				if err == nil && json.Unmarshal(b, &response) == nil {
					return nextEpisodePlanFromVideos(c, response.Meta.Videos, time.Now())
				}
			}
		}
	}
	return nextEpisodePlan{}, errors.New("episode metadata unavailable")
}

func (s *Server) nextTMDBEpisode(ctx context.Context, c Content, id, token string) (nextEpisodePlan, error) {
	origin := Origin{URL: "https://api.themoviedb.org/3/tv/" + id, Headers: http.Header{"Authorization": {"Bearer " + token}}}
	b, _, err := s.Net.Fetch(ctx, origin, manifestLimit)
	if err != nil {
		return nextEpisodePlan{}, err
	}
	var show struct {
		Seasons []struct {
			Number int `json:"season_number"`
		} `json:"seasons"`
	}
	if err := json.Unmarshal(b, &show); err != nil {
		return nextEpisodePlan{}, err
	}
	seasons := []int{c.Season}
	sort.Slice(show.Seasons, func(i, j int) bool { return show.Seasons[i].Number < show.Seasons[j].Number })
	for _, season := range show.Seasons {
		if season.Number > c.Season && c.Season > 0 {
			seasons = append(seasons, season.Number)
			break
		}
	}
	var videos []episodeMetadata
	for _, season := range seasons {
		origin.URL = fmt.Sprintf("https://api.themoviedb.org/3/tv/%s/season/%d", id, season)
		b, _, err := s.Net.Fetch(ctx, origin, manifestLimit)
		if err != nil {
			return nextEpisodePlan{}, err
		}
		var response struct {
			Episodes []struct {
				Season  int    `json:"season_number"`
				Episode int    `json:"episode_number"`
				AirDate string `json:"air_date"`
			} `json:"episodes"`
		}
		if err := json.Unmarshal(b, &response); err != nil {
			return nextEpisodePlan{}, err
		}
		for _, episode := range response.Episodes {
			videos = append(videos, episodeMetadata{Season: episode.Season, Episode: episode.Episode, Released: episode.AirDate})
		}
	}
	return nextEpisodePlanFromVideos(c, videos, time.Now())
}
