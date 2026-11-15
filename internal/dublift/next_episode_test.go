package dublift

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const exactNextGroup = "  Addon.Mixed/4K:v1 · group  "

type nextFixture struct {
	s                   *Server
	origin              *httptest.Server
	parent              *Session
	selected, unrelated atomic.Int64
	streams             []json.RawMessage
	videos              []episodeMetadata
	block               chan struct{}
	entered             chan struct{}
	canceled            chan struct{}
}

func newNextFixture(t *testing.T) *nextFixture {
	t.Helper()
	f := &nextFixture{entered: make(chan struct{}, 1), canceled: make(chan struct{}, 1)}
	f.videos = []episodeMetadata{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 2, Episode: 1}}
	f.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/selected/stream/"):
			f.selected.Add(1)
			if f.block != nil {
				select {
				case f.entered <- struct{}{}:
				default:
				}
				select {
				case <-f.block:
				case <-r.Context().Done():
					select {
					case f.canceled <- struct{}{}:
					default:
					}
					return
				}
			}
			jsonResponse(w, 200, map[string]any{"streams": f.streams})
		case strings.HasPrefix(r.URL.Path, "/unrelated/stream/"):
			f.unrelated.Add(1)
			io.WriteString(w, `{"streams":[{"name":"unrelated","externalUrl":"https://player.test/","unknown":42}]}`)
		case strings.Contains(r.URL.Path, "/meta/series/"):
			jsonResponse(w, 200, map[string]any{"meta": map[string]any{"name": "Fixture", "tmdb_id": "100", "videos": f.videos}})
		case strings.HasPrefix(r.URL.Path, "/api/tv/"):
			io.WriteString(w, `{"src":"/embed"}`)
		case r.URL.Path == "/embed":
			io.WriteString(w, `window.masterPlaylist={url:'/vix',params:{token:'fixture',expires:9999999999}}`)
		case r.URL.Path == "/vix.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Italian\",LANGUAGE=\"it\",URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO=\"a\"\nsource.m3u8\n")
		case r.URL.Path == "/source.m3u8", r.URL.Path == "/audio.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
		case strings.HasPrefix(r.URL.Path, "/tv/100/"):
			io.WriteString(w, movyPage("100", "tv", "Fixture"))
		case r.URL.Path == "/_next/static/chunks/player.js":
			io.WriteString(w, `/seattle/sources`)
		case r.URL.Path == "/movy-api/seed":
			io.WriteString(w, `{"seed":"fixture-seed"}`)
		case r.URL.Path == "/movy-api/seattle/sources":
			io.WriteString(w, movyEnvelope(t, movyPayload{Sources: []movySource{{URL: f.origin.URL + "/source.m3u8", Quality: "1080p", Type: "hls"}}}, "fixture-seed", 100))
		case r.URL.Path == "/segment.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(make([]byte, 512))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.origin.Close)
	f.s = lifecycleServer(t)
	cfg := f.s.Config.Get()
	// Protocol tests isolate the probe; a separate integration test below uses
	// real synthetic media and FFmpeg to verify matching and prepared reuse.
	cfg.FFmpeg, _ = os.Executable()
	cfg.FFprobe = filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(cfg.FFprobe, []byte("#!/bin/sh\nprintf '%s' '{\"streams\":[{\"codec_type\":\"video\",\"codec_name\":\"h264\"}],\"format\":{\"start_time\":\"0\"}}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Sources = []Source{
		{Type: "addon", Name: "same display name", ManifestURL: f.origin.URL + "/selected/manifest.json"},
		{Type: "addon", Name: "same display name", ManifestURL: f.origin.URL + "/unrelated/manifest.json"},
		{Type: "vixsrc", BaseURL: f.origin.URL, Disabled: true},
		{Type: "movy", BaseURL: f.origin.URL, Disabled: true},
	}
	if err := f.s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	f.streams = []json.RawMessage{
		json.RawMessage(`{"name":"Fallback","infoHash":"abcdef","custom":{"keep":true},"behaviorHints":{"bingeGroup":"other"}}`),
		json.RawMessage(fmt.Sprintf(`{"name":"expected","url":%q,"custom":{"keep":17},"behaviorHints":{"bingeGroup":%q,"unknownHint":42}}`, f.origin.URL+"/source.m3u8", exactNextGroup)),
		json.RawMessage(fmt.Sprintf(`{"name":"duplicate","url":%q,"behaviorHints":{"bingeGroup":%q}}`, f.origin.URL+"/source.m3u8", exactNextGroup)),
	}
	c, _ := ParseContent("series", "tmdb:100:1:1")
	stream := Stream{URL: f.origin.URL + "/source.m3u8"}
	stream.BehaviorHints.BingeGroup = exactNextGroup
	f.parent = f.s.newSession(c, stream)
	f.parent.source = cfg.Sources[0]
	f.parent.Duration = 600
	if !f.s.playerRequest(f.parent, httptest.NewRequest("GET", "/master.m3u8", nil)) {
		t.Fatal("selection failed")
	}
	return f
}
func (f *nextFixture) trigger(t *testing.T, at float64) *nextEpisodePrefetch {
	t.Helper()
	f.parent.position(at)
	f.s.maybePrefetchNextEpisode(f.parent)
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return f.s.nextEpisode
}
func waitNext(t *testing.T, p *nextEpisodePrefetch) {
	t.Helper()
	if p == nil {
		t.Fatal("prefetch not started")
	}
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Fatal("prefetch did not finish")
	}
}
func nextRequest(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestNextEpisodeTriggerEligibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   func(*nextFixture)
		position float64
	}{
		{"before threshold", func(*nextFixture) {}, 299},
		{"disabled", func(f *nextFixture) { cfg := f.s.Config.Get(); cfg.NextEpisodePrefetch = false; f.s.Config.Save(cfg) }, 300},
		{"unknown duration", func(f *nextFixture) { f.parent.Duration = 0 }, 300},
		{"invalid duration", func(f *nextFixture) { f.parent.Duration = math.Inf(1) }, 300},
		{"invalid position", func(*nextFixture) {}, math.NaN()},
		{"past end", func(*nextFixture) {}, 600},
		{"movie", func(f *nextFixture) { f.parent.Content.Type = "movie" }, 300},
		{"missing group", func(f *nextFixture) { f.parent.stream.BehaviorHints.BingeGroup = "" }, 300},
		{"only prepared", func(f *nextFixture) { f.parent.Playing = false }, 300},
		{"unknown source", func(f *nextFixture) { f.parent.source = Source{} }, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNextFixture(t)
			tc.change(f)
			if f.trigger(t, tc.position) != nil {
				t.Fatal("ineligible trigger")
			}
			if f.parent.nextEpisodeAttempted {
				t.Fatal("ineligible request consumed attempt")
			}
		})
	}
	t.Run("threshold and concurrent requests", func(t *testing.T) {
		f := newNextFixture(t)
		f.parent.position(300)
		var wg sync.WaitGroup
		for range 32 {
			wg.Go(func() { f.s.maybePrefetchNextEpisode(f.parent) })
		}
		wg.Wait()
		f.s.mu.Lock()
		p := f.s.nextEpisode
		f.s.mu.Unlock()
		waitNext(t, p)
		if !p.ready || f.selected.Load() != 1 || f.unrelated.Load() != 0 {
			t.Fatal("not one restricted lookup", p.ready, f.selected.Load(), f.unrelated.Load(), p.sessions[1].FallbackReason, p.sessions[1].Errors)
		}
		for range 10 {
			f.s.maybePrefetchNextEpisode(f.parent)
		}
		if f.selected.Load() != 1 {
			t.Fatal("duplicate prefetch")
		}
		if len(f.s.status(httptest.NewRequest("GET", "/api/status", nil))["sessions"].([]map[string]any)) != 1 {
			t.Fatal("speculation leaked into dashboard results")
		}
	})
}

func TestNextEpisodeMetadataIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		videos   []episodeMetadata
		want     string
	}{
		{"next", "tt123:1:1", []episodeMetadata{{Season: 1, Episode: 2}, {Season: 1, Episode: 1}}, "tt123:1:2"},
		{"season transition", "tmdb:100:1:8", []episodeMetadata{{Season: 2, Episode: 1}, {Season: 1, Episode: 8}}, "tmdb:100:2:1"},
		{"numeric namespace", "100:1:1", []episodeMetadata{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}, "100:1:2"},
		{"unknown current", "tt123:1:1", []episodeMetadata{{Season: 1, Episode: 2}}, ""},
		{"finale", "tt123:1:1", []episodeMetadata{{Season: 1, Episode: 1}}, ""},
		{"unreleased", "tt123:1:1", []episodeMetadata{{Season: 1, Episode: 1}, {Season: 1, Episode: 2, Released: "2099-01-01"}}, ""},
		{"special", "tt123:0:1", []episodeMetadata{{Season: 0, Episode: 1}, {Season: 1, Episode: 1}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := ParseContent("series", tc.id)
			got, err := nextFromVideos(c, tc.videos, time.Now())
			if tc.want == "" {
				if err == nil {
					t.Fatal(got)
				}
			} else if err != nil || got.ID != tc.want {
				t.Fatal(got, err)
			}
		})
	}
}

func TestNextEpisodeOneUseHandoffAndPreparedReuse(t *testing.T) {
	f := newNextFixture(t)
	generation := f.s.generation
	p := f.trigger(t, 300)
	waitNext(t, p)
	if !p.ready || f.s.generation != generation || !f.parent.Playing || f.parent.ctx.Err() != nil {
		t.Fatal("prefetch disturbed playback")
	}
	warmed := p.sessions[1]
	if warmed.Playing || !warmed.preparationStarted || p.sessions[2].preparationStarted {
		t.Fatal("first exact match selection is not deterministic")
	}
	if warmed.stream.BehaviorHints.BingeGroup != exactNextGroup {
		t.Fatal("group modified")
	}
	w := nextRequest(t, f.s, "POST", "/api/resolve", `{"type":"series","id":"tmdb:100:1:2"}`)
	if w.Code != 200 || f.unrelated.Load() != 1 || f.s.nextEpisode != p {
		t.Fatal("dashboard consumed speculative response", w.Body.String())
	}
	w = nextRequest(t, f.s, "HEAD", "http://head.test/stream/series/tmdb:100:1:2.json", "")
	if w.Code != 200 || f.s.nextEpisode != p || f.unrelated.Load() != 2 {
		t.Fatal("HEAD consumed speculation")
	}
	// A concurrent normal lookup can begin before handoff and finish afterward.
	_, lookup := f.s.beginLookup()
	if lookup.Err() != nil {
		t.Fatal("lookup canceled")
	}
	w = nextRequest(t, f.s, "GET", "https://player.test/stream/series/tmdb:100:1:2.json", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || f.s.nextEpisode != nil {
		t.Fatal(w.Body.String())
	}
	var response struct {
		Streams []Stream `json:"streams"`
	}
	json.Unmarshal(w.Body.Bytes(), &response)
	if len(response.Streams) != 3 || response.Streams[0].Name != "Fallback" || response.Streams[2].Name != "🇮🇹 duplicate" {
		t.Fatal("source order lost", w.Body.String())
	}
	if !strings.Contains(response.Streams[1].URL, "https://player.test/media/"+warmed.ID+"/") || !strings.Contains(w.Body.String(), `"unknownHint":42`) || !strings.Contains(w.Body.String(), `"keep":17`) {
		t.Fatal("request URL or unknown fields lost", w.Body.String())
	}
	if f.s.getSession(warmed.ID) != warmed || warmed.Playing || !f.parent.Playing {
		t.Fatal("handoff replaced playback or prepared session")
	}
	f.s.beginLookup()
	if f.s.getSession(warmed.ID) != warmed || warmed.ctx.Err() != nil {
		t.Fatal("concurrent normal lookup discarded handed-off session")
	}
	// HEAD cannot select this prepared stream.
	nextRequest(t, f.s, "HEAD", response.Streams[1].URL, "")
	if warmed.Playing || !f.parent.Playing {
		t.Fatal("HEAD selected next episode")
	}
	before := f.selected.Load()
	w = nextRequest(t, f.s, "GET", "/stream/series/tmdb:100:1:2.json", "")
	if f.unrelated.Load() != 3 || f.selected.Load() != before+1 {
		t.Fatal("later lookup was restricted")
	}
	// Old episode requests cannot recreate a consumed prefetch.
	f.trigger(t, 550)
	if f.s.nextEpisode != nil || f.selected.Load() != before+1 {
		t.Fatal("consumed prefetch recreated")
	}
	// Reuse the exact object and its once-only preparation during real selection.
	w = nextRequest(t, f.s, "GET", response.Streams[1].URL, "")
	if w.Code != 200 || !warmed.Playing || f.parent.ctx.Err() == nil || f.s.getSession(warmed.ID) != warmed {
		t.Fatal("prepared playback not reused", w.Code, w.Body.String())
	}
}

func TestNextEpisodeConcurrentConsumption(t *testing.T) {
	f := newNextFixture(t)
	p := f.trigger(t, 300)
	waitNext(t, p)
	c, _ := ParseContent("series", "tmdb:100:1:2")
	var hits atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if f.s.consumeNextEpisode(c) != nil {
				hits.Add(1)
			} else {
				f.s.beginLookup()
			}
		})
	}
	wg.Wait()
	if hits.Load() != 1 || p.sessions[1].ctx.Err() != nil || f.s.getSession(p.sessions[1].ID) != p.sessions[1] {
		t.Fatal("handoff race", hits.Load())
	}
	// Transferred sessions survive parent expiry and disabling speculative work.
	f.s.mu.Lock()
	f.s.discardLocked(f.parent)
	f.s.mu.Unlock()
	cfg := f.s.Config.Get()
	cfg.NextEpisodePrefetch = false
	f.s.Config.Save(cfg)
	if p.sessions[1].ctx.Err() != nil {
		t.Fatal("parent owned handed-off preparation")
	}
}

func TestNextEpisodeCollectionPreservesPreparedAlignment(t *testing.T) {
	for _, outcome := range []string{"handoff", "canceled", "expired", "parent expired"} {
		t.Run(outcome, func(t *testing.T) {
			f := newNextFixture(t)
			p := f.trigger(t, 300)
			waitNext(t, p)
			if !p.ready {
				t.Fatal("prefetch did not prepare")
			}
			warmed := p.sessions[p.expected]
			// Cached timing can approach its retention limit while its prepared
			// session is still detached from the normal stream results.
			manual := 2.25
			cached := Alignment{
				Manual: &manual, Updated: time.Now().Add(-59 * time.Minute),
				Boundaries: []Boundary{{SourceTime: 3, Offset: 1.5, Confidence: .9}},
			}
			f.s.Alignments.mu.Lock()
			f.s.Alignments.entries[warmed.Key] = cached
			f.s.Alignments.entries["unrelated"] = cached
			f.s.Alignments.mu.Unlock()
			f.parent.mu.Lock()
			lastUsed := f.parent.LastUsed
			f.parent.mu.Unlock()
			now := time.Now().Add(2 * time.Minute)
			f.s.collectExpired(now)
			if f.s.nextEpisode != p || !p.ready || f.s.getSession(warmed.ID) != nil || warmed.Playing {
				t.Fatal("collection changed detached prefetch ownership")
			}
			if got := f.s.Alignments.Get(warmed.Key); got.Manual == nil || *got.Manual != manual || len(got.Boundaries) != 1 || !got.Updated.Equal(cached.Updated) {
				t.Fatal("collection changed prepared timing", got)
			}
			if !f.s.Alignments.Get("unrelated").Updated.IsZero() {
				t.Fatal("collection retained an unrelated expired alignment")
			}
			f.parent.mu.Lock()
			unchanged := f.parent.LastUsed.Equal(lastUsed)
			f.parent.mu.Unlock()
			if !unchanged {
				t.Fatal("collection refreshed the parent session")
			}
			switch outcome {
			case "handoff":
				if f.s.consumeNextEpisode(p.content) == nil {
					t.Fatal("handoff failed")
				}
				f.s.collectExpired(now)
				if got := f.s.playbackOffset(warmed, 0); got != manual {
					t.Fatal("handed-off session lost its prepared offset", got)
				}
				return
			case "canceled":
				cfg := f.s.Config.Get()
				cfg.NextEpisodePrefetch = false
				if err := f.s.Config.Save(cfg); err != nil {
					t.Fatal(err)
				}
			case "expired":
				now = p.expires
			case "parent expired":
				f.parent.mu.Lock()
				f.parent.LastUsed = now.Add(-time.Hour)
				f.parent.mu.Unlock()
			}
			f.s.collectExpired(now)
			if f.s.nextEpisode != nil || warmed.ctx.Err() == nil || !f.s.Alignments.Get(warmed.Key).Updated.IsZero() {
				t.Fatal("discarded prefetch retained its expired alignment")
			}
		})
	}
}

func TestNextEpisodeCancellationAndNoStalePublication(t *testing.T) {
	for _, reason := range []string{"disabled", "source disabled", "source URL", "audio source URL", "alignment setting", "replacement", "discard", "expiry", "seek", "close"} {
		t.Run(reason, func(t *testing.T) {
			f := newNextFixture(t)
			f.block = make(chan struct{})
			p := f.trigger(t, 300)
			select {
			case <-f.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("stream request not started")
			}
			cfg := f.s.Config.Get()
			switch reason {
			case "disabled":
				cfg.NextEpisodePrefetch = false
				f.s.Config.Save(cfg)
			case "source disabled":
				cfg.Sources[0].Disabled = true
				f.s.Config.Save(cfg)
			case "source URL":
				cfg.Sources[0].ManifestURL = f.origin.URL + "/changed/manifest.json"
				f.s.Config.Save(cfg)
			case "audio source URL":
				cfg.Sources[2].BaseURL = f.origin.URL + "/changed"
				f.s.Config.Save(cfg)
			case "alignment setting":
				cfg.AlignmentSampleSeconds = 10
				f.s.Config.Save(cfg)
			case "replacement":
				v := f.s.newSession(f.parent.Content, Stream{})
				f.s.playerRequest(v, httptest.NewRequest("GET", "/master.m3u8", nil))
			case "discard":
				f.s.mu.Lock()
				f.s.discardLocked(f.parent)
				f.s.mu.Unlock()
			case "expiry":
				f.s.collectExpired(time.Now().Add(2 * time.Hour))
			case "seek":
				f.trigger(t, 0)
			case "close":
				f.s.Close()
			}
			waitNext(t, p)
			if p.ctx.Err() == nil || f.s.nextEpisode != nil {
				t.Fatal("obsolete result survived")
			}
			select {
			case <-f.canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP read not canceled")
			}
			close(f.block)
			if reason == "seek" {
				f.trigger(t, 550)
				if f.s.nextEpisode != nil {
					t.Fatal("seek repeatedly rearmed prefetch")
				}
			}
		})
	}
}

func TestNextEpisodeMissesAndFailures(t *testing.T) {
	for _, reason := range []string{"empty", "oversized", "case mismatch", "whitespace mismatch", "incompatible first match", "preparation failure", "no next", "expired", "mismatch", "unfinished"} {
		t.Run(reason, func(t *testing.T) {
			f := newNextFixture(t)
			switch reason {
			case "empty":
				f.streams = nil
			case "oversized":
				for len(f.streams) <= nextEpisodeStreamLimit {
					f.streams = append(f.streams, f.streams[1])
				}
			case "case mismatch":
				f.parent.stream.BehaviorHints.BingeGroup = strings.ToLower(exactNextGroup)
			case "whitespace mismatch":
				f.parent.stream.BehaviorHints.BingeGroup = strings.TrimSpace(exactNextGroup)
			case "incompatible first match":
				f.streams = append([]json.RawMessage{json.RawMessage(fmt.Sprintf(`{"infoHash":"abc","behaviorHints":{"bingeGroup":%q}}`, exactNextGroup))}, f.streams...)
			case "preparation failure":
				f.streams = []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"url":%q,"behaviorHints":{"bingeGroup":%q}}`, f.origin.URL+"/missing.m3u8", exactNextGroup))}
			case "no next":
				f.videos = f.videos[:1]
			case "unfinished":
				f.block = make(chan struct{})
			}
			p := f.trigger(t, 300)
			c, _ := ParseContent("series", "tmdb:100:1:2")
			if reason == "unfinished" {
				select {
				case <-f.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("not started")
				}
				started := time.Now()
				if f.s.consumeNextEpisode(c) != nil || time.Since(started) > time.Second {
					t.Fatal("unfinished speculation waited")
				}
				f.s.mu.Lock()
				f.s.cancelNextEpisodeLocked()
				f.s.mu.Unlock()
				close(f.block)
				waitNext(t, p)
				return
			}
			waitNext(t, p)
			switch reason {
			case "expired":
				f.s.mu.Lock()
				p.expires = time.Now().Add(-time.Second)
				f.s.mu.Unlock()
			case "mismatch":
				c, _ = ParseContent("series", "tmdb:100:2:1")
			}
			if f.s.consumeNextEpisode(c) != nil {
				t.Fatal("served unusable or mismatched speculation")
			}
			if reason != "mismatch" && f.s.nextEpisode != nil {
				t.Fatal("failed result retained")
			}
			path := "/stream/series/tmdb:100:1:2.json"
			if reason == "mismatch" {
				path = "/stream/series/tmdb:100:2:1.json"
			}
			w := nextRequest(t, f.s, "GET", path, "")
			if w.Code != 200 || f.unrelated.Load() != 1 {
				t.Fatal("ordinary discovery unavailable", w.Code)
			}
		})
	}
}

func TestNextEpisodeOnlyPlayerMediaGETTriggers(t *testing.T) {
	f := newNextFixture(t)
	f.parent.position(300)
	for range 3 {
		f.s.status(httptest.NewRequest("GET", "/api/status", nil))
	}
	if f.s.nextEpisode != nil {
		t.Fatal("dashboard polling triggered work")
	}
	resource := f.parent.addResource(Origin{URL: f.origin.URL + "/segment.ts"}, 300, true, true)
	nextRequest(t, f.s, "HEAD", "http://player.test"+resource, "")
	if f.s.nextEpisode != nil {
		t.Fatal("HEAD triggered work")
	}
	// Audio arriving later must not override the last video requested area.
	f.parent.position(0)
	f.parent.audioPosition(550)
	f.s.maybePrefetchNextEpisode(f.parent)
	if f.s.nextEpisode != nil {
		t.Fatal("audio overrode video position")
	}
	nextRequest(t, f.s, "GET", "http://player.test"+resource, "")
	f.s.mu.Lock()
	p := f.s.nextEpisode
	f.s.mu.Unlock()
	waitNext(t, p)
	if !p.ready {
		t.Fatal("player resource request did not prepare next episode")
	}
}

func TestNextEpisodeNativeSubtitlePreloadPreservesAttempt(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, media := range []string{"audio", "video"} {
			t.Run(fmt.Sprintf("nested=%t/%s", nested, media), func(t *testing.T) {
				f := newNextFixture(t)
				const subtitles = "WEBVTT\n\n00:05:01.000 --> 00:05:02.000\nCiao\n"
				const master = "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"Italian\",LANGUAGE=\"it\",URI=\"audio.m3u8\"\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"Italian\",LANGUAGE=\"it\",URI=\"subtitles.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO=\"audio\",SUBTITLES=\"subs\"\nvideo.m3u8\n"
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/subtitles.m3u8":
						if nested {
							io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nsubtitles-media.m3u8\n")
							return
						}
						fallthrough
					case "/subtitles-media.m3u8":
						io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:300\n#EXTINF:300,\nearly.vtt\n#EXTINF:300,\nlate.vtt\n#EXT-X-ENDLIST\n")
					case "/video.m3u8", "/audio.m3u8":
						io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:300\n#EXTINF:300,\nearly.bin\n#EXTINF:300,\nlate.bin\n#EXT-X-ENDLIST\n")
					case "/early.vtt", "/late.vtt":
						io.WriteString(w, subtitles)
					case "/early.bin", "/late.bin":
						io.WriteString(w, "native media")
					default:
						t.Errorf("unexpected native request: %s", r.URL)
						http.NotFound(w, r)
					}
				}))
				defer origin.Close()
				cfg := f.s.Config.Get()
				cfg.Sources[2].Disabled = false
				if err := f.s.Config.Save(cfg); err != nil {
					t.Fatal(err)
				}
				f.parent.source = cfg.Source("vixsrc")
				f.parent.stream.BehaviorHints.BingeGroup = builtinBingeGroup("vixsrc", "1 Mbps")
				f.parent.native = &nativePlayback{VariantURL: origin.URL + "/video.m3u8", Italian: true}
				var err error
				f.parent.nativeMaster, err = ParseHLS([]byte(master), Origin{URL: origin.URL + "/master.m3u8"})
				if err != nil {
					t.Fatal(err)
				}
				load := func(uri string) *HLS {
					t.Helper()
					w := nextRequest(t, f.s, "GET", uri, "")
					if w.Code != http.StatusOK {
						t.Fatal(w.Code, w.Body.String())
					}
					h, err := ParseHLS(w.Body.Bytes(), Origin{URL: "http://player.test" + uri})
					if err != nil {
						t.Fatal(err)
					}
					return h
				}
				h := load("/media/" + f.parent.ID + "/master.m3u8")
				var subtitleURL, mediaURL string
				for _, rendition := range h.Master.Renditions {
					if rendition.Type == "SUBTITLES" {
						subtitleURL = *rendition.URI
					} else if media == "audio" {
						mediaURL = *rendition.URI
					}
				}
				if media == "video" {
					mediaURL = h.Master.Variants[0].URI
				}
				sub := load(subtitleURL)
				if nested {
					sub = load(sub.Master.Variants[0].URI)
				}
				w := nextRequest(t, f.s, "GET", sub.Segments[1].URI, "")
				if w.Code != http.StatusOK || w.Body.String() != subtitles {
					t.Fatal("native subtitle bytes or timing changed", w.Code, w.Body.String())
				}
				f.s.mu.Lock()
				attempted, p := f.parent.nextEpisodeAttempted, f.s.nextEpisode
				f.s.mu.Unlock()
				if attempted || p != nil || !f.parent.PositionAt.IsZero() {
					t.Fatal("subtitle preload changed playback position or consumed the prefetch attempt")
				}
				playing := load(mediaURL)
				if w := nextRequest(t, f.s, "GET", playing.Segments[0].URI, ""); w.Code != http.StatusOK {
					t.Fatal(w.Code, w.Body.String())
				}
				if f.parent.nextEpisodeAttempted || f.parent.Position != 0 || f.parent.PositionAt.IsZero() {
					t.Fatal("early audio/video request did not establish the playback area")
				}
				if w := nextRequest(t, f.s, "GET", playing.Segments[1].URI, ""); w.Code != http.StatusOK {
					t.Fatal(w.Code, w.Body.String())
				}
				f.s.mu.Lock()
				p = f.s.nextEpisode
				f.s.mu.Unlock()
				waitNext(t, p)
				if !p.ready || f.parent.Position != 300 {
					t.Fatal("audio/video request could not prepare the next episode")
				}
			})
		}
	}
}

func TestNextEpisodeRequestBaseURL(t *testing.T) {
	for _, public := range []string{"", "https://public.test/prefix"} {
		t.Run(public, func(t *testing.T) {
			f := newNextFixture(t)
			cfg := f.s.Config.Get()
			cfg.PublicURL = public
			f.s.Config.Save(cfg)
			p := f.trigger(t, 300)
			waitNext(t, p)
			r := httptest.NewRequest("GET", "http://internal.test/stream/series/tmdb:100:1:2.json", nil)
			r.Header.Set("X-Forwarded-Host", "external.test")
			r.Header.Set("X-Forwarded-Proto", "https")
			w := httptest.NewRecorder()
			f.s.ServeHTTP(w, r)
			var response struct {
				Streams []Stream `json:"streams"`
			}
			json.Unmarshal(w.Body.Bytes(), &response)
			want := public
			if want == "" {
				want = "https://external.test"
			}
			u, _ := url.Parse(response.Streams[1].URL)
			if !strings.HasPrefix(u.String(), want+"/media/") {
				t.Fatal(u, w.Body.String())
			}
		})
	}
}

func TestNextEpisodeBuiltInSources(t *testing.T) {
	for _, provider := range []string{"vixsrc", "movy"} {
		t.Run(provider, func(t *testing.T) {
			f := newNextFixture(t)
			cfg := f.s.Config.Get()
			for i := range cfg.Sources {
				if cfg.Sources[i].Type == provider {
					cfg.Sources[i].Disabled = false
				}
			}
			if err := f.s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			f.parent.source = cfg.Source(provider)
			quality := "1080p"
			if provider == "vixsrc" {
				quality = "1 Mbps"
			}
			f.parent.stream.BehaviorHints.BingeGroup = builtinBingeGroup(provider, quality)
			mockMovyNetwork(f.s.Net, f.origin.URL)
			p := f.trigger(t, 300)
			waitNext(t, p)
			if !p.ready || f.selected.Load() != 0 || f.unrelated.Load() != 0 {
				t.Fatal("built-in prefetch queried addon streams", p.ready, f.selected.Load(), f.unrelated.Load())
			}
			if len(p.sessions) != 1 || !sameSource(p.sessions[0].source, cfg.Source(provider)) || !p.sessions[0].preparationStarted || p.sessions[0].Playing {
				t.Fatal("built-in preparation not complete")
			}
			if (p.sessions[0].native != nil) != (provider == "vixsrc") {
				t.Fatal("wrong native behavior")
			}
		})
	}
}

func TestNextEpisodeBackgroundStageDeadlinesAndCancellation(t *testing.T) {
	f := newNextFixture(t)
	priority := &speculativeWork{engine: f.s.Engine}
	priority.active.Store(true)
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), speculativeWorkKey{}, priority), 3*time.Second)
	defer cancel()
	release := f.s.Engine.foregroundRequest()
	started := make(chan struct{}, 2)
	pcm := signalPCM(40)
	extractor := func(ctx context.Context, at, length float64) ([]int16, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 15*time.Millisecond {
			t.Error("foreground wait consumed download deadline")
		}
		started <- struct{}{}
		return pcm[int(at*pcmRate):int((at+length)*pcmRate)], nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := findAnchor(ctx, extractor, extractor, 10, 0, 1, 40, 5, alignmentOptions{DownloadTimeout: 30 * time.Millisecond, CalculationTimeout: time.Second})
		done <- err
	}()
	select {
	case <-started:
		t.Fatal("speculative extraction bypassed foreground")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	release = f.s.Engine.foregroundRequest()
	waiting, stop := context.WithCancel(ctx)
	canceled := make(chan error, 1)
	go func() { canceled <- waitSpeculativeWork(waiting) }()
	stop()
	if err := <-canceled; err != context.Canceled {
		t.Fatal(err)
	}
	release()
}

func TestNextEpisodeSyntheticAlignmentAndPlaybackReuse(t *testing.T) {
	dir := fixture(t)
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "english.wav"), "-c:a", "aac", "-hls_time", "6", "-hls_playlist_type", "vod", "-hls_segment_filename", filepath.Join(dir, "en-%03d.ts"), filepath.Join(dir, "en.m3u8"))
	var sourceReads atomic.Int64
	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/meta/"):
			io.WriteString(w, `{"meta":{"name":"Synthetic series","videos":[{"season":1,"episode":1},{"season":1,"episode":2}]}}`)
		case strings.HasPrefix(r.URL.Path, "/stream/"):
			fmt.Fprintf(w, `{"streams":[{"name":"Synthetic MKV","url":%q,"behaviorHints":{"bingeGroup":"synthetic:exact"}}]}`, origin.URL+"/source.mkv")
		case strings.HasPrefix(r.URL.Path, "/api/tv/"):
			io.WriteString(w, `{"src":"/embed"}`)
		case r.URL.Path == "/embed":
			io.WriteString(w, `window.masterPlaylist={url:'/vix',params:{token:'fixture',expires:9999999999}}`)
		case r.URL.Path == "/vix.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"English\",LANGUAGE=\"en\",URI=\"en.m3u8\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Italian\",LANGUAGE=\"it\",URI=\"en.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO=\"a\"\nhq.m3u8\n")
		default:
			if r.URL.Path == "/source.mkv" {
				sourceReads.Add(1)
				if r.Header.Get("Range") == "" {
					t.Error("speculation attempted an unbounded source read")
				}
			}
			http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
		}
	}))
	defer origin.Close()
	s := lifecycleServer(t)
	cfg := s.Config.Get()
	cfg.Sources = withMovyDisabled([]Source{{Type: "addon", Name: "Synthetic", ManifestURL: origin.URL + "/manifest.json"}, {Type: "vixsrc", BaseURL: origin.URL, Disabled: true}})
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	c, _ := ParseContent("series", "tmdb:100:1:1")
	stream := Stream{URL: origin.URL + "/source.mkv"}
	stream.BehaviorHints.BingeGroup = "synthetic:exact"
	parent := s.newSession(c, stream)
	parent.source = cfg.Sources[0]
	parent.Duration = 600
	s.playerRequest(parent, httptest.NewRequest("GET", "/master.m3u8", nil))
	manual := 1.25
	if err := s.Alignments.UpdateContext(parent.ctx, parent.Key, func(a *Alignment) { a.Manual = &manual }); err != nil {
		t.Fatal(err)
	}
	parent.position(300)
	s.maybePrefetchNextEpisode(parent)
	s.mu.Lock()
	p := s.nextEpisode
	s.mu.Unlock()
	waitNext(t, p)
	if !p.ready {
		t.Fatalf("synthetic prefetch failed: %+v", p.sessions[0].Errors)
	}
	warmed := p.sessions[0]
	alignment := s.Alignments.Get(warmed.Key)
	if !alignment.reusable(cfg) || math.Abs(alignment.Offset-2.4) > .12 {
		t.Fatalf("alignment did not finish: %+v; errors: %+v", alignment, warmed.Errors)
	}
	if warmed.Playing || warmed.Aligning || !parent.Playing || s.playbackOffset(parent, 0) != manual {
		t.Fatal("preparation modified actual playback")
	}
	before := sourceReads.Load()
	w := nextRequest(t, s, "GET", "http://player.test/stream/series/tmdb:100:1:2.json", "")
	var response struct {
		Streams []Stream `json:"streams"`
	}
	json.Unmarshal(w.Body.Bytes(), &response)
	if len(response.Streams) != 1 {
		t.Fatal(w.Body.String())
	}
	w = nextRequest(t, s, "GET", response.Streams[0].URL, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Italian · Vixsrc") {
		t.Fatal("prepared playback failed", w.Code, w.Body.String())
	}
	if s.getSession(warmed.ID) != warmed || sourceReads.Load() != before || !warmed.Playing || warmed.Aligning || s.playbackOffset(warmed, 0) != alignment.Offset {
		t.Fatal("actual playback rebuilt or realigned preparation")
	}
}

func TestNextEpisodeMetadataResolvers(t *testing.T) {
	for _, mode := range []string{"tmdb", "builtin numeric"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int64
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/3/tv/100":
					io.WriteString(w, `{"seasons":[{"season_number":2},{"season_number":0},{"season_number":1}]}`)
				case "/3/tv/100/season/1":
					io.WriteString(w, `{"episodes":[{"season_number":1,"episode_number":8,"air_date":"2020-01-01"}]}`)
				case "/3/tv/100/season/2":
					io.WriteString(w, `{"episodes":[{"season_number":2,"episode_number":1,"air_date":"2021-01-01"},{"season_number":2,"episode_number":2,"air_date":"2021-01-08"}]}`)
				case "/tv/100/1/8":
					io.WriteString(w, movyPage("100", "tv", "Fixture"))
				case "/meta/series/tt1234567.json":
					io.WriteString(w, `{"meta":{"videos":[{"season":1,"episode":8},{"season":2,"episode":1},{"season":2,"episode":2}]}}`)
				default:
					t.Errorf("unexpected metadata/stream request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer origin.Close()
			s := lifecycleServer(t)
			target, _ := url.Parse(origin.URL)
			base := s.Net.Client.Transport
			s.Net.Client.Transport = movyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				clone := r.Clone(r.Context())
				u := *r.URL
				clone.URL = &u
				clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
				return base.RoundTrip(clone)
			})
			cfg := s.Config.Get()
			cfg.Sources = defaultSources()
			cfg.Sources[1].BaseURL = origin.URL
			if mode == "tmdb" {
				cfg.TMDBToken = "private-token"
			}
			c, _ := ParseContent("series", "tmdb:100:1:8")
			plan, err := s.resolveNextEpisode(context.Background(), c, cfg, cfg.Sources[0])
			if err != nil || plan.Next.ID != "tmdb:100:2:1" || plan.Following.ID != "tmdb:100:2:2" {
				t.Fatal(plan, err)
			}
			want := int64(2)
			if mode == "tmdb" {
				want = 3
			}
			if requests.Load() != want {
				t.Fatal("unexpected resolver fanout", requests.Load())
			}
		})
	}
}

func TestNextEpisodeSettingsAPIAndSourceIdentityPersistence(t *testing.T) {
	f := newNextFixture(t)
	cfg := f.s.Config.Get()
	cfg.NextEpisodeLeadMinutes = 1
	data, _ := json.Marshal(cfg)
	w := nextRequest(t, f.s, "POST", "/api/settings", string(data))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"nextEpisodeLeadMinutes":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.trigger(t, 539) != nil {
		t.Fatal("custom lead ignored")
	}
	p := f.trigger(t, 540)
	waitNext(t, p)
	// Reorders, renames and source autosaves leave the real selected connection valid.
	cfg = f.s.Config.Get()
	cfg.Sources[0], cfg.Sources[1] = cfg.Sources[1], cfg.Sources[0]
	cfg.Sources[1].Name = "renamed"
	if err := f.s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if f.s.nextEpisode != p || p.ctx.Err() != nil {
		t.Fatal("source display identity invalidated real connection")
	}
	cfg.NextEpisodeLeadMinutes = 31
	data, _ = json.Marshal(cfg)
	w = nextRequest(t, f.s, "POST", "/api/settings", string(data))
	if w.Code != 400 || f.s.Config.Get().NextEpisodeLeadMinutes != 1 {
		t.Fatal("invalid lead accepted")
	}
	// Tickets preserve the actual source even when names/groups collide.
	f.parent.ticket = f.s.sealPlayback(f.parent)
	ticket := f.parent.ticket
	f.s.mu.Lock()
	f.s.discardLocked(f.parent)
	f.s.mu.Unlock()
	restored, err := f.s.restorePlayback(f.parent.ID, ticket)
	if err != nil || !sameSource(restored.source, f.parent.source) {
		t.Fatal("ticket lost actual source", err)
	}
	cfg = f.s.Config.Get()
	cfg.NextEpisodePrefetch = false
	data, _ = json.Marshal(cfg)
	w = nextRequest(t, f.s, "POST", "/api/settings", string(data))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	reopened, err := OpenConfig(f.s.Config.path)
	if err != nil || reopened.Get().NextEpisodePrefetch || reopened.Get().NextEpisodeLeadMinutes != 1 {
		t.Fatal("API settings not persisted", err)
	}
}

func TestNextEpisodePreparationSubprocessCancellation(t *testing.T) {
	f := newNextFixture(t)
	started := filepath.Join(t.TempDir(), "probe-started")
	cfg := f.s.Config.Get()
	// The path is generated locally by TempDir; quoting preserves spaces.
	script := fmt.Sprintf("#!/bin/sh\n: > '%s'\nexec sleep 60\n", strings.ReplaceAll(started, "'", "'\\''"))
	if err := os.WriteFile(cfg.FFprobe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p := f.trigger(t, 300)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("preparation probe did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cfg.NextEpisodePrefetch = false
	if err := f.s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	waitNext(t, p)
	select {
	case <-p.sessions[1].ready:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not stop preparation subprocess")
	}
	if p.sessions[1].prepareErr == nil || p.sessions[1].ctx.Err() == nil || f.s.nextEpisode != nil || !f.parent.Playing || f.parent.ctx.Err() != nil {
		t.Fatal("cancellation disturbed playback or published preparation")
	}
}
