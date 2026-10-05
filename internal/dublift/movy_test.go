package dublift

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Independent synthetic wire fixture from the Nuvio scraper's tests. It has
// no live credentials or signed media URLs.
const movyWireFixture = "Lui0xaaeiZl8OJ0ERlQOaC4PrMj6dvs0xiWH3pEbMw-44qAWWQRVY-CWofSb-AwOJTbWj-g-G-mhZos8FuASGZXc3prRfMZP_M8blF6eb60pLCVzv8ZyH96_F2UlJyvryTGDCCJXyUrpTiPOzWmINA_IxOptZY0sBVAl9lP0DNMz6JJJVxjXVSCfkdEzhdyHa2bqrOHtwg"

func TestMovyCodecMatchesNuvioFixture(t *testing.T) {
	decoded, err := decodeMovySources(movyWireFixture, "fixture-seed", 125988)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Sources) != 1 || decoded.Sources[0].URL != "https://cdn.example.org/master.m3u8" || decoded.Sources[0].Quality != "Auto HLS" || len(decoded.Subtitles) != 1 || decoded.Subtitles[0].Lang != "EN" {
		t.Fatalf("incorrect wire conversion: %+v", decoded)
	}
	for _, tc := range []struct {
		payload, seed string
		id            uint32
	}{
		{movyWireFixture, "expired-seed", 125988}, {movyWireFixture, "fixture-seed", 603},
		{"!not-base64!", "seed", 603}, {"A", "seed", 603}, {movyWireFixture, "", 125988},
		{strings.Repeat("A", 2800001), "seed", 603},
	} {
		if _, err := decodeMovySources(tc.payload, tc.seed, tc.id); err == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
}

func movyEnvelope(t *testing.T, data any, seed string, id uint32) string {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	b = append([]byte("mvm1"), b...)
	movyCrypt(b, seed, id)
	return base64.RawURLEncoding.EncodeToString(b)
}

func movyPage(id, kind, title string) string {
	return `<script src="/_next/static/chunks/player.js"></script><script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"details":{"tmdbId":` + id + `,"mediaType":` + strconvJSON(kind) + `,"title":` + strconvJSON(title) + `,"titleGerman":"Titel & Mehr","year":2023,"imdbId":"tt1234567","seasons":[{},{}]}}}}</script>`
}

func strconvJSON(value string) string { b, _ := json.Marshal(value); return string(b) }

type movyTestTransport struct {
	origin *url.URL
	next   http.RoundTripper
}

func (transport movyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	prefix := ""
	switch r.URL.Host {
	case "api.wecollege.net":
		prefix = "/movy-api"
	case "subtitles.vidy.st":
		prefix = "/movy-subtitles"
	default:
		if r.URL.Host != transport.origin.Host {
			return nil, errors.New("external access disabled in Movy test")
		}
	}
	clone := r.Clone(r.Context())
	u := *r.URL
	clone.URL = &u
	clone.URL.Scheme, clone.URL.Host = transport.origin.Scheme, transport.origin.Host
	clone.URL.Path = prefix + clone.URL.Path
	return transport.next.RoundTrip(clone)
}

func mockMovyNetwork(n *Network, origin string) {
	u, _ := url.Parse(origin)
	n.Client.Transport = movyTestTransport{u, n.Client.Transport}
}

func TestMovyMovieAndEpisodeRequests(t *testing.T) {
	for _, kind := range []string{"movie", "series"} {
		t.Run(kind, func(t *testing.T) {
			c, _ := ParseContent(kind, map[string]string{"movie": "tmdb:603", "series": "tmdb:603:0:2"}[kind])
			mediaType, path := "movie", "/movie/603"
			if kind == "series" {
				mediaType, path = "tv", "/tv/603/0/2"
			}
			var apiCalls, seeds, registryCalls atomic.Int64
			var origin *httptest.Server
			origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Origin") != origin.URL || r.Header.Get("User-Agent") != userAgent {
					t.Error("Movy request headers lost")
				}
				switch r.URL.Path {
				case path:
					if r.URL.Query().Get("play") != "true" {
						t.Error("missing play query")
					}
					io.WriteString(w, movyPage("603", mediaType, "Silo & More"))
				case "/_next/static/chunks/player.js":
					registryCalls.Add(1)
					io.WriteString(w, `/berlin/sources;/munich/sources;/broken/sources;/berlin/sources`)
				case "/movy-api/seed":
					if r.URL.Query().Get("mediaId") != "603" {
						t.Error("seed uses wrong ID")
					}
					seeds.Add(1)
					io.WriteString(w, `{"seed":"fixture-seed"}`)
				case "/movy-api/berlin/sources", "/movy-api/munich/sources":
					if r.URL.Path == "/movy-api/berlin/sources" && apiCalls.Add(1) == 1 {
						w.WriteHeader(401)
						return
					}
					q := r.URL.Query()
					if q.Get("title") != "Silo%20%26%20More" || q.Get("mediaType") != mediaType || q.Get("year") != "2023" || q.Get("totalSeasons") != "2" || q.Get("enc") != "2" || q.Get("imdbId") != "tt1234567" || q.Get("seed") != "fixture-seed" {
						t.Error("Movy query conversion changed", q)
					}
					if kind == "series" && (q.Get("seasonId") != "0" || q.Get("episodeId") != "2") {
						t.Error("episode identity changed")
					}
					if kind == "movie" && (q.Get("seasonId") != "1" || q.Get("episodeId") != "1") {
						t.Error("movie query defaults changed")
					}
					if r.Header.Get("Referer") != origin.URL+path+"?play=true" {
						t.Error("title referer lost")
					}
					quality, source := "1080p", "/hd.mp4"
					if r.URL.Path == "/movy-api/berlin/sources" {
						quality, source = "4K", "/uhd.mkv"
						if q.Get("altTitle") != "Titel%20%26%20Mehr" {
							t.Error("alternate title encoding changed")
						}
					} else if q.Get("language") != "german" {
						t.Error("Munich language missing")
					}
					data := movyPayload{Sources: []movySource{
						{URL: origin.URL + "/uhd.mkv", Quality: "720p"},
						{URL: origin.URL + source, Quality: quality},
						{URL: origin.URL + "/sd.mp4", Quality: "SD"},
					}, Subtitles: []movySubtitle{{URL: origin.URL + "/en.vtt", Lang: "EN"}}}
					// Both raw and JSON-string envelopes occur upstream.
					json.NewEncoder(w).Encode(movyEnvelope(t, data, "fixture-seed", 603))
				case "/movy-api/broken/sources":
					w.WriteHeader(500)
				case "/movy-subtitles/search":
					q := r.URL.Query()
					if q.Get("id") != "603" || kind == "series" && (q.Get("season") != "0" || q.Get("episode") != "2") {
						t.Error("subtitle identity changed")
					}
					json.NewEncoder(w).Encode([]movySubtitle{{URL: origin.URL + "/en.vtt", Lang: "en"}, {URL: origin.URL + "/it.vtt", Language: "Italian"}})
				default:
					t.Error("discovery read media", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer origin.Close()
			n := NewNetwork()
			mockMovyNetwork(n, origin.URL)
			streams, err := n.ResolveMovy(context.Background(), origin.URL, c, "603")
			if err != nil || len(streams) != 2 {
				t.Fatal("movie/episode discovery failed", len(streams), err)
			}
			if !strings.Contains(streams[0].Name, "2160p") || !strings.Contains(streams[1].Name, "1080p") || !strings.HasPrefix(streams[0].Title, "Silo & More\n") {
				t.Fatal("quality order/title lost", streams)
			}
			if seeds.Load() < 2 {
				t.Fatal("401 did not refresh seed")
			}
			if len(streams[0].Subtitles) != 2 || streams[0].Subtitles[0].Lang != "en" || streams[0].Subtitles[1].Lang != "it" {
				t.Fatal("subtitle merge changed", streams[0].Subtitles)
			}
			if _, err := n.ResolveMovy(context.Background(), origin.URL+"/", c, "603"); err != nil || registryCalls.Load() != 1 {
				t.Fatal("registry cache missed", err, registryCalls.Load())
			}
		})
	}
}

func TestMovyPageRejectsWrongTitle(t *testing.T) {
	for _, data := range []string{movyPage("604", "movie", "Other"), movyPage("603", "tv", "Other"), movyPage("603", "movie", ""), `<script id="__NEXT_DATA__">invalid</script>`, `no player`} {
		if _, err := parseMovyPage([]byte(data), "movie", "603"); err == nil {
			t.Fatal("mismatched metadata accepted")
		}
	}
}

func TestMovyMinimumQualityResults(t *testing.T) {
	for _, tc := range []struct {
		name      string
		qualities []string
		want      []string
	}{
		{"mixed qualities", []string{"360p", "480p", "HD", "720p", "1079p", "1079", "1080p", "FHD", "4K"}, []string{"2160p", "1080p", "1080p"}},
		{"only low qualities", []string{"SD", "720p", "1079p"}, nil},
		{"auto fallback with low quality", []string{"720p", "Auto HLS"}, []string{"Auto quality"}},
		{"auto fallback with 1080p", []string{"Auto HLS", "1080p"}, []string{"1080p", "Auto quality"}},
		{"auto fallback with both target qualities", []string{"Auto HLS", "1080p", "4K"}, []string{"2160p", "1080p", "Auto quality"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var origin *httptest.Server
			origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/movie/603":
					io.WriteString(w, movyPage("603", "movie", "Fixture"))
				case "/_next/static/chunks/player.js":
					io.WriteString(w, `/seattle/sources`)
				case "/movy-api/seed":
					io.WriteString(w, `{"seed":"fixture-seed"}`)
				case "/movy-subtitles/search":
					io.WriteString(w, `[]`)
				case "/movy-api/seattle/sources":
					payload := movyPayload{Sources: []movySource{}}
					for i, quality := range tc.qualities {
						payload.Sources = append(payload.Sources, movySource{URL: fmt.Sprintf("%s/source-%d.mp4", origin.URL, i), Quality: quality})
					}
					io.WriteString(w, movyEnvelope(t, payload, "fixture-seed", 603))
				default:
					t.Error("quality filtering probed media", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer origin.Close()
			n := NewNetwork()
			mockMovyNetwork(n, origin.URL)
			streams, err := n.ResolveMovy(context.Background(), origin.URL, Content{Type: "movie"}, "603")
			if err != nil || len(streams) != len(tc.want) {
				t.Fatal("incorrect minimum-quality results", len(streams), err)
			}
			for i, quality := range tc.want {
				if !strings.Contains(streams[i].Name, "Movy · "+quality+" · ") {
					t.Fatal("incorrect surviving quality/order", streams[i].Name)
				}
			}
		})
	}
}

func TestMovyExtensionlessQualitiesPreserveMasterAndTicket(t *testing.T) {
	var mediaReads atomic.Int64
	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/playlist":
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"en\",NAME=\"English\",LANGUAGE=\"en\",URI=\"en.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=14000000,RESOLUTION=3840x2160,AUDIO=\"en\"\nuhd.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=4000000,AVERAGE-BANDWIDTH=3500000,RESOLUTION=1920x1080,AUDIO=\"en\"\nhd.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,AUDIO=\"en\"\nlow.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=854x480,AUDIO=\"en\"\nsd.m3u8\n")
		case "/hd.m3u8", "/uhd.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
		case "/segment.ts":
			mediaReads.Add(1)
			if r.Header.Get("Cookie") != "session=fixture" || r.Header.Get("Referer") != "https://referer.test/" {
				t.Error("source headers lost")
			}
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(make([]byte, 512))
		default:
			t.Error("unexpected source read", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer origin.Close()
	s := lifecycleServer(t)
	choices := s.Net.normalizeMovySources(context.Background(), movyPayload{Sources: []movySource{
		{URL: origin.URL + "/playlist", Headers: map[string]string{"Cookie": "session=fixture", "Referer": "https://referer.test/", "Origin": "bad\r\nheader", "Authorization": "discard"}},
		{URL: origin.URL + "/drm.m3u8", DRM: json.RawMessage(`true`)}, {URL: origin.URL + "/video.mpd", Type: "dash"},
		{URL: origin.URL + "/embed/603"}, {URL: "file:///tmp/video.mp4"},
	}}, "seattle", origin.URL, origin.URL+"/movie/603?play=true")
	if len(choices) != 2 || choices[1].stream.URL != origin.URL+"/playlist" || choices[1].stream.variantURL != origin.URL+"/hd.m3u8" || choices[1].stream.Headers["Origin"] != origin.URL || choices[1].stream.Headers["Authorization"] != "" {
		t.Fatal("quality/header normalization failed", choices)
	}
	if mediaReads.Load() != 0 {
		t.Fatal("discovery fetched media")
	}
	c, _ := ParseContent("movie", "tmdb:603")
	v := s.newSession(c, choices[1].stream)
	other := s.newSession(c, choices[0].stream)
	if v.lookupKey == other.lookupKey {
		t.Fatal("qualities share lookup identity")
	}
	encoded := s.sealPlayback(v)
	s.mu.Lock()
	s.discardLocked(v)
	s.mu.Unlock()
	restored, err := s.restorePlayback(v.ID, encoded)
	if err != nil || restored.stream.variantURL != origin.URL+"/hd.m3u8" {
		t.Fatal("ticket lost selected quality", err)
	}
	asset, master, variant, err := s.inspectSource(context.Background(), restored.stream)
	if err != nil || asset.Origin.URL != origin.URL+"/hd.m3u8" || variant.Audio != "en" || master.Master.Renditions[0].Language != "en" {
		t.Fatal("preparation lost selected quality/English group", err)
	}
	if mediaReads.Load() != 1 {
		t.Fatal("expected only bounded preparation prefix")
	}
}

func TestMovyListingUsesVixAudioAndSourceOrder(t *testing.T) {
	ffmpegAvailable(t)
	for _, italian := range []bool{false, true} {
		t.Run(fmt.Sprintf("italian=%t", italian), func(t *testing.T) {
			var movyPages, mediaReads atomic.Int64
			var origin *httptest.Server
			origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/movie/603":
					movyPages.Add(1)
					io.WriteString(w, movyPage("603", "movie", "Fixture title"))
				case "/_next/static/chunks/player.js":
					io.WriteString(w, `/seattle/sources`)
				case "/movy-api/seed":
					io.WriteString(w, `{"seed":"fixture-seed"}`)
				case "/movy-api/seattle/sources":
					io.WriteString(w, movyEnvelope(t, movyPayload{Sources: []movySource{{URL: origin.URL + "/video.m3u8", Type: "hls", Quality: "1080p"}}}, "fixture-seed", 603))
				case "/movy-subtitles/search":
					io.WriteString(w, `[]`)
				case "/addon/stream/movie/tmdb:603.json":
					fmt.Fprintf(w, `{"streams":[{"name":"Addon","url":%q}]}`, origin.URL+"/addon.mkv")
				case "/api/movie/603":
					io.WriteString(w, `{"src":"/embed"}`)
				case "/embed":
					io.WriteString(w, `window.masterPlaylist={url:'/vix',params:{token:'fixture',expires:9999999999}}`)
				case "/vix.m3u8":
					io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"English\",LANGUAGE=\"en\",URI=\"en.m3u8\"\n")
					if italian {
						io.WriteString(w, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Italian\",LANGUAGE=\"it\",URI=\"it.m3u8\"\n")
					}
					io.WriteString(w, "#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO=\"a\"\nvix-video.m3u8\n")
				case "/en.m3u8", "/it.m3u8":
					io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\naudio.ts\n#EXT-X-ENDLIST\n")
				case "/video.m3u8", "/addon.mkv", "/audio.ts", "/vix-video.m3u8":
					mediaReads.Add(1)
					w.WriteHeader(500)
				default:
					http.NotFound(w, r)
				}
			}))
			defer origin.Close()
			s := lifecycleServer(t)
			mockMovyNetwork(s.Net, origin.URL)
			cfg := s.Config.Get()
			cfg.PublicURL = ""
			cfg.Sources = []Source{{Type: "movy", BaseURL: origin.URL}, {Type: "addon", Name: "Addon", ManifestURL: origin.URL + "/addon/manifest.json"}, {Type: "vixsrc", BaseURL: origin.URL, Disabled: true}}
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			listing := func() []Stream {
				w := httptest.NewRecorder()
				s.ServeHTTP(w, httptest.NewRequest("GET", "/stream/movie/tmdb:603.json", nil))
				var response struct {
					Streams []Stream `json:"streams"`
				}
				if json.Unmarshal(w.Body.Bytes(), &response) != nil || w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				return response.Streams
			}
			streams := listing()
			if len(streams) != 2 || !strings.Contains(streams[0].Name, "Movy") || !strings.Contains(streams[1].Name, "Addon") {
				t.Fatal("source order changed", streams)
			}
			if strings.Contains(streams[0].URL, "/media/") != italian || strings.HasPrefix(streams[0].Name, "🇮🇹 ") != italian {
				t.Fatal("Italian audio eligibility incorrect", streams[0])
			}
			if !italian && (streams[0].URL != origin.URL+"/video.m3u8" || streams[0].Headers["Referer"] != origin.URL+"/movie/603?play=true") {
				t.Fatal("original fallback changed")
			}
			if mediaReads.Load() != 0 {
				t.Fatal("listing probed video or decoded audio")
			}
			if italian {
				s.mu.Lock()
				valid := true
				for _, session := range s.sessions {
					session.mu.Lock()
					if session.SourceName == "Movy" && (session.native != nil || session.listedVix == nil || session.listedEnglish == nil || session.ContentName != "Fixture title") {
						valid = false
					}
					session.mu.Unlock()
				}
				s.mu.Unlock()
				if !valid {
					t.Fatal("Movy bypassed addon audio alignment path")
				}
			}
			cfg = s.Config.Get()
			cfg.Sources[0], cfg.Sources[1] = cfg.Sources[1], cfg.Sources[0]
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			if streams = listing(); !strings.Contains(streams[0].Name, "Addon") {
				t.Fatal("reorder ignored")
			}
			before := movyPages.Load()
			cfg.Sources[1].Disabled = true
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			if streams = listing(); len(streams) != 1 || movyPages.Load() != before {
				t.Fatal("disabled Movy fetched/listed")
			}
		})
	}
}

func TestMovySourceValidation(t *testing.T) {
	for _, sources := range [][]Source{
		{{Type: "vixsrc", BaseURL: "https://vix.test"}},
		{{Type: "vixsrc", BaseURL: "https://vix.test"}, {Type: "movy", BaseURL: "file:///tmp/video"}},
		{{Type: "vixsrc", BaseURL: "https://vix.test"}, {Type: "movy", BaseURL: movyDefaultURL}, {Type: "movy", BaseURL: movyDefaultURL}},
	} {
		if validateSources(sources) == nil {
			t.Fatal("invalid Movy settings accepted")
		}
	}
}

func TestMovyCancellationStopsServerRequests(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movie/603":
			io.WriteString(w, movyPage("603", "movie", "Fixture"))
		case "/_next/static/chunks/player.js":
			io.WriteString(w, `/seattle/sources`)
		case "/movy-api/seed":
			io.WriteString(w, `{"seed":"fixture-seed"}`)
		case "/movy-subtitles/search":
			io.WriteString(w, `[]`)
		case "/movy-api/seattle/sources":
			close(started)
			<-r.Context().Done()
			close(canceled)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	n := NewNetwork()
	mockMovyNetwork(n, origin.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := n.ResolveMovy(ctx, origin.URL, Content{Type: "movie"}, "603"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled discovery did not finish")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("obsolete server request survived")
	}
}

type movyRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn movyRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestMovyQuickScrapeWaitsForQualityWithinBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delays    [4]time.Duration
		qualities [4]string
		want      []string
		elapsed   time.Duration
		canceled  int64
	}{
		{"both qualities early", [4]time.Duration{time.Second, 2 * time.Second, 10 * time.Second, 10 * time.Second}, [4]string{"1080p", "4K"}, []string{"2160p", "1080p"}, 2 * time.Second, 2},
		{"4K after old cutoff", [4]time.Duration{time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second}, [4]string{"1080p", "4K"}, []string{"2160p", "1080p"}, 5 * time.Second, 2},
		{"1080p only", [4]time.Duration{time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second}, [4]string{"1080p"}, []string{"1080p"}, 8 * time.Second, 3},
		{"4K only", [4]time.Duration{time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second}, [4]string{"4K"}, []string{"2160p"}, 8 * time.Second, 3},
		{"auto only", [4]time.Duration{time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second}, [4]string{"Auto HLS"}, []string{"Auto quality"}, 8 * time.Second, 3},
		{"all servers finish early", [4]time.Duration{time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}, [4]string{"1080p"}, []string{"1080p"}, 2 * time.Second, 0},
		{"late first usable result", [4]time.Duration{9 * time.Second, 11 * time.Second, 11 * time.Second, 11 * time.Second}, [4]string{"1080p"}, []string{"1080p"}, 9 * time.Second, 3},
		{"4K beyond budget", [4]time.Duration{time.Second, 9 * time.Second, 10 * time.Second, 10 * time.Second}, [4]string{"1080p", "4K"}, []string{"1080p"}, 8 * time.Second, 3},
		{"low quality cannot trigger cutoff", [4]time.Duration{time.Second, 9 * time.Second, 9 * time.Second, 9 * time.Second}, [4]string{"720p"}, nil, 9 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The fake transport keeps every request inside the virtual clock,
			// exercising real discovery deadlines without slow sleeps or live I/O.
			synctest.Test(t, func(t *testing.T) {
				servers := []string{"seattle", "boise", "miami", "denver"}
				var canceled atomic.Int64
				n := NewNetwork()
				n.Client.Transport = movyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					var body string
					switch r.URL.Host {
					case "movy.test":
						switch r.URL.Path {
						case "/movie/603":
							body = movyPage("603", "movie", "Fixture")
						case "/_next/static/chunks/player.js":
							for _, server := range servers {
								body += "/" + server + "/sources;"
							}
						}
					case "subtitles.vidy.st":
						body = "[]"
					case "api.wecollege.net":
						if r.URL.Path == "/seed" {
							body = `{"seed":"fixture-seed"}`
							break
						}
						for i, server := range servers {
							if r.URL.Path != "/"+server+"/sources" {
								continue
							}
							timer := time.NewTimer(tc.delays[i])
							defer timer.Stop()
							select {
							case <-timer.C:
							case <-r.Context().Done():
								canceled.Add(1)
								return nil, r.Context().Err()
							}
							payload := movyPayload{Sources: []movySource{}}
							if tc.qualities[i] != "" {
								payload.Sources = append(payload.Sources, movySource{URL: "https://media.test/" + server + ".m3u8", Quality: tc.qualities[i]})
							}
							body = movyEnvelope(t, payload, "fixture-seed", 603)
						}
					}
					if body == "" {
						t.Errorf("unexpected discovery request: %s", r.URL)
						return nil, errors.New("unexpected discovery request")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})
				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
				defer cancel()
				started := time.Now()
				streams, err := n.ResolveMovy(ctx, "https://movy.test", Content{Type: "movie"}, "603")
				if err != nil || len(streams) != len(tc.want) {
					t.Fatal("unexpected quick-scrape results", len(streams), err)
				}
				if elapsed := time.Since(started); elapsed != tc.elapsed {
					t.Fatalf("search took %s, want %s", elapsed, tc.elapsed)
				}
				for i, quality := range tc.want {
					if !strings.Contains(streams[i].Name, "Movy · "+quality+" · ") {
						t.Fatal("incorrect surviving quality/order", streams[i].Name)
					}
				}
				synctest.Wait()
				if canceled.Load() != tc.canceled {
					t.Fatalf("canceled %d requests, want %d", canceled.Load(), tc.canceled)
				}
			})
		})
	}
}

func TestMovyTargetQualitiesCancelPendingServers(t *testing.T) {
	var requests atomic.Int64
	allStarted := make(chan struct{})
	canceled := make(chan struct{}, 3)
	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movie/603":
			io.WriteString(w, movyPage("603", "movie", "Fixture"))
		case "/_next/static/chunks/player.js":
			io.WriteString(w, `/seattle/sources;/boise/sources;/miami/sources;/denver/sources;/dallas/sources`)
		case "/movy-api/seed":
			io.WriteString(w, `{"seed":"fixture-seed"}`)
		case "/movy-subtitles/search":
			io.WriteString(w, `[]`)
		default:
			if !strings.HasPrefix(r.URL.Path, "/movy-api/") {
				t.Error("unexpected origin request", r.URL.Path)
				w.WriteHeader(404)
				return
			}
			if requests.Add(1) == 4 {
				close(allStarted)
			}
			if r.URL.Path == "/movy-api/seattle/sources" {
				select {
				case <-allStarted:
				case <-r.Context().Done():
					return
				}
				io.WriteString(w, movyEnvelope(t, movyPayload{Sources: []movySource{
					{URL: origin.URL + "/uhd.mp4", Quality: "4K HDR"},
					{URL: origin.URL + "/hd.mp4", Quality: "FullHD"},
				}}, "fixture-seed", 603))
			} else {
				<-r.Context().Done()
				canceled <- struct{}{}
			}
		}
	}))
	defer origin.Close()
	n := NewNetwork()
	mockMovyNetwork(n, origin.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	streams, err := n.ResolveMovy(ctx, origin.URL, Content{Type: "movie"}, "603")
	if err != nil || len(streams) != 2 {
		t.Fatal("quality stop failed", err, len(streams))
	}
	if requests.Load() != 4 {
		t.Fatal("server concurrency/early stop changed", requests.Load())
	}
	for range 3 {
		select {
		case <-canceled:
		case <-ctx.Done():
			t.Fatal("quick scrape left pending requests running")
		}
	}
}
