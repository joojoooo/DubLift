package dublift

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

func agreedAlignment() Alignment {
	return Alignment{AutoExpected: 3, AutoComplete: true, AutoSamples: []Anchor{{SourceTime: 30, Offset: .1, Confidence: .9}, {SourceTime: 400, Offset: .09, Confidence: .95}, {SourceTime: 800, Offset: .11, Confidence: .92}}, Offset: .1, Confidence: .9}
}
func TestDirectTolerancePolicy(t *testing.T) {
	cfg := DefaultSettings()
	a := agreedAlignment()
	if ok, why := directAlignment(a, cfg, 1000, 0); !ok {
		t.Fatal(why)
	}
	cases := []struct {
		name   string
		change func(*Alignment)
	}{
		{"not completed", func(a *Alignment) { a.AutoComplete = false }},
		{"one missing", func(a *Alignment) { a.AutoSamples = a.AutoSamples[:2] }},
		{"one low confidence", func(a *Alignment) { a.AutoSamples[1].Confidence = .2 }},
		{"one outlier", func(a *Alignment) { a.AutoSamples[2].Offset = .126 }},
		{"editions differ", func(a *Alignment) { a.DifferentEdit = true }},
		{"early samples only", func(a *Alignment) { a.AutoSamples[2].SourceTime = 600 }},
		{"timeline corrections", func(a *Alignment) { a.Boundaries = []Boundary{{SourceTime: 300, Offset: .05}} }},
		{"manual override", func(a *Alignment) { x := 1.0; a.Manual = &x }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := agreedAlignment()
			tc.change(&a)
			if ok, why := directAlignment(a, cfg, 1000, 0); ok {
				t.Fatalf("unexpected direct mode: %s", why)
			}
		})
	}
	if ok, _ := directAlignment(a, cfg, 1000, .1); ok {
		t.Fatal("ignored incompatible native clock")
	}
	cfg.DirectTolerance = nil
	if ok, why := directAlignment(Alignment{}, cfg, 1000, 900); !ok {
		t.Fatalf("infinite did not bypass alignment: %s", why)
	}
	cfg.DirectPlayback = false
	if ok, _ := directAlignment(a, cfg, 1000, 0); ok {
		t.Fatal("disabled direct audio was used")
	}
}

func TestInfiniteTolerancePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c, e := OpenConfig(path)
	if e != nil {
		t.Fatal(e)
	}
	cfg := c.Get()
	if !cfg.DirectPlayback || cfg.DirectTolerance == nil || *cfg.DirectTolerance != .125 {
		t.Fatal("wrong direct defaults")
	}
	cfg.DirectTolerance = nil
	if e = c.Save(cfg); e != nil {
		t.Fatal(e)
	}
	c, e = OpenConfig(path)
	if e != nil {
		t.Fatal(e)
	}
	if c.Get().DirectTolerance != nil {
		t.Fatal("infinite tolerance did not persist")
	}
	b, e := json.Marshal(c.Get())
	if e != nil || !strings.Contains(string(b), `"directTolerance":null`) {
		t.Fatal(string(b), e)
	}
}

func TestDirectDiscontinuitiesMustAgree(t *testing.T) {
	video := &HLS{Segments: []HLSSegment{{Start: 0}, {Start: 60, Discontinuity: true}}}
	audio := &HLS{Segments: []HLSSegment{{Start: 0}, {Start: 60.02, Discontinuity: true}}}
	if !compatibleDiscontinuities(video, audio) {
		t.Fatal("matching discontinuities were rejected")
	}
	if compatibleDiscontinuities(video, nil) {
		t.Fatal("a clock reset only in video cannot use untouched audio")
	}
	audio.Segments[1].Start = 75
	if compatibleDiscontinuities(video, audio) {
		t.Fatal("mismatched reset positions were accepted")
	}
}

func TestDirectAccessRequiresAnonymousMedia(t *testing.T) {
	needsHeader := true
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media.m3u8" {
			http.Error(w, "playlist requires headers", 403)
			return
		}
		if needsHeader && r.Header.Get("X-Secret") != "yes" {
			http.Error(w, "missing header", 403)
			return
		}
		if r.URL.Path == "/key" {
			w.Write([]byte("0123456789abcdef"))
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write([]byte{0x47, 0, 0, 0})
	}))
	defer origin.Close()
	raw := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-KEY:METHOD=AES-128,URI=\"/key\"\n#EXTINF:6,\none.ts\n#EXT-X-ENDLIST\n"
	h, e := ParseHLS([]byte(raw), Origin{origin.URL + "/media.m3u8", http.Header{"X-Secret": {"yes"}}})
	if e != nil {
		t.Fatal(e)
	}
	a := &Asset{HLS: h, Origin: h.Origin}
	n := NewNetwork()
	n.Client.Jar = nil
	if p := checkDirectAsset(context.Background(), n, a); p.Ready {
		t.Fatal("assumed that configured headers could be sent by the player")
	}
	needsHeader = false
	p := checkDirectAsset(context.Background(), n, a)
	if !p.Ready || p.RemoteManifest {
		t.Fatalf("local finite snapshot should permit direct resources: %+v", p)
	}
	snapshot := directPlaylist(h)
	if !strings.Contains(snapshot, origin.URL+"/key") || !strings.Contains(snapshot, origin.URL+"/one.ts") || strings.Contains(snapshot, "/media/") {
		t.Fatal(snapshot)
	}
}

func TestEncryptedHLSAndDirectPlaybackAfterShutdown(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	writeWAV(t, filepath.Join(dir, "input.wav"), signalPCM(84))
	key := []byte("0123456789abcdef")
	os.WriteFile(filepath.Join(dir, "key.bin"), key, 0600)
	os.WriteFile(filepath.Join(dir, "key-info"), []byte("/key\n"+filepath.Join(dir, "key.bin")+"\n"), 0600)
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "input.wav"), "-c:a", "aac", "-hls_time", "6", "-hls_playlist_type", "vod", "-hls_key_info_file", filepath.Join(dir, "key-info"), "-hls_segment_filename", filepath.Join(dir, "audio-%03d.ts"), filepath.Join(dir, "audio.m3u8"))
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24", "-t", "84", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-sc_threshold", "0", "-hls_time", "6", "-hls_playlist_type", "vod", "-hls_segment_filename", filepath.Join(dir, "video-%03d.ts"), filepath.Join(dir, "video.m3u8"))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/key" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(key)
			return
		}
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	video, e := server.Engine.OpenAsset(ctx, Origin{URL: origin.URL + "/video.m3u8"}, true)
	if e != nil {
		t.Fatal(e)
	}
	audio, e := server.Engine.OpenAsset(ctx, Origin{URL: origin.URL + "/audio.m3u8", Headers: http.Header{"Referer": {"https://unused.example/"}}}, true)
	if e != nil {
		t.Fatal(e)
	}
	track := Track{ID: "vix-it", Name: "Italiano", Lang: "it", Asset: audio, Selector: "0:a:0"}
	pcm, e := server.Engine.PCM(ctx, track, 35, 4)
	if e != nil || len(pcm) < 40000 {
		t.Fatalf("encrypted window failed: %d samples / %v", len(pcm), e)
	}
	v := server.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: video.Origin.URL})
	v.video = video
	v.Duration = 84
	v.tracks = []Track{track}
	v.clockBase = 1.4
	v.vixEnglish = &track
	v.prepare.Do(func() { close(v.ready) })
	server.Alignments.Update(v.Key, func(a *Alignment) {
		*a = Alignment{AutoComplete: true, AutoExpected: 3, AutoSamples: []Anchor{{SourceTime: 20, Confidence: .9}, {SourceTime: 44, Confidence: .92}, {SourceTime: 70, Confidence: .95}}, Confidence: .9}
	})
	local := httptest.NewServer(server)
	server.ensureDirectAccess(ctx, v)
	master := getBytes(t, local.URL+"/media/"+v.ID+"/master.m3u8")
	var parsed playlist.Multivariant
	if e = parsed.Unmarshal(master); e != nil {
		t.Fatal(e)
	}
	if parsed.Variants[0].URI != video.HLS.Origin.URL || parsed.Renditions[0].URI == nil || *parsed.Renditions[0].URI != audio.HLS.Origin.URL {
		t.Fatalf("master retained a local dependency:\n%s", master)
	}
	if d := server.deliveryFor(v); !d.CanStopServer {
		t.Fatalf("not direct: %+v", d)
	}
	local.Close()
	server.Close()
	// The player already has its master. Even a late seek, including an AES
	// key fetch, must work when every DubLift listener has been stopped.
	playerMaster := filepath.Join(dir, "player-master.m3u8")
	os.WriteFile(playerMaster, master, 0600)
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-protocol_whitelist", "file,http,tcp,crypto", "-allowed_extensions", "ALL", "-ss", "48", "-i", playerMaster, "-t", "2", "-map", "0:v:0", "-map", "0:a:0", "-c", "copy", "-f", "null", "-")
}

func TestProxyPreferenceOverridesInfinite(t *testing.T) {
	cfg, e := OpenConfig(filepath.Join(t.TempDir(), "config.json"))
	if e != nil {
		t.Fatal(e)
	}
	value := cfg.Get()
	value.PreferProxy = true
	value.DirectTolerance = nil
	cfg.Save(value)
	server := &Server{Config: cfg}
	d := server.deliveryFor(&Session{})
	if d.VideoDirect || d.AudioDirect || d.CanStopServer {
		t.Fatal("infinite overrode proxy preference")
	}
}

func TestFailedReanalysisClearsStaleAutomaticOffset(t *testing.T) {
	cfg, err := OpenConfig(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	v := server.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: "https://example.test/unavailable.m3u8"})
	// Empty media segments make extraction fail before any network or process.
	v.video = &Asset{ID: "unavailable", HLS: &HLS{Duration: 100}}
	tk := &Track{ID: "english", Asset: v.video, Selector: "0:a:0"}
	v.sourceEnglish, v.vixEnglish = tk, tk
	server.Alignments.Update(v.Key, func(a *Alignment) {
		*a = agreedAlignment()
		a.Offset = 2
	})
	if !server.startAlignment(v, nil) {
		t.Fatal("analysis was not started")
	}
	v.mu.Lock()
	done := v.alignDone
	v.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("failed analysis did not finish")
	}
	a := server.Alignments.Get(v.Key)
	if a.Offset != 0 || a.Confidence != 0 || !a.AutoComplete || len(a.AutoSamples) != 0 || a.At(10, .68) != 0 {
		t.Fatalf("failed reanalysis retained a stale result: %+v", a)
	}
}
