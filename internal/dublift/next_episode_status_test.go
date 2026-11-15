package dublift

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func automaticStatus(t *testing.T, s *Server) nextEpisodeDashboardStatus {
	t.Helper()
	w := nextRequest(t, s, "GET", "/api/status", "")
	var response struct {
		Automatic nextEpisodeDashboardStatus `json:"automaticNextEpisode"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Automatic
}

func TestNextEpisodeDashboardWaitingAndDisabled(t *testing.T) {
	s := lifecycleServer(t)
	view := automaticStatus(t, s)
	if !view.Enabled || view.State != "waiting" || view.LeadMinutes != 5 || !strings.Contains(view.Message, "series stream") {
		t.Fatalf("missing initial automatic status: %+v", view)
	}
	cfg := s.Config.Get()
	cfg.NextEpisodePrefetch = false
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	view = automaticStatus(t, s)
	if view.Enabled || view.State != "disabled" || !strings.Contains(view.Message, "disabled") {
		t.Fatal(view)
	}
	f := newNextFixture(t)
	close(f.parent.ready)
	f.parent.position(0)
	used := f.parent.LastUsed
	for range 5 {
		view = automaticStatus(t, f.s)
		if view.State != "waiting" || !strings.Contains(view.Trigger, "final 5 minutes") {
			t.Fatal(view)
		}
	}
	if f.parent.LastUsed != used || f.parent.nextEpisodeAttempted || f.s.nextEpisode != nil || f.selected.Load() != 0 {
		t.Fatal("dashboard reads triggered work or extended playback")
	}
}

func TestNextEpisodeDashboardSearchPreparationAndHandoff(t *testing.T) {
	f := newNextFixture(t)
	f.block = make(chan struct{})
	p := f.trigger(t, 300)
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("search did not start")
	}
	view := automaticStatus(t, f.s)
	if view.State != "searching" || view.Content != "tmdb:100:1:2" || view.ParentContent != "tmdb:100:1:1" || view.SourceName != "same display name" {
		t.Fatalf("search invisible: %+v", view)
	}
	if len(f.s.status(httptest.NewRequest("GET", "/api/status", nil))["sessions"].([]map[string]any)) != 1 {
		t.Fatal("automatic results mixed with normal results")
	}
	close(f.block)
	waitNext(t, p)
	view = automaticStatus(t, f.s)
	if view.State != "ready" || view.StreamName != "expected" || view.StreamCount != 3 || !strings.Contains(view.AudioTiming, "Offset 0 fallback") {
		t.Fatalf("prepared result invisible: %+v", view)
	}
	warmed := p.sessions[1]
	used, warmedUsed := f.parent.LastUsed, warmed.LastUsed
	// Snapshot a running alignment without exposing or selecting its detached session.
	f.s.mu.Lock()
	p.dashboard.State = "preparing"
	f.s.mu.Unlock()
	warmed.mu.Lock()
	warmed.Aligning = true
	warmed.SampleIndex = 2
	warmed.SampleTotal = 3
	warmed.SamplePhase = "Downloading and decoding Vixsrc English audio"
	warmed.mu.Unlock()
	view = automaticStatus(t, f.s)
	if view.State != "aligning" || view.SampleIndex != 2 || view.SampleTotal != 3 || !strings.Contains(view.SamplePhase, "Vixsrc English") {
		t.Fatalf("alignment progress invisible: %+v", view)
	}
	warmed.mu.Lock()
	warmed.Aligning = false
	warmed.mu.Unlock()
	f.s.mu.Lock()
	p.dashboard.State = "ready"
	f.s.mu.Unlock()
	for range 5 {
		automaticStatus(t, f.s)
	}
	if f.parent.LastUsed != used || warmed.LastUsed != warmedUsed || warmed.Playing || f.s.nextEpisode != p {
		t.Fatal("automatic status changed playback ownership or lifetime")
	}
	encoded, _ := json.Marshal(view)
	for _, private := range []string{f.origin.URL, "/source.m3u8", exactNextGroup, "resume=", "manifest.json"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("automatic summary exposes private media/source data: %s", encoded)
		}
	}
	next, _ := ParseContent("series", "tmdb:100:1:2")
	if f.s.consumeNextEpisode(next) == nil {
		t.Fatal("status consumed prepared results")
	}
	view = automaticStatus(t, f.s)
	if view.State != "handedOff" || !strings.Contains(view.Message, "handed to the player") || f.s.getSession(warmed.ID) != warmed || !f.parent.Playing || warmed.Playing {
		t.Fatalf("handoff status lost or selected playback: %+v", view)
	}
}

func TestNextEpisodeDashboardTerminalOutcomes(t *testing.T) {
	for _, outcome := range []string{"unavailable", "failed", "canceled", "disabled", "expired"} {
		t.Run(outcome, func(t *testing.T) {
			f := newNextFixture(t)
			if outcome == "unavailable" {
				f.videos = f.videos[:1]
			}
			if outcome == "failed" {
				f.streams = nil
			}
			p := f.trigger(t, 300)
			waitNext(t, p)
			switch outcome {
			case "canceled":
				f.trigger(t, 0)
			case "disabled":
				cfg := f.s.Config.Get()
				cfg.NextEpisodePrefetch = false
				f.s.Config.Save(cfg)
			case "expired":
				f.s.mu.Lock()
				p.expires = time.Now().Add(-time.Second)
				f.s.mu.Unlock()
				next, _ := ParseContent("series", "tmdb:100:1:2")
				f.s.consumeNextEpisode(next)
			}
			want := outcome
			if outcome == "disabled" {
				want = "canceled"
			}
			view := automaticStatus(t, f.s)
			if view.State != want || view.Message == "" || view.SourceName == "" || view.Started.IsZero() || view.Updated.IsZero() || f.s.nextEpisode != nil {
				t.Fatalf("outcome disappeared: %+v", view)
			}
			if outcome == "disabled" && (view.Enabled || !strings.Contains(view.Message, "disabled")) {
				t.Fatal(view)
			}
			// An obsolete worker cannot replace the final outcome, and polling does
			// not extend its finite summary retention.
			if f.s.nextEpisodeStage(p, "searching", "stale worker") {
				t.Fatal("stale worker published status")
			}
			again := automaticStatus(t, f.s)
			if again.State != view.State || !again.Updated.Equal(view.Updated) {
				t.Fatal("polling or stale worker changed terminal summary")
			}
			f.s.mu.Lock()
			expired := f.s.nextEpisodeDashboardLocked(view.Updated.Add(nextEpisodeLifetime + time.Second))
			f.s.mu.Unlock()
			if expired.State == want && want != "disabled" {
				t.Fatalf("terminal summary never expired: %+v", expired)
			}
		})
	}
}

func TestNextEpisodeDashboardEventsIncludeAutomaticSearch(t *testing.T) {
	f := newNextFixture(t)
	f.block = make(chan struct{})
	local := httptest.NewServer(f.s)
	defer local.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", local.URL+"/api/events", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	read := func() nextEpisodeDashboardStatus {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var status struct {
				Automatic nextEpisodeDashboardStatus `json:"automaticNextEpisode"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &status); err != nil {
				t.Fatal(err)
			}
			return status.Automatic
		}
	}
	if view := read(); view.State != "waiting" {
		t.Fatal(view)
	}
	p := f.trigger(t, 300)
	if view := read(); view.State != "searching" || view.Content != "tmdb:100:1:2" {
		t.Fatal("automatic progress missing from event stream", view)
	}
	close(f.block)
	waitNext(t, p)
	if view := read(); view.State != "ready" {
		t.Fatal("completion missing from event stream", view)
	}
}

func TestNextEpisodeDashboardAdvancesOnlyWhenHandedOffEpisodePlays(t *testing.T) {
	for _, tc := range []struct {
		name      string
		videos    []episodeMetadata
		following string
	}{
		{"same season", []episodeMetadata{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3}}, "tmdb:100:1:3"},
		{"season transition", []episodeMetadata{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 2, Episode: 1}}, "tmdb:100:2:1"},
		{"no known following episode", []episodeMetadata{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}, ""},
		{"unreleased following episode", []episodeMetadata{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3, Released: "2099-01-01"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNextFixture(t)
			f.videos = tc.videos
			p := f.trigger(t, 300)
			waitNext(t, p)
			warmed := p.sessions[1]
			var requests atomic.Int64
			transport := f.s.Net.Client.Transport
			f.s.Net.Client.Transport = movyRoundTripFunc(func(r *http.Request) (*http.Response, error) { requests.Add(1); return transport.RoundTrip(r) })
			// Give the synthetic next episode a normal series runtime so subsequent
			// requested-area tracking can check its unchanged five-minute trigger.
			warmed.Duration = 600
			response := nextRequest(t, f.s, "GET", "http://player.test/stream/series/tmdb:100:1:2.json", "")
			var listing struct {
				Streams []Stream `json:"streams"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
				t.Fatal(err)
			}
			if len(listing.Streams) != 3 {
				t.Fatal(response.Body.String())
			}
			before := f.selected.Load()
			view := automaticStatus(t, f.s)
			if view.State != "handedOff" || view.Content != "tmdb:100:1:2" {
				t.Fatal("handoff advanced before playback", view)
			}
			head := nextRequest(t, f.s, "HEAD", listing.Streams[1].URL, "")
			view = automaticStatus(t, f.s)
			if head.Code != 200 || warmed.Playing || view.State != "handedOff" || view.Content != "tmdb:100:1:2" {
				t.Fatal("HEAD advanced the panel", view)
			}
			playback := nextRequest(t, f.s, "GET", listing.Streams[1].URL, "")
			view = automaticStatus(t, f.s)
			if playback.Code != 200 || !warmed.Playing || view.State != "waiting" || view.ParentContent != warmed.Content.ID || view.Content != tc.following {
				t.Fatalf("panel did not advance with selected playback: %+v; %d", view, playback.Code)
			}
			if view.Content == warmed.Content.ID || view.StreamName != "" || view.StreamCount != 0 || view.PreparationStatus != "" || view.SamplePhase != "" || view.AudioTiming != "" || !view.Expires.IsZero() {
				t.Fatal("previous episode's preparation remained visible", view)
			}
			if tc.following == "" && !strings.Contains(view.Message, "not yet known") {
				t.Fatal("unknown following episode was guessed", view)
			}
			used := warmed.LastUsed
			for range 5 {
				automaticStatus(t, f.s)
			}
			if warmed.LastUsed != used {
				t.Fatal("polling extended active playback")
			}
			nextRequest(t, f.s, "GET", listing.Streams[1].URL, "")
			if f.selected.Load() != before || f.unrelated.Load() != 0 || f.s.nextEpisode != nil || warmed.nextEpisodeAttempted || f.s.getSession(warmed.ID) != warmed || requests.Load() != 0 {
				t.Fatal("UI rollover started discovery or preparation")
			}
			// Read-only waiting status stays tied to the selected session, including
			// an episode longer than the terminal-outcome retention period.
			f.s.mu.Lock()
			later := f.s.nextEpisodeDashboardLocked(view.Updated.Add(nextEpisodeLifetime + time.Second))
			f.s.mu.Unlock()
			if later.Content != tc.following || later.ParentContent != warmed.Content.ID || later.State != "waiting" {
				t.Fatal("waiting target expired during playback", later)
			}
			warmed.position(100)
			f.s.maybePrefetchNextEpisode(warmed)
			if f.s.nextEpisode != nil || warmed.nextEpisodeAttempted || f.selected.Load() != before {
				t.Fatal("following prefetch started before normal threshold")
			}
			if tc.following != "" {
				warmed.position(300)
				f.s.maybePrefetchNextEpisode(warmed)
				f.s.mu.Lock()
				following := f.s.nextEpisode
				f.s.mu.Unlock()
				waitNext(t, following)
				if !following.ready || following.content.ID != tc.following || f.selected.Load() != before+1 {
					t.Fatal("normal threshold no longer prepares following episode")
				}
			} else {
				f.s.mu.Lock()
				f.s.discardLocked(warmed)
				f.s.mu.Unlock()
				if view := automaticStatus(t, f.s); view.ParentContent != "" || view.Content != "" {
					t.Fatal("discarded playback left a stale pending target", view)
				}
			}
		})
	}
}
