package dublift

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type contentMetadataTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (t contentMetadataTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "v3-cinemeta.strem.io" {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = t.target.Scheme, t.target.Host
	}
	return t.base.RoundTrip(r)
}

func TestContentNameConsistentAcrossResolvers(t *testing.T) {
	const details = "📺 1280×720 · 📶 1.8 Mbps\n🎞️ H.264 / AAC"
	const description = "🍿 Inception (2010)\n📺 1080p\n💾 4 GB"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/addon/stream/movie/"):
			fmt.Fprintf(w, `{"streams":[{"name":"PenguPlay","description":%q,"externalUrl":"https://player.test/","customField":{"keep":true}}]}`, description)
		case strings.HasPrefix(r.URL.Path, "/addon/meta/"):
			http.NotFound(w, r) // The addon supplies streams but no title metadata.
		case r.URL.Path == "/meta/movie/tt1375666.json":
			io.WriteString(w, `{"meta":{"name":"Inception","moviedb_id":27205}}`)
		case r.URL.Path == "/api/movie/27205":
			io.WriteString(w, `{"src":"/embed"}`)
		case r.URL.Path == "/embed":
			io.WriteString(w, `window.masterPlaylist={url:'/master',params:{token:'fixture',expires:9999999999}}`)
		case r.URL.Path == "/master.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1800000,RESOLUTION=1280x720,CODECS=\"avc1.640028,mp4a.40.2\"\n720.m3u8\n")
		default:
			t.Errorf("title lookup or stream listing read unexpected media: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	target, _ := url.Parse(origin.URL)
	for _, tc := range []struct {
		name       string
		id         string
		dashboard  bool
		preset     string
		nativeOnly bool
		want       string
	}{
		{name: "dashboard preset", id: "tmdb:27205", dashboard: true, preset: "Inception", want: "Inception"},
		{name: "dashboard custom TMDB", id: "27205", dashboard: true, want: "Inception"},
		{name: "dashboard custom IMDb", id: "tt1375666", dashboard: true, want: "Inception"},
		{name: "Stremio IMDb", id: "tt1375666", want: "Inception"},
		{name: "Stremio TMDB", id: "tmdb:27205", want: "Inception"},
		{name: "VixSrc only preset", id: "tmdb:27205", dashboard: true, preset: "Inception", nativeOnly: true, want: "Inception"},
		{name: "VixSrc only IMDb", id: "tt1375666", nativeOnly: true, want: "Inception"},
		{name: "VixSrc unknown TMDB title", id: "tmdb:27205", nativeOnly: true, want: "Movie · 27205"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := lifecycleServer(t)
			s.Net.Client.Transport = contentMetadataTransport{base: s.Net.Client.Transport, target: target}
			cfg := s.Config.Get()
			cfg.FFmpeg, cfg.FFprobe = "/missing/ffmpeg", "/missing/ffprobe"
			cfg.Sources = withMovyDisabled([]Source{{Type: "vixsrc", BaseURL: origin.URL}})
			if !tc.nativeOnly {
				cfg.Sources = append(cfg.Sources, Source{Type: "addon", Name: "PenguPlay", ManifestURL: origin.URL + "/addon/manifest.json"})
			}
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/stream/movie/"+tc.id+".json", nil)
			if tc.dashboard {
				body, _ := json.Marshal(map[string]string{"type": "movie", "id": tc.id, "contentName": tc.preset})
				r = httptest.NewRequest(http.MethodPost, "/api/resolve", bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("lookup failed: %d %s", w.Code, w.Body.String())
			}
			var response struct {
				Streams []Stream `json:"streams"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			wantCount := 2
			if tc.nativeOnly {
				wantCount = 1
			}
			if len(response.Streams) != wantCount || response.Streams[0].Title != details {
				t.Fatal("listing lost VixSrc qualities or technical details", w.Body.String())
			}
			if !tc.nativeOnly && (response.Streams[1].Description != description || !strings.Contains(string(response.Streams[1].raw), `"customField":{"keep":true}`)) {
				t.Fatal("display changes modified the original addon stream", w.Body.String())
			}
			request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			var views []map[string]any
			deadline := time.Now().Add(3 * time.Second)
			for {
				views = s.status(request)["sessions"].([]map[string]any)
				matched := len(views) == wantCount
				for _, view := range views {
					matched = matched && view["contentName"] == tc.want
				}
				if matched {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("sources did not share the expected heading %q: %v", tc.want, views)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if views[0]["title"] != details {
				t.Fatal("resolution/bitrate was removed from dashboard stream details")
			}
			playbackURL, _ := url.Parse(views[0]["url"].(string))
			id := views[0]["id"].(string)
			ticket, err := s.readPlayback(id, playbackURL.Query().Get("resume"))
			if err != nil || ticket.ContentName != tc.want {
				t.Fatal("playback ticket lost the heading", ticket.ContentName, err)
			}
			// Restoring the same URL after cleanup retains the title immediately.
			s.beginLookup()
			restored, err := s.restorePlayback(id, playbackURL.Query().Get("resume"))
			if err != nil {
				t.Fatal(err)
			}
			restored.mu.Lock()
			name := restored.ContentName
			restored.mu.Unlock()
			if name != tc.want {
				t.Fatalf("restored heading = %q, want %q", name, tc.want)
			}
		})
	}
}

func TestFallbackContentNameKeepsTitlePunctuationAndEpisode(t *testing.T) {
	for _, tc := range []struct {
		kind, id, text, want string
	}{
		{"movie", "tmdb:27205", "🍿 Inception (2010)\r\n📺 1080p", "Inception"},
		{"movie", "tmdb:1", " 🎬 1917 (2019)", "1917"},
		{"movie", "tmdb:1", "🍿 Blade Runner 2049 (2017)", "Blade Runner 2049"},
		{"movie", "tmdb:1", "🍿 [REC] (2007)", "[REC]"},
		{"movie", "tmdb:1", "$5 a Day (2008)", "$5 a Day"},
		{"movie", "tmdb:1", "It (Chapter Two)", "It (Chapter Two)"},
		{"series", "tmdb:81349:1:2", "📺 Devs (2020)", "Devs · S01E02"},
		{"series", "tmdb:81349:1:2", "Devs S01E02", "Devs S01E02"},
		{"series", "tmdb:81349:1:2", "", "Series · 81349 · S01E02"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			c, err := ParseContent(tc.kind, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if got := fallbackContentName(c, Stream{Description: tc.text}); got != tc.want {
				t.Fatalf("heading = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestContentMetadataOnlyUpdatesItsOwnLiveSessions(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(fmt.Sprintf("selected=%t", selected), func(t *testing.T) {
			started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-release:
					io.WriteString(w, `{"meta":{"name":"Inception"}}`)
				case <-r.Context().Done():
					close(canceled)
				}
			}))
			defer origin.Close()
			defer close(release)
			s := lifecycleServer(t)
			c, _ := ParseContent("movie", "tmdb:27205")
			first := s.newSession(c, Stream{})
			playing := s.newSession(c, Stream{})
			first.ContentName, playing.ContentName = "Old fallback", "Old fallback"
			cfg := s.Config.Get()
			cfg.Sources = withMovyDisabled([]Source{{Type: "addon", ManifestURL: origin.URL + "/manifest.json"}})
			done := make(chan struct{})
			go func() {
				s.resolveContentName(c, cfg, []*Session{first, playing})
				close(done)
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("metadata lookup did not start")
			}
			if selected && !s.playerRequest(playing, httptest.NewRequest(http.MethodGet, "/media/selected/master.m3u8", nil)) {
				t.Fatal("could not select playback")
			}
			s.beginLookup()
			fresh := s.newSession(c, Stream{})
			fresh.ContentName = "New lookup"
			if selected {
				release <- struct{}{}
			} else {
				select {
				case <-canceled:
				case <-time.After(3 * time.Second):
					t.Fatal("discarding all results did not cancel metadata reads")
				}
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("metadata lookup did not finish")
			}
			if fresh.ContentName != "New lookup" || first.ContentName != "Old fallback" {
				t.Fatal("old metadata updated fresh or discarded sessions")
			}
			if selected && playing.ContentName != "Inception" {
				t.Fatal("playback selection canceled the selected stream's title lookup")
			}
		})
	}
}
