package dublift

import (
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

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

func TestVixUnknownAudioRenditions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		audio      string
		wantLangs  string
		wantID     string
		wantNative bool
	}{
		{"missing language", `NAME="Audio"`, "und", "vix-und-1", true},
		{"undefined language", `NAME="Audio",LANGUAGE="und"`, "und", "vix-und-1", true},
		{"unknown language", `NAME="Audio",LANGUAGE="Unknown"`, "und", "vix-und-1", true},
		{"whitespace language", `NAME="Audio",LANGUAGE="  "`, "und", "vix-und-1", true},
		{"Italian name", `NAME="Italian",LANGUAGE="und"`, "it", "vix-it-1", true},
		{"Italian tag", `NAME="Audio",LANGUAGE="ita"`, "it", "vix-it-1", true},
		{"English only", `NAME="English",LANGUAGE="en"`, "", "", false},
		{"German only", `NAME="Deutsch",LANGUAGE="de"`, "", "", false},
		{"all unknown tracks retained", "NAME=\"First\",LANGUAGE=\"und\",DEFAULT=NO,URI=\"first.m3u8\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Second\",DEFAULT=YES", "und,und", "vix-und-1", true},
		{"Italian beats unknown", "NAME=\"First\",LANGUAGE=\"und\",DEFAULT=YES,URI=\"first.m3u8\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Italian\",LANGUAGE=\"it\",DEFAULT=NO", "und,it", "vix-it-2", true},
		{"English before unknown", "NAME=\"English\",LANGUAGE=\"en\",DEFAULT=YES,URI=\"english.m3u8\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Unknown\",LANGUAGE=\"und\",DEFAULT=NO", "en,und", "vix-und-2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mediaReads atomic.Int64
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ".m3u8") {
					io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
				} else if r.URL.Path == "/sub.vtt" {
					io.WriteString(w, "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nCiao\n")
				} else {
					mediaReads.Add(1)
					http.Error(w, "listing must not decode separate audio", 500)
				}
			}))
			defer origin.Close()
			// Italian subtitles cannot make an English/German-only result eligible.
			raw := "#EXTM3U\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"Italian\",LANGUAGE=\"it\",URI=\"sub.m3u8\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\"," + tc.audio + ",URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,AUDIO=\"a\",SUBTITLES=\"subs\"\nvideo.m3u8\n"
			// The subtitle loader also supports standalone WebVTT files.
			raw = strings.Replace(raw, `URI="sub.m3u8"`, `URI="sub.vtt"`, 1)
			h, err := ParseHLS([]byte(raw), Origin{URL: origin.URL + "/master.m3u8"})
			if err != nil {
				t.Fatal(err)
			}
			s := lifecycleServer(t)
			v := s.newSession(Content{Type: "series", ID: "tmdb:64464:1:1"}, Stream{})
			v.video = &Asset{}
			v.tracks = []Track{{ID: "source", Lang: "en", Original: true}}
			err = s.vixTracks(context.Background(), v, h)
			if (err == nil) != (tc.wantID != "") {
				t.Fatal("audio eligibility:", err)
			}
			var langs []string
			for _, track := range v.tracks {
				if !track.Subtitle && !track.Original {
					langs = append(langs, track.Lang)
					if track.Lang == "und" && track.Name != "Unknown · Vixsrc" {
						t.Fatal("unknown audio was mislabelled", track.Name)
					}
				}
			}
			if strings.Join(langs, ",") != tc.wantLangs || preferredVixAudio(v.tracks) != tc.wantID {
				t.Fatal("incorrect tracks or default", langs, preferredVixAudio(v.tracks))
			}
			if strings.Contains(tc.wantLangs, "en") != (v.vixEnglish != nil) || (v.vixEnglish != nil && v.vixEnglish.Lang != "en") {
				t.Fatal("English alignment reference does not match discovered audio")
			}
			if tc.wantID == "" && len(v.tracks) != 1 {
				t.Fatal("failed discovery added Vixsrc tracks")
			}
			qualities := vixQualityStreams(h, v.tracks)
			if len(qualities) != 1 || qualities[0].native.Italian != tc.wantNative {
				t.Fatal("native Italian flag does not match audio eligibility")
			}
			b, err := s.nativePlaylist(v, h, "", true, false, 0)
			if err != nil {
				t.Fatal(err)
			}
			native, err := ParseHLS(b, Origin{URL: "http://local.test/master.m3u8"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantID != "" {
				wanted := h.Master.Renditions[1]
				if strings.HasSuffix(tc.wantID, "-2") {
					wanted = h.Master.Renditions[2]
				}
				for i, r := range native.Master.Renditions {
					if r.Type == playlist.MultivariantRenditionTypeAudio {
						if r.Default != (h.Master.Renditions[i] == wanted) {
							t.Fatalf("native default incorrect:\n%s", b)
						}
						if vixLanguage(h.Master.Renditions[i].Language, h.Master.Renditions[i].Name) == "und" && !strings.HasPrefix(r.Name, "Unknown · Vixsrc") {
							t.Fatal("native unknown track was mislabelled", r.Name)
						}
					}
				}
				b, err = s.master(v)
				if err != nil {
					t.Fatal(err)
				}
				var m playlist.Multivariant
				if err := m.Unmarshal(b); err != nil {
					t.Fatal(err)
				}
				last := m.Renditions[len(m.Renditions)-1]
				if !last.Default || !last.Autoselect || last.URI == nil || *last.URI != "track/"+tc.wantID+".m3u8" {
					t.Fatalf("preferred audio is not last and default:\n%s", b)
				}
				for _, r := range m.Renditions[:len(m.Renditions)-1] {
					if r.Type == playlist.MultivariantRenditionTypeAudio && (r.Default || r.Autoselect) {
						t.Fatalf("another audio track can be selected automatically:\n%s", b)
					}
				}
			}
			if mediaReads.Load() != 0 {
				t.Fatal("separate renditions were decoded during discovery")
			}
		})
	}
}

func TestVixEmbeddedAudioLanguages(t *testing.T) {
	ffmpegAvailable(t)
	for _, tc := range []struct {
		name      string
		languages []string
		wantLangs string
		wantID    string
	}{
		{"English only", []string{"eng"}, "", ""},
		{"German only", []string{"deu"}, "", ""},
		{"multiple unknown", []string{"eng", "und", "und"}, "en,und,und", "vix-embedded-und-1"},
		{"Italian beats unknown", []string{"und", "ita"}, "und,it", "vix-embedded-it-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000"}
			for i, lang := range tc.languages {
				args = append(args, "-map", "0:a:0", fmt.Sprintf("-metadata:s:a:%d", i), "language="+lang)
			}
			args = append(args, "-t", "2", "-c:a", "aac", "-hls_time", "2", "-hls_playlist_type", "vod", filepath.Join(dir, "audio.m3u8"))
			command(t, "ffmpeg", args...)
			origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
			defer origin.Close()
			s := lifecycleServer(t)
			for _, kind := range []string{"master", "media", "mixed"} {
				var h *HLS
				var err error
				if kind == "media" {
					h, err = s.Net.LoadHLS(context.Background(), Origin{URL: origin.URL + "/audio.m3u8"})
				} else {
					raw := "#EXTM3U\n"
					if kind == "mixed" {
						raw += "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"English\",LANGUAGE=\"en\",URI=\"audio.m3u8\"\n"
						raw += fmt.Sprintf("#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Embedded\",LANGUAGE=%q\n", tc.languages[len(tc.languages)-1])
					}
					raw += "#EXT-X-STREAM-INF:BANDWIDTH=500000"
					if kind == "mixed" {
						raw += ",AUDIO=\"a\""
					}
					raw += "\naudio.m3u8\n"
					h, err = ParseHLS([]byte(raw), Origin{URL: origin.URL + "/master.m3u8"})
				}
				if err != nil {
					t.Fatal(err)
				}
				v := &Session{}
				err = s.vixTracks(context.Background(), v, h)
				if (err == nil) != (tc.wantID != "") {
					t.Fatalf("%s embedded audio eligibility: %v", kind, err)
				}
				var langs []string
				for _, track := range v.tracks {
					langs = append(langs, track.Lang)
				}
				wantLangs := tc.wantLangs
				if kind == "mixed" && tc.wantID != "" {
					wantLangs += ",en"
					if v.vixEnglish == nil || v.vixEnglish.ID != "vix-en-0" {
						t.Fatal("external English alignment reference was lost")
					}
				}
				if strings.Join(langs, ",") != wantLangs || preferredVixAudio(v.tracks) != tc.wantID {
					t.Fatal("embedded tracks or default incorrect", kind, langs, preferredVixAudio(v.tracks))
				}
				if native := vixQualityStreams(h, v.tracks)[0].native; native.Italian != (tc.wantID != "") {
					t.Fatal("embedded audio flag incorrect")
				}
			}
		})
	}
}

func TestVixMixedAudioProbeFailureKeepsExternal(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/video.m3u8" {
			http.Error(w, "embedded audio unavailable", http.StatusBadGateway)
			return
		}
		io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
	}))
	defer origin.Close()
	h, err := ParseHLS([]byte(`#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="Embedded"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="English",LANGUAGE="en",URI="en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="Italian",LANGUAGE="it",URI="it.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=500000,AUDIO="a"
video.m3u8
`), Origin{URL: origin.URL + "/master.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	s := lifecycleServer(t)
	v := &Session{}
	if err := s.vixTracks(context.Background(), v, h); err != nil {
		t.Fatal("embedded probe failure discarded external audio:", err)
	}
	if len(v.tracks) != 2 || preferredVixAudio(v.tracks) != "vix-it-2" || v.vixEnglish == nil || v.vixEnglish.ID != "vix-en-1" {
		t.Fatal("external audio or English reference was lost")
	}
	if len(v.Errors) != 1 || !strings.Contains(v.Errors[0], "Vixsrc embedded audio") {
		t.Fatal("embedded probe failure was not reported", v.Errors)
	}
}

func TestVixEmbeddedUnknownAudioListingAndPlayback(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x72:rate=10", "-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000", "-t", "4", "-c:v", "libx264", "-preset", "ultrafast", "-g", "20", "-sc_threshold", "0", "-c:a", "aac", "-hls_time", "2", "-hls_playlist_type", "vod", filepath.Join(dir, "muxed.m3u8"))
	if err := os.WriteFile(filepath.Join(dir, "sub.vtt"), []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nCiao\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var sourceReads atomic.Int64
	files := http.FileServer(http.Dir(dir))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tv/64464/1/1":
			io.WriteString(w, `{"src":"/embed"}`)
		case "/embed":
			io.WriteString(w, `window.masterPlaylist={url:'/master',params:{token:'fixture',expires:9999999999}}`)
		case "/master.m3u8":
			// The reported episode lists subtitles, with unlabelled AAC muxed
			// into each video variant and no EXT-X-MEDIA audio rendition.
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"Italian\",LANGUAGE=\"ita\",URI=\"sub.vtt\"\n#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=1280x720,CODECS=\"avc1.42c00a,mp4a.40.2\",SUBTITLES=\"subs\"\nmuxed.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=200000,RESOLUTION=854x480,CODECS=\"avc1.42c00a,mp4a.40.2\",SUBTITLES=\"subs\"\nmuxed.m3u8\n")
		case "/addon/stream/series/tmdb:64464:1:1.json":
			fmt.Fprintf(w, `{"streams":[{"name":"Fixture","url":"http://%s/source.m3u8"}]}`, r.Host)
		case "/source.m3u8":
			sourceReads.Add(1)
			r.URL.Path = "/muxed.m3u8"
			files.ServeHTTP(w, r)
		default:
			files.ServeHTTP(w, r)
		}
	}))
	defer origin.Close()
	s := lifecycleServer(t)
	cfg := s.Config.Get()
	cfg.PublicURL = ""
	cfg.Sources = withMovyDisabled([]Source{{Type: "vixsrc", BaseURL: origin.URL}, {Type: "addon", Name: "Fixture", ManifestURL: origin.URL + "/addon/manifest.json"}})
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(s)
	defer local.Close()
	b := getBytes(t, local.URL+"/stream/series/tmdb:64464:1:1.json")
	var response struct {
		Streams []Stream `json:"streams"`
	}
	if err := json.Unmarshal(b, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Streams) != 3 || sourceReads.Load() != 0 {
		t.Fatal("listing lost streams or probed the addon video", string(b))
	}
	for _, stream := range response.Streams {
		if !strings.HasPrefix(stream.Name, "🇮🇹 ") || !strings.Contains(stream.URL, "/media/") {
			t.Fatal("embedded unknown audio did not enable the Italian flag", stream.Name)
		}
	}
	stream := response.Streams[2]
	b = getBytes(t, stream.URL)
	var m playlist.Multivariant
	if err := m.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	last := m.Renditions[len(m.Renditions)-1]
	if !last.Default || last.Language != "und" || last.Name != "Unknown · Vixsrc" || last.URI == nil {
		t.Fatalf("embedded unknown audio is not labelled and preferred:\n%s", b)
	}
	id := strings.Split(strings.TrimPrefix(stream.URL, local.URL+"/media/"), "/")[0]
	v := s.getSession(id)
	if v.vixEnglish != nil || v.Aligning {
		t.Fatal("unknown audio was used as an English alignment reference")
	}
	audioURL := strings.Split(stream.URL, "master.m3u8")[0] + *last.URI
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-xerror", "-protocol_whitelist", "http,tcp,crypto", "-i", audioURL, "-map", "0:a:0", "-t", "1", "-f", "null", "-")
}
