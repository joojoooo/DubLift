package dublift

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

func TestNativePlaylistMakesImplicitByteRangesExplicit(t *testing.T) {
	s := lifecycleServer(t)
	h, err := ParseHLS([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:17\n#EXT-X-MAP:URI=\"segment.bin\",BYTERANGE=\"4@0\"\n#EXTINF:6,\n#EXT-X-BYTERANGE:4@4\nsegment.bin\n#EXTINF:6,\n#EXT-X-BYTERANGE:4\nsegment.bin\n#EXT-X-DISCONTINUITY\n#EXTINF:6,\n#EXT-X-BYTERANGE:4\nsegment.bin\n#EXT-X-ENDLIST\n"), Origin{URL: "https://source.test/video.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{})
	b, err := s.nativePlaylist(v, h, "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	local, err := ParseHLS(b, Origin{URL: "http://local.test/master.m3u8"})
	if err != nil {
		t.Fatal("rewritten ranges must remain valid with distinct segment URLs", err)
	}
	for i, segment := range local.Segments {
		if segment.Range != h.Segments[i].Range || segment.Start != h.Segments[i].Start || segment.Sequence != h.Segments[i].Sequence {
			t.Fatal("segment range or clock changed")
		}
		if i > 0 && segment.URI == local.Segments[i-1].URI {
			t.Fatal("segment URLs no longer track playback position")
		}
	}
	if !bytes.Contains(b, []byte(`BYTERANGE="4@0"`)) || !local.Segments[2].Discontinuity {
		t.Fatal("initialization range or discontinuity lost")
	}
}

func TestNativePlaybackRestartRecoversCookiesAndSelectedQuality(t *testing.T) {
	for _, tc := range []struct {
		quality     string
		legacy      bool
		unavailable bool
	}{
		{quality: "720p"},
		{quality: "1 Mbps"},
		{quality: "Auto quality"},
		{quality: "720p", legacy: true},
		{quality: "720p", unavailable: true},
	} {
		t.Run(fmt.Sprintf("%s/legacy=%t/unavailable=%t", tc.quality, tc.legacy, tc.unavailable), func(t *testing.T) {
			var resolutions, wrongProviderReads atomic.Int64
			payload := []byte("native fixture media")
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				provider := "http://" + r.Host + "/private"
				if r.Header.Get("Origin") != provider || r.Header.Get("Referer") != provider+"/" {
					t.Error("original provider headers lost")
					http.Error(w, "missing headers", 403)
					return
				}
				switch r.URL.Path {
				case "/private/api/movie/603":
					resolutions.Add(1)
					io.WriteString(w, `{"src":"/embed"}`)
				case "/embed":
					n := resolutions.Load()
					http.SetCookie(w, &http.Cookie{Name: "source-session", Value: fmt.Sprintf("private-cookie-%d", n), Path: "/"})
					fmt.Fprintf(w, "window.masterPlaylist={url:'/signed/%d/master',params:{token:'private-token-%d',expires:9999999999}}", n, n)
				default:
					n := resolutions.Load()
					cookie, err := r.Cookie("source-session")
					if err != nil || cookie.Value != fmt.Sprintf("private-cookie-%d", n) || !strings.HasPrefix(r.URL.Path, fmt.Sprintf("/signed/%d/", n)) {
						http.Error(w, "missing cookie or expired signature", 403)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/master.m3u8") && tc.quality != "Auto quality" {
						resolution := ",RESOLUTION=1280x720"
						if tc.quality == "1 Mbps" {
							resolution = ""
						}
						fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\n1080.m3u8?token=private-token-%d\n", n)
						if !tc.unavailable || n == 1 {
							fmt.Fprintf(w, "#EXT-X-STREAM-INF:BANDWIDTH=1000000%s\nselected.m3u8?token=private-token-%d\n", resolution, n)
						}
					} else if strings.HasSuffix(r.URL.Path, ".m3u8") {
						io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.bin\n#EXT-X-ENDLIST\n")
					} else {
						http.ServeContent(w, r, "segment.bin", time.Time{}, bytes.NewReader(payload))
					}
				}
			}))
			defer origin.Close()
			wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wrongProviderReads.Add(1)
				http.Error(w, "wrong provider", 500)
			}))
			defer wrong.Close()
			s := lifecycleServer(t)
			cfg := s.Config.Get()
			cfg.PublicURL = ""
			cfg.FFmpeg, cfg.FFprobe = "/missing/ffmpeg", "/missing/ffprobe"
			cfg.Sources = withMovyDisabled([]Source{{Type: "vixsrc", BaseURL: origin.URL + "/private"}})
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			c, _ := ParseContent("movie", "tmdb:603")
			o, err := s.Net.ResolveVix(context.Background(), cfg.Source("vixsrc").BaseURL, c.Type, c.TMDB, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Net.LoadHLS(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			qualities := vixQualityStreams(h)
			selected := qualities[len(qualities)-1]
			v := s.newSession(c, selected.stream)
			v.native, v.nativeMaster = selected.native, h
			if tc.legacy {
				v.native.ProviderURL, v.native.Quality = "", ""
			}
			v.ticket = s.sealPlayback(v)
			ticket, err := s.readPlayback(v.ID, v.ticket)
			if err != nil {
				t.Fatal(err)
			}
			plain, _ := json.Marshal(ticket)
			if bytes.Contains(plain, []byte("source-session")) || bytes.Contains(plain, []byte("private-cookie")) {
				t.Fatal("playback ticket persisted cookies")
			}
			s.Close()
			cfg.Sources[0].BaseURL = wrong.URL
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			restarted, err := NewServer(s.Config)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			local := httptest.NewServer(restarted)
			defer local.Close()
			url := local.URL + "/media/" + v.ID + "/master.m3u8?resume=" + v.ticket
			req, _ := http.NewRequest("HEAD", url, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 || resolutions.Load() != 1 || len(restarted.sessions) != 0 {
				t.Fatal("HEAD restored or resolved playback")
			}
			if tc.unavailable {
				restored, err := restarted.restorePlayback(v.ID, v.ticket)
				if err != nil {
					t.Fatal(err)
				}
				err = restarted.prepareSession(context.Background(), restored)
				if err == nil || !strings.Contains(err.Error(), "selected source quality is no longer available") || strings.Contains(err.Error(), "private-") {
					t.Fatal("unavailable quality must fail without exposing credentials", err)
				}
				return
			}
			data := getBytes(t, url)
			if bytes.Contains(data, []byte("private-token")) || bytes.Contains(data, []byte("private-cookie")) || bytes.Contains(data, []byte(origin.URL)) {
				t.Fatal("native playlist exposed origin credentials")
			}
			if tc.quality != "Auto quality" {
				var master playlist.Multivariant
				if err := master.Unmarshal(data); err != nil || len(master.Variants) != 1 || master.Variants[0].Bandwidth != 1000000 {
					t.Fatal("resume changed selected quality", err)
				}
				data = getBytes(t, local.URL+master.Variants[0].URI)
			}
			media, err := ParseHLS(data, Origin{URL: local.URL + "/master.m3u8"})
			if err != nil || media.Master != nil || len(media.Segments) != 1 {
				t.Fatal("resume did not return the source media playlist", err)
			}
			if !bytes.Equal(getBytes(t, media.Segments[0].URI), payload) || resolutions.Load() != 2 || wrongProviderReads.Load() != 0 {
				t.Fatal("resume did not recover cookies from the original provider")
			}
			u, _ := http.NewRequest("GET", url, nil)
			if strings.Contains(restarted.playbackURL(u, restarted.getSession(v.ID)), "private-") {
				t.Fatal("playback URL exposed credentials")
			}
		})
	}
}

func TestVixNativeQualitiesOrderingAndPlayback(t *testing.T) {
	var videoReads, disabledReads atomic.Int64
	payload := []byte("0123456789abcdef")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first/stream/movie/tmdb:603.json" || r.URL.Path == "/last/stream/movie/tmdb:603.json" {
			name := "first"
			if strings.HasPrefix(r.URL.Path, "/last") {
				name = "last"
			}
			fmt.Fprintf(w, `{"streams":[{"name":%q,"externalUrl":"https://player.test/","custom":true}]}`, name)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/disabled") {
			disabledReads.Add(1)
			http.Error(w, "disabled source was fetched", 500)
			return
		}
		if strings.Contains(r.URL.Path, "/meta/") {
			io.WriteString(w, `{"meta":{"name":"Fixture movie"}}`)
			return
		}
		if r.Header.Get("Referer") == "" || r.Header.Get("Origin") == "" {
			t.Error("source headers lost")
			http.Error(w, "missing headers", 403)
			return
		}
		switch r.URL.Path {
		case "/api/movie/603":
			io.WriteString(w, `{"src":"/embed"}`)
		case "/embed":
			http.SetCookie(w, &http.Cookie{Name: "source-session", Value: "fixture", Path: "/"})
			io.WriteString(w, `window.masterPlaylist={url:'/master',params:{token:'fixture',expires:9999999999}}`)
		case "/master.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"English\",LANGUAGE=\"en\",DEFAULT=YES,URI=\"en.m3u8\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Italian\",LANGUAGE=\"it\",DEFAULT=NO,URI=\"it.m3u8\"\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"Italian\",LANGUAGE=\"it\",URI=\"sub.m3u8\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"unused\",NAME=\"Unused\",URI=\"unused.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,AVERAGE-BANDWIDTH=1500000,RESOLUTION=1280x720,CODECS=\"avc1.640028,mp4a.40.2\",AUDIO=\"a\",SUBTITLES=\"subs\",X-CUSTOM=\"keep\"\n720.m3u8?quality=low\n#EXT-X-STREAM-INF:BANDWIDTH=5000000,AVERAGE-BANDWIDTH=4250000,RESOLUTION=1920x1080,CODECS=\"avc1.640028,mp4a.40.2\",AUDIO=\"a\",SUBTITLES=\"subs\"\n1080.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=4000000,RESOLUTION=1920x1080,CODECS=\"avc1.640028,mp4a.40.2\",AUDIO=\"a\"\nduplicate.m3u8\n")
		case "/720.m3u8", "/1080.m3u8", "/duplicate.m3u8", "/en.m3u8", "/it.m3u8":
			if strings.Contains(r.URL.Path, "720") || strings.Contains(r.URL.Path, "1080") || strings.Contains(r.URL.Path, "duplicate") {
				videoReads.Add(1)
			}
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:17\n#EXT-X-DISCONTINUITY\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0x00000000000000000000000000000000\n#EXT-X-MAP:URI=\"init.mp4\",BYTERANGE=\"4@0\"\n#EXTINF:6,\n#EXT-X-BYTERANGE:8@4\nsegment.bin\n#EXT-X-ENDLIST\n")
		case "/sub.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsub.vtt\n#EXT-X-ENDLIST\n")
		case "/sub.vtt":
			io.WriteString(w, "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nCiao\n")
		case "/segment.bin", "/init.mp4", "/key.bin":
			cookie, err := r.Cookie("source-session")
			if err != nil || cookie.Value != "fixture" {
				t.Error("source cookie lost")
			}
			http.ServeContent(w, r, "segment.bin", time.Time{}, bytes.NewReader(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	s := lifecycleServer(t)
	cfg := s.Config.Get()
	cfg.PublicURL = ""
	cfg.FFmpeg, cfg.FFprobe = "/missing/ffmpeg", "/missing/ffprobe"
	cfg.Sources = withMovyDisabled([]Source{
		{Type: "addon", Name: "First", ManifestURL: origin.URL + "/first/manifest.json"},
		{Type: "vixsrc", BaseURL: origin.URL},
		{Type: "addon", Name: "Disabled", ManifestURL: origin.URL + "/disabled/manifest.json", Disabled: true},
		{Type: "addon", Name: "Last", ManifestURL: origin.URL + "/last/manifest.json"},
	})
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(s)
	defer local.Close()
	listing := func() []Stream {
		t.Helper()
		var response struct {
			Streams []Stream `json:"streams"`
		}
		if err := json.Unmarshal(getBytes(t, local.URL+"/stream/movie/tmdb:603.json"), &response); err != nil {
			t.Fatal(err)
		}
		return response.Streams
	}
	streams := listing()
	if len(streams) != 4 || streams[0].Name != "first" || streams[3].Name != "last" || !strings.Contains(streams[1].Name, "1080p · 4.25 Mbps") || !strings.Contains(streams[2].Name, "720p · 1.5 Mbps") {
		t.Fatalf("unexpected source order/qualities: %d", len(streams))
	}
	if !strings.Contains(streams[2].Title, "1280×720") {
		t.Fatal("missing resolution")
	}
	if videoReads.Load() != 0 || disabledReads.Load() != 0 {
		t.Fatal("listing probed video or queried a hidden source")
	}
	if !strings.Contains(string(streams[0].raw), `"custom":true`) {
		t.Fatal("original addon metadata lost")
	}
	views := s.status(httptest.NewRequest("GET", "/api/status", nil))["sessions"].([]map[string]any)
	for i, source := range []Source{cfg.Sources[0], s.Config.Get().Sources[1], s.Config.Get().Sources[1], cfg.Sources[3]} {
		provider := source.dashboardSource()
		if views[i]["sourceID"] != provider.ID || views[i]["sourceName"] != provider.Name {
			t.Fatalf("stream %d lost its configured source", i)
		}
	}
	// Move VixSrc first, then verify that source ordering persists in the listing.
	cfg.Sources[0], cfg.Sources[1] = cfg.Sources[1], cfg.Sources[0]
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	streams = listing()
	if !strings.Contains(streams[0].Name, "1080p") || streams[2].Name != "first" {
		t.Fatal("reordering did not change source grouping")
	}
	selected := streams[1].URL // Explicitly play 720p, not the highest quality.
	req, _ := http.NewRequest("HEAD", selected, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if videoReads.Load() != 0 {
		t.Fatal("HEAD prepared or selected playback")
	}
	s.mu.Lock()
	count := len(s.sessions)
	s.mu.Unlock()
	if count != 4 {
		t.Fatal("HEAD discarded unselected results")
	}
	masterData := getBytes(t, selected)
	if !strings.Contains(string(masterData), `X-CUSTOM="keep"`) {
		t.Fatal("unknown HLS attribute lost")
	}
	var master playlist.Multivariant
	if err := master.Unmarshal(masterData); err != nil {
		t.Fatal(err)
	}
	if len(master.Variants) != 1 || master.Variants[0].Resolution != "1280x720" || len(master.Renditions) != 3 {
		t.Fatal("selected quality/rendition groups lost")
	}
	for _, r := range master.Renditions {
		if (r.Language == "it" && r.Type == playlist.MultivariantRenditionTypeAudio) != r.Default {
			t.Fatal("Italian audio default not selected")
		}
	}
	proxied := getBytes(t, local.URL+master.Variants[0].URI)
	for _, tag := range []string{"#EXT-X-MEDIA-SEQUENCE:17", "#EXT-X-DISCONTINUITY", "#EXT-X-BYTERANGE:8@4", `BYTERANGE="4@0"`} {
		if !strings.Contains(string(proxied), tag) {
			t.Fatal("source media clock or ranges lost", tag)
		}
	}
	media, err := ParseHLS(proxied, Origin{URL: local.URL + master.Variants[0].URI})
	if err != nil {
		t.Fatal(err)
	}
	rangeReq, _ := http.NewRequest("GET", media.Segments[0].URI, nil)
	rangeReq.Header.Set("Range", "bytes=4-11")
	resp, err = http.DefaultClient.Do(rangeReq)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(data) != "456789ab" || resp.Header.Get("Content-Range") != "bytes 4-11/16" {
		t.Fatal("native playback lost bytes or range response")
	}
	for _, r := range master.Renditions {
		child := getBytes(t, local.URL+*r.URI)
		if bytes.Contains(child, []byte(origin.URL)) {
			t.Fatal("origin HLS resource escaped proxy")
		}
		if r.Type == playlist.MultivariantRenditionTypeSubtitles {
			sub, err := ParseHLS(child, Origin{URL: local.URL + *r.URI})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(getBytes(t, sub.Segments[0].URI)), "Ciao") {
				t.Fatal("subtitle rendition lost")
			}
		}
	}
	// Refreshing keeps active playback, and the encrypted ticket restores the
	// selected quality after its session is discarded.
	listing()
	s.mu.Lock()
	var active *Session
	for _, v := range s.sessions {
		if v.Playing {
			active = v
			break
		}
	}
	if active == nil {
		s.mu.Unlock()
		t.Fatal("refresh discarded active native playback")
	}
	s.discardLocked(active)
	s.mu.Unlock()
	if err := master.Unmarshal(getBytes(t, selected)); err != nil || master.Variants[0].Resolution != "1280x720" {
		t.Fatal("resume lost native playback quality", err)
	}
	// VixSrc remains useful with no addons configured.
	cfg.Sources = withMovyDisabled([]Source{{Type: "vixsrc", BaseURL: origin.URL}})
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if got := listing(); len(got) != 2 {
		t.Fatal("VixSrc-only listing failed", len(got))
	}
	cfg.Sources[0].Disabled = true
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if got := listing(); len(got) != 0 {
		t.Fatal("disabled VixSrc video was listed")
	}
}

func TestVixNativeAutoQualityAndMissingItalian(t *testing.T) {
	for _, raw := range []string{
		"#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"English\",LANGUAGE=\"en\",URI=\"en.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=640x360,AUDIO=\"a\"\n360.m3u8\n",
	} {
		h, err := ParseHLS([]byte(raw), Origin{URL: "https://fixture.test/master.m3u8"})
		if err != nil {
			t.Fatal(err)
		}
		qualities := vixQualityStreams(h)
		if len(qualities) != 1 || qualities[0].native.Italian {
			t.Fatal("native video incorrectly required/claimed Italian audio")
		}
		if h.Master == nil && !strings.Contains(qualities[0].stream.Name, "Auto quality") {
			t.Fatal("invented a resolution for a media playlist")
		}
	}
}

func TestNativeSourcePlaybackDecodes(t *testing.T) {
	ffmpegAvailable(t)
	for _, kind := range []string{"ts", "fmp4"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x72:rate=10", "-t", "4", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-g", "20", "-sc_threshold", "0", "-hls_time", "2", "-hls_playlist_type", "vod"}
			if kind == "fmp4" {
				args = append(args, "-hls_segment_type", "fmp4")
			}
			command(t, "ffmpeg", append(args, filepath.Join(dir, "video.m3u8"))...)
			command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000", "-t", "4", "-c:a", "aac", "-hls_time", "2", "-hls_playlist_type", "vod", filepath.Join(dir, "italian.m3u8"))
			raw := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"Italian\",LANGUAGE=\"it\",DEFAULT=YES,AUTOSELECT=YES,URI=\"italian.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=128x72,CODECS=\"avc1.42c00a,mp4a.40.2\",AUDIO=\"audio\"\nvideo.m3u8\n"
			if err := os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
			defer origin.Close()
			s := lifecycleServer(t)
			cfg := s.Config.Get()
			cfg.PublicURL = ""
			cfg.FFmpeg, cfg.FFprobe = "/missing/ffmpeg", "/missing/ffprobe"
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			h, err := ParseHLS([]byte(raw), Origin{URL: origin.URL + "/master.m3u8"})
			if err != nil {
				t.Fatal(err)
			}
			quality := vixQualityStreams(h)[0]
			v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, quality.stream)
			v.native, v.nativeMaster = quality.native, h
			local := httptest.NewServer(s)
			defer local.Close()
			command(t, "ffmpeg", "-nostdin", "-v", "error", "-xerror", "-protocol_whitelist", "http,tcp,crypto", "-i", local.URL+"/media/"+v.ID+"/master.m3u8", "-map", "0:v:0", "-map", "0:a:0", "-t", "3", "-f", "null", "-")
			v.mu.Lock()
			aligning := v.Aligning
			v.mu.Unlock()
			if aligning {
				t.Fatal("native playback started alignment")
			}
		})
	}
}
