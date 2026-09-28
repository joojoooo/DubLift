package dublift

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func lifecycleServer(t *testing.T) *Server {
	t.Helper()
	c, err := OpenConfig(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func jsonValue(t *testing.T, b []byte) any {
	t.Helper()
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFallbackPreservesEveryFieldAndUpstreamOrder(t *testing.T) {
	ffmpegAvailable(t)
	for _, italian := range []bool{false, true} {
		t.Run(fmt.Sprint("italian=", italian), func(t *testing.T) {
			var first, second []json.RawMessage
			var origin *httptest.Server
			origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/slow/stream/movie/tmdb:603.json":
					time.Sleep(20 * time.Millisecond)
					jsonResponse(w, 200, map[string]any{"streams": first})
				case "/fast/stream/movie/tmdb:603.json":
					jsonResponse(w, 200, map[string]any{"streams": second})
				case "/slow/meta/movie/tmdb:603.json", "/fast/meta/movie/tmdb:603.json":
					io.WriteString(w, `{"meta":{"name":"The Matrix"}}`)
				case "/api/movie/603":
					io.WriteString(w, `{"src":"/embed"}`)
				case "/embed":
					io.WriteString(w, `window.masterPlaylist={url:'/vix',params:{token:'fixture',expires:9999999999}}`)
				case "/vix.m3u8":
					lang := "en"
					if italian {
						lang = "it"
					}
					fmt.Fprintf(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Audio\",LANGUAGE=\"%s\",URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO=\"a\"\nvideo.m3u8\n", lang)
				case "/audio.m3u8", "/video.m3u8":
					io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
				case "/segment.ts":
					if r.Header.Get("Range") != "bytes=0-511" {
						t.Error("listing must only request a media prefix")
					}
					w.Header().Set("Content-Type", "video/mp2t")
					w.Write(make([]byte, 512))
				case "/no-range.mkv":
					if r.Header.Get("Range") != "bytes=0-0" && r.Header.Get("Range") != "bytes=0-511" {
						t.Error("expected small range probe")
					}
					io.WriteString(w, "origin ignores ranges")
				default:
					t.Errorf("unexpected request (listing must not read media): %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer origin.Close()
			first = []json.RawMessage{
				json.RawMessage(fmt.Sprintf(`{"name":"Zulu · 4K","title":"The Matrix\nFull title and size","description":"All provider details\nSecond line","url":%q,"behaviorHints":{"filename":"The.Matrix.1999.mkv","proxyHeaders":{"request":{"Referer":"https://required.test/"}},"videoSize":123456789},"subtitles":[{"id":"it","lang":"ita","url":"https://subs.test/1"}],"customField":{"keep":true}}`, origin.URL+"/video.m3u8")),
				json.RawMessage(`{"name":"Donation","externalUrl":"https://pengu.uk/donate"}`),
				json.RawMessage(`{"name":"Torrent","infoHash":"0123456789abcdef","fileIdx":3,"sources":["tracker:test"],"behaviorHints":{"videoSize":555}}`),
			}
			second = []json.RawMessage{
				json.RawMessage(fmt.Sprintf(`{"name":"Alpha","description":"Nonseekable original","url":%q,"headers":{"X-Test":"keep"}}`, origin.URL+"/no-range.mkv")),
				json.RawMessage(`{"name":"External","externalUrl":"https://player.test/","unknown":17}`),
				json.RawMessage(`{"name":"Nearby","externalUrl":"https://pengu.uk/donate/"}`),
			}
			s := lifecycleServer(t)
			cfg := s.Config.Get()
			cfg.VixBaseURL = origin.URL
			cfg.Addons = []Addon{{"Slow", origin.URL + "/slow/manifest.json"}, {"Fast", origin.URL + "/fast/manifest.json"}}
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest("GET", "/stream/movie/tmdb:603.json", nil))
			var result struct {
				Streams []json.RawMessage `json:"streams"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			want := []json.RawMessage{first[0], first[2], second[0], second[1], second[2]}
			if len(result.Streams) != len(want) {
				t.Fatal(w.Body.String())
			}
			for i := range want {
				got, expected := jsonValue(t, result.Streams[i]), jsonValue(t, want[i])
				if italian && i == 0 {
					m, e := got.(map[string]any), expected.(map[string]any)
					if m["name"] != "🇮🇹 "+e["name"].(string) || !strings.Contains(m["url"].(string), "/media/") {
						t.Fatal(m)
					}
					m["name"], m["url"] = e["name"], e["url"]
					h := m["behaviorHints"].(map[string]any)
					if h["notWebReady"] != true {
						t.Fatal(h)
					}
					h["filename"] = e["behaviorHints"].(map[string]any)["filename"]
					h["proxyHeaders"] = e["behaviorHints"].(map[string]any)["proxyHeaders"]
					delete(h, "notWebReady")
				}
				if !reflect.DeepEqual(got, expected) {
					t.Fatalf("stream %d changed: %s", i, result.Streams[i])
				}
			}
			w = httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest("GET", "/api/status", nil))
			var state struct {
				Sessions []struct {
					Name               string
					Order              int
					Title, Description string
				}
			}
			json.Unmarshal(w.Body.Bytes(), &state)
			if len(state.Sessions) != 5 {
				t.Fatal(w.Body.String())
			}
			for i, name := range []string{"Zulu · 4K", "Torrent", "Alpha", "External", "Nearby"} {
				if state.Sessions[i].Name != name || state.Sessions[i].Order != i {
					t.Fatal(state)
				}
			}
			if state.Sessions[0].Title != "The Matrix\nFull title and size" || state.Sessions[0].Description != "All provider details\nSecond line" {
				t.Fatal(state)
			}
		})
	}
}

func TestOrderedItalianResultLimitAndSourceTimeout(t *testing.T) {
	for _, parallel := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("parallel=%d", parallel), func(t *testing.T) {
			ffmpegAvailable(t)
			var mu sync.Mutex
			var checked []string
			active, maxActive := 0, 0
			var streams []Stream
			var origin *httptest.Server
			origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/stream/movie/tmdb:603.json":
					jsonResponse(w, 200, map[string]any{"streams": streams})
				case "/api/movie/603":
					io.WriteString(w, `{"src":"/embed"}`)
				case "/embed":
					io.WriteString(w, `window.masterPlaylist={url:'/vix',params:{token:'fixture',expires:9999999999}}`)
				case "/vix.m3u8":
					io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Italiano\",LANGUAGE=\"it\",URI=\"it.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO=\"a\"\nvideo.m3u8\n")
				case "/it.m3u8":
					io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
				case "/bad.m3u8", "/one.m3u8", "/two.m3u8", "/three.m3u8":
					mu.Lock()
					checked = append(checked, r.URL.Path)
					active++
					maxActive = max(maxActive, active)
					mu.Unlock()
					defer func() {
						mu.Lock()
						active--
						mu.Unlock()
					}()
					if r.URL.Path == "/one.m3u8" {
						time.Sleep(200 * time.Millisecond)
					}
					if r.URL.Path == "/bad.m3u8" && parallel == 3 {
						time.Sleep(400 * time.Millisecond)
						io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
						return
					}
					if r.URL.Path == "/bad.m3u8" {
						select {
						case <-r.Context().Done():
						case <-time.After(1500 * time.Millisecond):
							http.Error(w, "slow source", http.StatusServiceUnavailable)
						}
						return
					}
					io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
				case "/segment.ts":
					w.Header().Set("Content-Type", "video/mp2t")
					w.Write(make([]byte, 512))
				default:
					http.NotFound(w, r)
				}
			}))
			defer origin.Close()
			for _, name := range []string{"bad", "one", "two", "three"} {
				streams = append(streams, Stream{Name: name, URL: origin.URL + "/" + name + ".m3u8"})
			}
			s := lifecycleServer(t)
			cfg := s.Config.Get()
			cfg.VixBaseURL = origin.URL
			cfg.Addons = []Addon{{Name: "Fixture", ManifestURL: origin.URL + "/manifest.json"}}
			cfg.MaxItalianResults = 2
			cfg.SourceCheckTimeoutSeconds = 1
			cfg.SourceCheckParallelism = parallel
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest("GET", "/stream/movie/tmdb:603.json", nil))
			var response struct {
				Streams []Stream `json:"streams"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Streams) != 4 {
				t.Fatal(w.Body.String())
			}
			for i, stream := range response.Streams {
				wantItalian := (parallel == 3 && i < 2) || (parallel != 3 && (i == 1 || i == 2))
				if strings.HasPrefix(stream.Name, "🇮🇹 ") != wantItalian {
					t.Fatalf("stream %d Italian=%t, want %t", i, strings.HasPrefix(stream.Name, "🇮🇹 "), wantItalian)
				}
				if (stream.URL != streams[i].URL) != wantItalian {
					t.Fatalf("stream %d URL transformed unexpectedly", i)
				}
			}
			mu.Lock()
			order := append([]string(nil), checked...)
			observedParallel := maxActive
			mu.Unlock()
			if parallel == 1 && !reflect.DeepEqual(order, []string{"/bad.m3u8", "/one.m3u8", "/two.m3u8"}) {
				t.Fatalf("source checks = %v", order)
			}
			sort.Strings(order)
			wantChecks := []string{"/bad.m3u8", "/one.m3u8", "/two.m3u8"}
			if parallel == 3 {
				wantChecks = wantChecks[:2]
			}
			if !reflect.DeepEqual(order, wantChecks) || observedParallel != min(parallel, cfg.MaxItalianResults) {
				t.Fatalf("source checks = %v, max parallel = %d", order, observedParallel)
			}
			w = httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest("GET", "/api/status", nil))
			var state struct {
				Sessions []struct {
					Passthrough    bool
					FallbackReason string
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
				t.Fatal(err)
			}
			if len(state.Sessions) != 4 || !state.Sessions[3].Passthrough || state.Sessions[3].FallbackReason != "Italian result limit reached" {
				t.Fatal(w.Body.String())
			}
			if parallel == 3 {
				if state.Sessions[0].Passthrough || state.Sessions[2].FallbackReason != "Italian result limit reached" {
					t.Fatal(w.Body.String())
				}
			} else if !strings.Contains(state.Sessions[0].FallbackReason, "timeout") {
				t.Fatal(w.Body.String())
			}
		})
	}
}

func TestBypassSourceChecksDefersUntilPrepareAndRequiresItalian(t *testing.T) {
	ffmpegAvailable(t)
	var italian atomic.Bool
	italian.Store(true)
	var sourceRequests atomic.Int64
	var streams []Stream
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stream/movie/tmdb:603.json":
			jsonResponse(w, 200, map[string]any{"streams": streams})
		case "/api/movie/603":
			io.WriteString(w, `{"src":"/embed"}`)
		case "/embed":
			io.WriteString(w, `window.masterPlaylist={url:'/vix',params:{token:'fixture',expires:9999999999}}`)
		case "/vix.m3u8":
			lang := "it"
			if !italian.Load() {
				lang = "en"
			}
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Audio\",LANGUAGE=\"%s\",URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO=\"a\"\nvideo.m3u8\n", lang)
		case "/audio.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
		case "/one.m3u8", "/two.m3u8":
			sourceRequests.Add(1)
			http.Error(w, "unavailable source", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	streams = []Stream{
		{Name: "one", URL: origin.URL + "/one.m3u8"},
		{Name: "two", URL: origin.URL + "/two.m3u8"},
		{Name: "torrent"},
		{Name: "donation", ExternalURL: "https://pengu.uk/donate"},
	}
	s := lifecycleServer(t)
	cfg := s.Config.Get()
	cfg.VixBaseURL = origin.URL
	cfg.Addons = []Addon{{Name: "Fixture", ManifestURL: origin.URL + "/manifest.json"}}
	cfg.MaxItalianResults = 1
	cfg.BypassSourceChecks = true
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/stream/movie/tmdb:603.json", nil))
	var response struct {
		Streams []Stream `json:"streams"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Streams) != 3 || sourceRequests.Load() != 0 {
		t.Fatal(w.Body.String(), "source requests:", sourceRequests.Load())
	}
	for i, stream := range response.Streams {
		if (i < 2) != strings.HasPrefix(stream.Name, "🇮🇹 ") {
			t.Fatalf("stream %d: %s", i, stream.Name)
		}
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/status", nil))
	var state struct {
		Sessions []struct {
			ID                  string
			SourceCheckDeferred bool
			Passthrough         bool
			FallbackReason      string
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 3 || !state.Sessions[0].SourceCheckDeferred || !state.Sessions[1].SourceCheckDeferred || state.Sessions[2].SourceCheckDeferred {
		t.Fatal(w.Body.String())
	}
	prepare := httptest.NewRequest("POST", "/api/sessions/"+state.Sessions[0].ID+"/prepare", strings.NewReader(`{}`))
	prepare.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, prepare)
	if w.Code != http.StatusBadGateway || sourceRequests.Load() != 1 {
		t.Fatal("Prepare must check its source:", w.Code, sourceRequests.Load())
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/status", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Sessions[0].Passthrough || state.Sessions[0].SourceCheckDeferred || state.Sessions[0].FallbackReason == "" {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/media/"+state.Sessions[1].ID+"/master.m3u8", nil))
	if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != streams[1].URL || sourceRequests.Load() != 2 {
		t.Fatal("playback must check and fall back to its source:", w.Code, w.Header().Get("Location"), sourceRequests.Load())
	}
	italian.Store(false)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/stream/movie/tmdb:603.json", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Streams) != 3 || sourceRequests.Load() != 2 {
		t.Fatal(w.Body.String(), "source requests:", sourceRequests.Load())
	}
	for i, stream := range response.Streams {
		if stream.URL != streams[i].URL || strings.HasPrefix(stream.Name, "🇮🇹 ") {
			t.Fatalf("stream %d became Italian without Vixsrc audio", i)
		}
	}
}

func TestPlaybackDetectionAndAutomaticCleanup(t *testing.T) {
	s := lifecycleServer(t)
	c := Content{ID: "tmdb:603", Type: "movie"}
	a := s.newSession(c, Stream{Name: "first", URL: "https://example.test/a.m3u8"})
	b := s.newSession(c, Stream{Name: "second", URL: "https://example.test/b.m3u8"})
	cache := func(v *Session) {
		t.Helper()
		if _, err := s.Engine.Cache.Get(v.ctx, "analysis", func() ([]byte, error) { return make([]byte, 8), nil }); err != nil {
			t.Fatal(err)
		}
	}
	cache(a)
	cache(b)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("HEAD", "/media/"+a.ID+"/master.m3u8", nil))
	if w.Code != 200 || len(s.sessions) != 2 || a.Playing || a.RequestMethod != "HEAD" {
		t.Fatal("HEAD consumed stream results")
	}
	// Keep preparation blocked to prove the player is visible before any probe
	// or alignment completes.
	a.listedReady = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/media/"+a.ID+"/master.m3u8", nil).WithContext(ctx))
		close(done)
	}()
	deadline := time.After(time.Second)
	for {
		a.mu.Lock()
		playing := a.Playing
		a.mu.Unlock()
		if playing && s.getSession(b.ID) == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("playback not detected before preparation")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if b.ctx.Err() == nil || s.Engine.Cache.Used() != 8 {
		t.Fatal("unselected work/cache survived")
	}
	cancel()
	<-done
	previous := a.LastUsed
	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/status", nil))
	if !a.LastUsed.Equal(previous) {
		t.Fatal("dashboard polling kept session alive")
	}
	if err := s.Alignments.Update(a.Key, func(v *Alignment) { v.Offset = 2 }); err != nil {
		t.Fatal(err)
	}
	s.collectExpired(previous.Add(time.Hour))
	if s.getSession(a.ID) != nil || a.ctx.Err() == nil || s.Engine.Cache.Used() != 0 {
		t.Fatal("idle session/cache not collected")
	}
	if !s.Alignments.Get(a.Key).Updated.IsZero() {
		t.Fatal("idle alignment not deleted")
	}
	reopened, err := OpenAlignments(s.Alignments.path)
	if err != nil || !reopened.Get(a.Key).Updated.IsZero() {
		t.Fatal("disk cache survived collection", err)
	}
	if s.Alignments.UpdateContext(a.ctx, a.Key, func(v *Alignment) { v.Offset = 3 }) == nil {
		t.Fatal("canceled job restored deleted alignment")
	}
	next := s.newSession(c, Stream{URL: "https://example.test/next"})
	cache(next)
	_, lookup := s.beginLookup()
	if len(s.sessions) != 0 || next.ctx.Err() == nil || s.Engine.Cache.Used() != 0 {
		t.Fatal("new lookup kept old results")
	}
	s.beginLookup()
	if lookup.Err() == nil {
		t.Fatal("new lookup did not cancel previous lookup")
	}
}

func TestStartupReleasesAtFirstSampleAndUpdatesOffset(t *testing.T) {
	s := lifecycleServer(t)
	v := s.newSession(Content{}, Stream{})
	v.Aligning = true
	v.firstAligned = make(chan struct{})
	v.alignDone = make(chan struct{})
	// Immediate startup must not wait on any sample or anonymous-access check.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.awaitStartupAlignment(ctx, v)
	if ctx.Err() != nil {
		t.Fatal("immediate startup waited")
	}
	cfg := s.Config.Get()
	cfg.StartImmediately = false
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.awaitStartupAlignment(ctx, v); close(done) }()
	select {
	case <-done:
		t.Fatal("started before first sample")
	case <-time.After(20 * time.Millisecond):
	}
	close(v.firstAligned)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("waited for all samples")
	}
	select {
	case <-v.alignDone:
		t.Fatal("test unexpectedly completed all samples")
	default:
	}
	if err := s.Alignments.Update(v.Key, func(a *Alignment) { a.Offset = 1.25; a.Confidence = .9 }); err != nil {
		t.Fatal(err)
	}
	if got := s.playbackOffset(v, 30); got != 1.25 {
		t.Fatal("first offset unavailable", got)
	}
	v.startupZero = true
	if got := s.playbackOffset(v, 30); got != 0 {
		t.Fatal("immediate startup must hold zero", got)
	}
	s.Alignments.Update(v.Key, func(a *Alignment) { a.AutoComplete = true })
	if got := s.playbackOffset(v, 30); got != 1.25 {
		t.Fatal("finished alignment did not apply", got)
	}
	s.Alignments.Update(v.Key, func(a *Alignment) { a.AutoComplete = false; x := -.5; a.Manual = &x })
	if got := s.playbackOffset(v, 30); got != -.5 {
		t.Fatal("manual offset ignored", got)
	}
}

func TestShortAlignmentSettingsAndSampleCoverage(t *testing.T) {
	cfg := DefaultSettings()
	if cfg.SearchRadius != 8 || cfg.AlignmentSamples != 3 || !cfg.StartImmediately {
		t.Fatal("incorrect startup defaults")
	}
	for _, n := range []int{1, 2, 3, 12} {
		for _, duration := range []float64{84, 3600, 12000} {
			positions := alignmentPositions(duration, n)
			if len(positions) != n || positions[0] > 20 {
				t.Fatal(positions)
			}
			for i, p := range positions {
				if p < 0 || p+alignmentWindow > duration || (i > 0 && p <= positions[i-1]) {
					t.Fatal(positions)
				}
			}
			if n > 1 && math.Abs(positions[n-1]-duration*.8) > alignmentWindow {
				t.Fatal("late sample missing", positions)
			}
		}
	}
	s := lifecycleServer(t)
	cfg = s.Config.Get()
	cfg.AlignmentSamples = 5
	cfg.StartImmediately = false
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	fresh, err := OpenConfig(s.Config.path)
	if err != nil || fresh.Get().AlignmentSamples != 5 || fresh.Get().StartImmediately {
		t.Fatal("settings did not persist", err)
	}
	for _, invalid := range []int{0, 13} {
		cfg.AlignmentSamples = invalid
		if cfg.Validate() == nil {
			t.Fatal("invalid count accepted")
		}
	}
}

func TestClearedCacheCannotBeRepopulatedByInflightWork(t *testing.T) {
	c := NewByteCache(100)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		c.Get(context.Background(), "old", func() ([]byte, error) { close(started); <-release; return make([]byte, 20), nil })
	}()
	<-started
	c.Clear()
	close(release)
	<-done
	if c.Used() != 0 {
		t.Fatal("old work repopulated cleared cache")
	}
}

func TestResultRefreshDoesNotInterruptActivePlayback(t *testing.T) {
	s := lifecycleServer(t)
	active := s.newSession(Content{}, Stream{URL: "https://origin.test/active.m3u8"})
	s.playerRequest(active, httptest.NewRequest("GET", "/", nil))
	s.Engine.Cache.Get(active.ctx, "video", func() ([]byte, error) { return []byte("video"), nil })
	old := s.newSession(Content{}, Stream{URL: "https://origin.test/old.m3u8"})
	s.beginLookup()
	if active.ctx.Err() != nil || s.getSession(active.ID) != active || s.Engine.Cache.Used() != 5 {
		t.Fatal("result refresh interrupted current playback")
	}
	if old.ctx.Err() == nil || s.getSession(old.ID) != nil {
		t.Fatal("refresh retained unselected results")
	}
	next := s.newSession(Content{}, Stream{URL: "https://origin.test/next.m3u8"})
	s.playerRequest(next, httptest.NewRequest("GET", "/", nil))
	if active.ctx.Err() == nil || s.getSession(active.ID) != nil || s.Engine.Cache.Used() != 0 {
		t.Fatal("new selection kept the previous playback/cache")
	}
}
