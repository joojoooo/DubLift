package dublift

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMediaProxyHeadersAndRange(t *testing.T) {
	var header string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("X-Required")
		http.ServeContent(w, r, "segment.ts", time.Time{}, strings.NewReader("0123456789abcdef"))
	}))
	defer origin.Close()
	cfg, e := OpenConfig(filepath.Join(t.TempDir(), "config.json"))
	if e != nil {
		t.Fatal(e)
	}
	server, e := NewServer(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer server.Close()
	v := server.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: origin.URL})
	resource := v.addResource(Origin{URL: origin.URL}, 42, true, true)
	request := httptest.NewRequest("GET", resource, nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, request)
	if w.Code != 200 || w.Body.String() != "0123456789abcdef" || w.Header().Get("Location") != "" {
		t.Fatal("video was not proxied", w.Code, w.Body.String())
	}
	if v.Position != 42 {
		t.Fatal("video playback position not tracked")
	}
	if v.videoDownload.bytes.Load() != 16 {
		t.Fatal("proxied video bytes were not counted")
	}
	request = httptest.NewRequest("GET", resource, nil)
	request.Header.Set("Range", "bytes=3-7")
	w = httptest.NewRecorder()
	server.ServeHTTP(w, request)
	if w.Code != 206 || w.Header().Get("Content-Range") != "bytes 3-7/16" || w.Body.String() != "34567" {
		t.Fatalf("range changed: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if v.videoDownload.bytes.Load() != 21 {
		t.Fatalf("proxied video bytes = %d", v.videoDownload.bytes.Load())
	}
	withHeaders := v.addResource(Origin{origin.URL, http.Header{"X-Required": {"forward-me"}}}, 50, true, false)
	request = httptest.NewRequest("GET", withHeaders, nil)
	w = httptest.NewRecorder()
	server.ServeHTTP(w, request)
	if w.Code != 200 || header != "forward-me" {
		t.Fatal("required header was not transparently proxied")
	}
	if v.videoDownload.bytes.Load() != 21 {
		t.Fatal("non-video resource changed video download count")
	}
}

func TestPlaybackPlaylistsKeepMediaOnDubLift(t *testing.T) {
	server := &Server{}
	origin := "https://media.example.test/"
	video := &Asset{ID: "video", HLS: &HLS{Origin: Origin{URL: origin + "video.m3u8"}, Raw: "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n", Segments: []HLSSegment{{Start: 0, Duration: 6}}}}
	audio := &Asset{ID: "audio", HLS: &HLS{Origin: Origin{URL: origin + "audio.m3u8"}, Raw: "#EXTM3U\n#EXTINF:6,\naudio.ts\n#EXT-X-ENDLIST\n", Segments: []HLSSegment{{Start: 0, Duration: 6}}}}
	v := &Session{ID: "session", video: video, tracks: []Track{{ID: "original", Name: "Original audio", Lang: "en", Asset: audio, Original: true}}, resources: map[string]resource{}}
	master, err := server.master(v)
	if err != nil || !strings.Contains(string(master), "video.m3u8") || !strings.Contains(string(master), "track/original.m3u8") || strings.Contains(string(master), origin) {
		t.Fatalf("master exposed an origin: %s (%v)", master, err)
	}
	for _, asset := range []*Asset{video, audio} {
		playlist := server.proxyPlaylist(v, asset, asset == video)
		if strings.Contains(playlist, origin) || !strings.Contains(playlist, "/media/session/resource/") {
			t.Fatalf("media playlist exposed an origin: %s", playlist)
		}
	}
	if len(v.resources) != 4 {
		t.Fatalf("expected local URLs for video, audio, key, and map; got %d", len(v.resources))
	}
}
func TestSubtitleShift(t *testing.T) {
	raw := "WEBVTT\n\n00:00:09.000 --> 00:00:13.000\nCiao.\n"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, raw) }))
	defer origin.Close()
	h, e := ParseHLS([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:20\n#EXTINF:20,\nsub.vtt\n#EXT-X-ENDLIST\n"), Origin{URL: origin.URL + "/sub.m3u8"})
	if e != nil {
		t.Fatal(e)
	}
	server := &Server{Net: NewNetwork()}
	b, e := server.subtitles(context.Background(), &Session{}, Track{Asset: &Asset{HLS: h}}, 10, 6, 2, 1.4)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(b, []byte("00:00:11.000 --> 00:00:15.000")) || !bytes.Contains(b, []byte("MPEGTS:126000")) {
		t.Fatalf("incorrect subtitle shift: %s", b)
	}
}

func TestMappedSubtitlesUseRelativeAudioClockAndCache(t *testing.T) {
	requests := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		io.WriteString(w, "WEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:126000\n\n00:00:09.000 --> 00:00:13.000\nCiao.\n")
	}))
	defer origin.Close()
	h, err := ParseHLS([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:20\n#EXTINF:20,\nsub.vtt\n#EXT-X-ENDLIST\n"), Origin{URL: origin.URL + "/sub.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	cache := NewByteCache(1 << 20)
	ctx := context.Background()
	cache.Get(ctx, "subtitle-clock:english", func() ([]byte, error) { return []byte("1.4"), nil })
	server := &Server{Net: NewNetwork(), Engine: &Engine{Cache: cache}}
	v := &Session{vixEnglish: &Track{Asset: &Asset{ID: "english"}}}
	track := Track{Asset: &Asset{ID: "subtitles", HLS: h}}
	for range 2 {
		b, err := server.subtitles(ctx, v, track, 10, 6, 2, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte("00:00:11.000 --> 00:00:15.000")) || !bytes.Contains(b, []byte("MPEGTS:90000")) {
			t.Fatalf("native subtitle clock counted twice: %s", b)
		}
	}
	if requests != 1 {
		t.Fatalf("subtitle text fetched %d times instead of cached", requests)
	}
}
