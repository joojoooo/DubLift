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
	"strings"
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
				json.RawMessage(`{"name":"Torrent","infoHash":"0123456789abcdef","fileIdx":3,"sources":["tracker:test"],"behaviorHints":{"videoSize":555}}`),
			}
			second = []json.RawMessage{
				json.RawMessage(fmt.Sprintf(`{"name":"Alpha","description":"Nonseekable original","url":%q,"headers":{"X-Test":"keep"}}`, origin.URL+"/no-range.mkv")),
				json.RawMessage(`{"name":"External","externalUrl":"https://player.test/","unknown":17}`),
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
			want := append(append([]json.RawMessage{}, first...), second...)
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
			if len(state.Sessions) != 4 {
				t.Fatal(w.Body.String())
			}
			for i, name := range []string{"Zulu · 4K", "Torrent", "Alpha", "External"} {
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
