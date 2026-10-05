package dublift

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
)

func ffmpegAvailable(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, e := exec.LookPath(name); e != nil {
			t.Skip(name + " required for media integration test")
		}
	}
}

func assertContinuousTransport(t *testing.T, segments ...[]byte) {
	t.Helper()
	last := make(map[uint16]byte)
	for segmentIndex, data := range segments {
		if len(data)%188 != 0 {
			t.Fatalf("audio segment %d is not packet aligned", segmentIndex)
		}
		for i := 0; i < len(data); i += 188 {
			packet := data[i : i+188]
			if packet[0] != 0x47 {
				t.Fatalf("audio segment %d has bad transport sync", segmentIndex)
			}
			pid := uint16(packet[1]&0x1f)<<8 | uint16(packet[2])
			control := packet[3] >> 4 & 3
			if pid == 0x1fff || control == 0 {
				continue
			}
			counter := packet[3] & 15
			if prior, ok := last[pid]; ok {
				expected := prior
				if control&1 != 0 {
					expected++
				}
				if counter != expected&15 {
					t.Fatalf("audio PID %d counter broke at segment %d packet %d: got %d, want %d", pid, segmentIndex, i/188, counter, expected&15)
				}
				if segmentIndex > 0 && control&2 != 0 && packet[4] > 0 && packet[5]&0x80 != 0 {
					t.Fatalf("audio PID %d signaled a discontinuity in segment %d", pid, segmentIndex)
				}
			}
			last[pid] = counter
		}
	}
}
func command(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if e != nil {
		t.Fatalf("%s: %v\n%s", name, e, b)
	}
	return b
}
func writeWAV(t *testing.T, path string, pcm []int16) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)*2))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint32(pcmRate))
	binary.Write(&b, binary.LittleEndian, uint32(pcmRate*2))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)*2))
	binary.Write(&b, binary.LittleEndian, pcm)
	if e := os.WriteFile(path, b.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
}
func fixture(t *testing.T) string {
	t.Helper()
	ffmpegAvailable(t)
	dir := t.TempDir()
	writeWAV(t, filepath.Join(dir, "english.wav"), signalPCM(84))
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-i", filepath.Join(dir, "english.wav"), "-t", "84", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0", "-bf", "2", "-c:a", "aac", "-af", "adelay=2400:all=1", "-metadata:s:a:0", "language=eng", "-movflags", "+faststart", filepath.Join(dir, "source.mp4"))
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "source.mp4"), "-map", "0", "-c", "copy", filepath.Join(dir, "source.mkv"))
	return dir
}
func testEngine(t *testing.T) *Engine {
	t.Helper()
	cfg, e := OpenConfig(filepath.Join(t.TempDir(), "config.json"))
	if e != nil {
		t.Fatal(e)
	}
	engine, e := NewEngine(cfg, NewNetwork())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}
func packetTimes(t *testing.T, path, selector string) (float64, float64) {
	t.Helper()
	b := command(t, "ffprobe", "-v", "error", "-select_streams", selector, "-show_entries", "packet=pts_time,duration_time,flags", "-of", "json", path)
	var v struct {
		Packets []struct {
			PTS      string `json:"pts_time"`
			Duration string `json:"duration_time"`
			Flags    string `json:"flags"`
		} `json:"packets"`
	}
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	if len(v.Packets) == 0 {
		t.Fatal("empty media")
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, p := range v.Packets {
		x, _ := strconv.ParseFloat(p.PTS, 64)
		d, _ := strconv.ParseFloat(p.Duration, 64)
		lo = min(lo, x)
		hi = max(hi, x+d)
	}
	return lo, hi
}

func TestIndexedFileSeekAndCopy(t *testing.T) {
	dir := fixture(t)
	// Real releases often contain chapters. FFmpeg otherwise synthesizes an
	// extra MP4 text track despite -map 0:v:0 -dn, breaking video-only fragments.
	metadata := filepath.Join(dir, "chapters.txt")
	if err := os.WriteFile(metadata, []byte(";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=42000\ntitle=Opening\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=42000\nEND=84000\ntitle=Second half\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command(t, "ffmpeg", "-v", "error", "-i", filepath.Join(dir, "source.mkv"), "-i", metadata, "-map", "0", "-map_chapters", "1", "-c", "copy", filepath.Join(dir, "source.chaptered.mkv"))
	extensions := []string{"mp4", "mkv", "chaptered.mkv"}
	if bytes.Contains(command(t, "ffmpeg", "-hide_banner", "-encoders"), []byte("libx265")) {
		command(t, "ffmpeg", "-v", "error", "-i", filepath.Join(dir, "source.mp4"), "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=1:frame-threads=1:keyint=240:min-keyint=1:scenecut=0:open-gop=1", "-force_key_frames", "0,2,12,18,28,38,48,58,68,78", "-c:a", "copy", filepath.Join(dir, "source.hevc.mkv"))
		extensions = append(extensions, "hevc.mkv")
	} else {
		t.Log("HEVC fixture omitted: optional libx265 encoder unavailable")
	}
	var mu sync.Mutex
	var ranges []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		if r.Header.Get("X-Required") != "yes" {
			http.Error(w, "header lost", 403)
			return
		}
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	defer origin.Close()
	engine := testEngine(t)
	for _, ext := range extensions {
		t.Run(ext, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			a, e := engine.OpenAsset(ctx, Origin{origin.URL + "/source." + ext, http.Header{"X-Required": {"yes"}}}, false)
			if e != nil {
				t.Fatal(e)
			}
			if len(a.Index.Boundaries) < 10 || math.Abs(a.Duration()-84) > .2 {
				t.Fatalf("bad index: %+v", a.Index)
			}
			p, e := engine.Probe(ctx, a)
			if e != nil {
				t.Fatal(e)
			}
			if len(p.Streams) < 2 {
				t.Fatal(p)
			}
			for _, n := range []int{0, 1, 7, 8} {
				start, end := a.Index.Boundaries[n], a.Index.Boundaries[n+1]
				b, e := engine.Video(ctx, a, start, end-start)
				if e != nil {
					t.Fatal(e)
				}
				init, media, e := splitFMP4(b)
				if e != nil {
					t.Fatal(e)
				}
				if len(init) == 0 || len(media) == 0 {
					t.Fatal("empty fragment")
				}
				path := filepath.Join(dir, fmt.Sprintf("out-%s-%d.mp4", ext, n))
				os.WriteFile(path, b, 0600)
				flags := command(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-read_intervals", "%+#1", "-show_entries", "packet=flags", "-of", "default=nw=1", path)
				if !bytes.Contains(flags, []byte("K")) {
					t.Fatal("first video keyframe lost its sync flag", string(flags))
				}
				lo, hi := packetTimes(t, path, "v:0")
				t.Logf("%s segment %d: expected %.3f..%.3f, packets %.3f..%.3f", ext, n, start, end, lo, hi)
				if math.Abs(lo-(start+1)) > .13 || math.Abs(hi-(end+1)) > .15 {
					t.Errorf("fragment timestamp mismatch: %.3f..%.3f vs %.3f..%.3f", lo, hi, start, end)
				}
				if ext == "mkv" && n == 7 {
					// A range that ends early may still make FFmpeg exit with a
					// valid but incomplete MP4. It must never enter the VOD cache.
					var parts fmp4.Parts
					if err := parts.Unmarshal(media); err != nil {
						t.Fatal(err)
					}
					parts[0].Tracks[0].Samples = parts[0].Tracks[0].Samples[:len(parts[0].Tracks[0].Samples)/2]
					var partial seekablebuffer.Buffer
					partial.Write(init)
					if err := parts[0].Marshal(&partial); err != nil {
						t.Fatal(err)
					}
					if _, err := placeFragments(partial.Bytes(), lo, start+1, end+1, true, uint32(n+1)); err == nil || !strings.Contains(err.Error(), "incomplete video segment") {
						t.Fatalf("short remux accepted: %v", err)
					}
				}
				command(t, "ffmpeg", "-v", "error", "-i", path, "-map", "0:v:0", "-f", "null", "-")
			}
			var segments [][]byte
			bounds := a.Index.Boundaries[1:5]
			if err := engine.VideoWindow(ctx, a, bounds, func(n int, data []byte) error {
				segments = append(segments, data)
				return nil
			}); err != nil {
				t.Fatal("sequential video window", err)
			}
			if len(segments) != len(bounds)-1 {
				t.Fatal("video window omitted segments")
			}
			for n, data := range segments {
				path := filepath.Join(dir, fmt.Sprintf("window-%s-%d.mp4", ext, n))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				lo, hi := packetTimes(t, path, "v:0")
				if math.Abs(lo-bounds[n]-1) > .13 || math.Abs(hi-bounds[n+1]-1) > .15 {
					t.Fatalf("window segment %d has wrong packet clock: %.3f..%.3f", n, lo, hi)
				}
				command(t, "ffmpeg", "-v", "error", "-i", path, "-map", "0:v:0", "-f", "null", "-")
			}
			if ext == "mkv" {
				checkProgressiveVideoWindow(t, engine, ctx, a)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range ranges {
		if r == "" {
			t.Fatal("unranged file read")
		}
	}
}

func TestIndexedMKVRemuxKeepsTailAfterOriginStopsServingIt(t *testing.T) {
	dir := fixture(t)
	path := filepath.Join(dir, "source.mkv")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var tailOff atomic.Int64
	var rejectTail atomic.Bool
	var blocked atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw := r.Header.Get("Range"); raw != "" && rejectTail.Load() {
			start, _, err := requestRange(raw, info.Size())
			if err == nil && start >= tailOff.Load() {
				blocked.Add(1)
				http.Error(w, "tail unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	defer origin.Close()
	engine := testEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	asset, err := engine.OpenAsset(ctx, Origin{URL: origin.URL + "/source.mkv"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(asset.File.tail) == 0 || asset.File.tailOff+int64(len(asset.File.tail)) != info.Size() {
		t.Fatal("Matroska tail was not retained")
	}
	tailOff.Store(asset.File.tailOff)
	rejectTail.Store(true)
	bounds := asset.Index.Boundaries[:3]
	count := 0
	if err := engine.VideoWindow(ctx, asset, bounds, func(int, []byte) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 2 || blocked.Load() != 0 {
		t.Fatalf("segments=%d, attempted unavailable tail reads=%d", count, blocked.Load())
	}
}

func TestIndexedMKVDoesNotRequireUnrelatedTailBytes(t *testing.T) {
	dir := fixture(t)
	base, err := os.ReadFile(filepath.Join(dir, "source.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, padding := range []int{1 << 20, 5 << 20} {
		t.Run(fmt.Sprint(padding), func(t *testing.T) {
			name := fmt.Sprintf("padded-%d.mkv", padding)
			if err := os.WriteFile(filepath.Join(dir, name), append(bytes.Clone(base), make([]byte, padding)...), 0600); err != nil {
				t.Fatal(err)
			}
			var rejected atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if raw := r.Header.Get("Range"); raw != "" {
					start, end, err := requestRange(raw, int64(len(base)+padding))
					if err == nil && end >= int64(len(base)) && end-start+1 >= 1<<20 {
						rejected.Add(1)
						http.Error(w, "trailing bytes unavailable", http.StatusServiceUnavailable)
						return
					}
				}
				http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
			}))
			defer origin.Close()
			engine := testEngine(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			asset, err := engine.OpenAsset(ctx, Origin{URL: origin.URL + "/" + name}, false)
			if err != nil || asset == nil || len(asset.Index.Boundaries) < 2 {
				t.Fatalf("valid cues were rejected after a tail read failed: asset=%v, err=%v", asset, err)
			}
			if rejected.Load() == 0 {
				t.Fatal("fixture did not reject an optional tail request")
			}
		})
	}
}

func checkProgressiveVideoWindow(t *testing.T, engine *Engine, ctx context.Context, a *Asset) {
	t.Helper()
	bounds := a.Index.Boundaries[:5]
	u, skip, cleanup, err := engine.job(ctx, a, bounds[0], bounds[len(bounds)-1]+1, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	clock := &packetClock{}
	raw, err := engine.run(ctx, "ffmpeg", videoArgs(u, skip, bounds[0], bounds[len(bounds)-1]), segmentLimit, clock)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "window.mp4")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	published := 0
	w := &videoWindowWriter{bounds: bounds, duration: a.Duration(), clock: &windowClock{packetClock: packetClock{known: true, first: clock.first}}, publish: func(n int, data []byte) error {
		if n != published {
			t.Errorf("out-of-order segment: %d", n)
		}
		published++
		return nil
	}}
	checkedPartial := false
	for off := 0; off < len(raw); {
		end := min(off+997, len(raw)) // Deliberately split MP4 box headers/bodies.
		if _, err := w.Write(raw[off:end]); err != nil {
			t.Fatal(err)
		}
		off = end
		if published == 1 && !checkedPartial {
			if off == len(raw) {
				t.Fatal("first segment waited for the entire window")
			}
			if err := w.flush(true); err == nil {
				t.Fatal("truncated window was accepted as complete")
			}
			checkedPartial = true
		}
	}
	if err := w.flush(true); err != nil {
		t.Fatal(err)
	}
	if !checkedPartial || published != len(bounds)-1 {
		t.Fatal("segments were not published progressively")
	}
}

func TestVirtualHLSEndToEnd(t *testing.T) {
	dir := fixture(t)
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "source.mp4"), "-c", "copy", "-hls_time", "6", "-hls_playlist_type", "vod", "-hls_segment_filename", filepath.Join(dir, "hq-%03d.ts"), filepath.Join(dir, "hq.m3u8"))
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "source.mp4"), "-c", "copy", "-hls_time", "6", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-start_number", "17", "-hls_segment_filename", filepath.Join(dir, "fmp4-%03d.m4s"), filepath.Join(dir, "fmp4.m3u8"))
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "english.wav"), "-c:a", "aac", "-hls_time", "6", "-hls_playlist_type", "vod", "-hls_segment_filename", filepath.Join(dir, "en-%03d.ts"), filepath.Join(dir, "en.m3u8"))
	os.WriteFile(filepath.Join(dir, "sub.m3u8"), []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:84\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:84,\nsub.vtt\n#EXT-X-ENDLIST\n"), 0600)
	os.WriteFile(filepath.Join(dir, "sub.vtt"), []byte("WEBVTT\n\n00:00:42.000 --> 00:00:45.000\nCiao, mondo.\n"), 0600)
	var rejectEarlyEnglish atomic.Bool
	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rejectEarlyEnglish.Load() && r.URL.Path == "/en-000.ts" {
			http.Error(w, "early clip unavailable", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/manifest.json":
			io.WriteString(w, `{"id":"fixture","resources":["stream"],"types":["movie","series"]}`)
		case "/meta/movie/tmdb:603.json", "/meta/series/tmdb:81349.json":
			io.WriteString(w, `{"meta":{"name":"Fixture title"}}`)
		case "/stream/movie/tmdb:603.json", "/stream/series/tmdb:81349:1:1.json":
			fmt.Fprintf(w, `{"streams":[{"name":"Fixture MKV","url":%q},{"name":"Fixture HLS","url":%q},{"name":"Fixture fMP4 HLS","url":%q}]}`, origin.URL+"/source.mkv", origin.URL+"/hq.m3u8", origin.URL+"/fmp4.m3u8")
		case "/api/movie/603", "/api/tv/81349/1/1":
			io.WriteString(w, `{"src":"/embed"}`)
		case "/embed":
			io.WriteString(w, `window.masterPlaylist={url:'/vix',params:{token:'fixture',expires:9999999999}}`)
		case "/vix.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"English\",LANGUAGE=\"en\",URI=\"en.m3u8\",DEFAULT=YES,AUTOSELECT=YES\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Italian\",LANGUAGE=\"it\",URI=\"en.m3u8\",DEFAULT=NO,AUTOSELECT=YES\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"s\",NAME=\"Italian\",LANGUAGE=\"it\",URI=\"sub.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,CODECS=\"avc1.64001f,mp4a.40.2\",AUDIO=\"a\",SUBTITLES=\"s\"\nhq.m3u8\n")
		default:
			http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
		}
	}))
	defer origin.Close()
	cfg, e := OpenConfig(filepath.Join(t.TempDir(), "config.json"))
	if e != nil {
		t.Fatal(e)
	}
	settings := cfg.Get()
	settings.PublicURL = "" // Test clients use the httptest listener, not the host LAN address.
	settings.Sources = []Source{{Type: "addon", Name: "Fixture", ManifestURL: origin.URL + "/manifest.json"}, {Type: "vixsrc", BaseURL: origin.URL, Disabled: true}}
	if e = cfg.Save(settings); e != nil {
		t.Fatal(e)
	}
	server, e := NewServer(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer server.Close()
	local := httptest.NewServer(server)
	defer local.Close()
	var streams struct {
		Streams []Stream `json:"streams"`
	}
	getJSON(t, local.URL+"/stream/movie/tmdb:603.json", &streams)
	if len(streams.Streams) != 3 {
		t.Fatalf("streams: %+v", streams)
	}
	for _, stream := range streams.Streams {
		t.Run(stream.Name, func(t *testing.T) {
			var fresh struct {
				Streams []Stream `json:"streams"`
			}
			getJSON(t, local.URL+"/stream/movie/tmdb:603.json", &fresh)
			for _, candidate := range fresh.Streams {
				if candidate.Name == stream.Name {
					stream = candidate
					break
				}
			}
			resp, e := http.Get(stream.URL)
			if e != nil {
				t.Fatal(e)
			}
			master, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("master: %s", master)
			}
			if !strings.Contains(string(master), `NAME="Italian · Vixsrc",AUTOSELECT=YES,DEFAULT=YES`) {
				t.Fatalf("Italian not preferred:\n%s", master)
			}
			var parsed playlist.Multivariant
			if err := parsed.Unmarshal(master); err != nil {
				t.Fatal(err)
			}
			if len(parsed.Renditions) == 0 {
				t.Fatalf("master has no renditions:\n%s", master)
			}
			last := parsed.Renditions[len(parsed.Renditions)-1]
			if last.Language != "it" || !last.Default || last.Type != playlist.MultivariantRenditionTypeAudio {
				t.Fatalf("Italian audio is not the last and default rendition:\n%s", master)
			}
			for _, rendition := range parsed.Renditions[:len(parsed.Renditions)-1] {
				if rendition.Type == playlist.MultivariantRenditionTypeAudio && (rendition.Default || rendition.Autoselect) {
					t.Fatalf("another audio rendition can be chosen automatically:\n%s", master)
				}
			}
			id := strings.Split(strings.TrimPrefix(stream.URL, local.URL+"/media/"), "/")[0]
			session := server.getSession(id)
			// Immediate file alignment uses a complete playback window. Start
			// with the segment this simulated player watches after seeking.
			if session.video.File != nil {
				for n := 7; n < 14; n++ {
					getBytes(t, local.URL+"/media/"+id+"/video/"+strconv.Itoa(n)+"/segment.m4s")
				}
			} else {
				video := getBytes(t, local.URL+"/media/"+id+"/video.m3u8")
				var media playlist.Media
				if err := media.Unmarshal(video); err != nil {
					t.Fatal(err)
				}
				getBytes(t, local.URL+media.Segments[7].URI)
			}
			deadline := time.Now().Add(25 * time.Second)
			for time.Now().Before(deadline) {
				session.mu.Lock()
				running := session.Aligning
				session.mu.Unlock()
				if !running {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			alignment := server.Alignments.Get(session.Key)
			t.Logf("alignment %+v", alignment)
			if alignment.Confidence < .68 || math.Abs(alignment.Offset-2.4) > .12 {
				t.Errorf("automatic sync failed: %+v; errors: %+v", alignment, session.Errors)
			}
			// Retrieve only a late segment, without requesting earlier output.
			n := 7
			var it Track
			for _, track := range session.tracks {
				if track.Lang == "it" && !track.Subtitle {
					it = track
					break
				}
			}
			if zero := getBytes(t, local.URL+"/media/"+id+"/track/"+it.ID+"/0.ts"); len(zero) == 0 {
				t.Fatal("empty startup audio")
			}
			audioURL := local.URL + "/media/" + id + "/track/" + it.ID + "/" + strconv.Itoa(n) + ".ts"
			audio := getBytes(t, audioURL)
			nextAudio := getBytes(t, local.URL+"/media/"+id+"/track/"+it.ID+"/"+strconv.Itoa(n+1)+".ts")
			assertContinuousTransport(t, audio, nextAudio)
			seekAudio := getBytes(t, local.URL+"/media/"+id+"/track/"+it.ID+"/"+strconv.Itoa(n+4)+".ts")
			assertInitialTransportReset(t, seekAudio)
			assertContinuousTransport(t, seekAudio, getBytes(t, local.URL+"/media/"+id+"/track/"+it.ID+"/"+strconv.Itoa(n+5)+".ts"))
			path := filepath.Join(dir, "audio-"+id+".ts")
			os.WriteFile(path, audio, 0600)
			lo, hi := packetTimes(t, path, "a:0")
			nextPath := filepath.Join(dir, "audio-next-"+id+".ts")
			os.WriteFile(nextPath, nextAudio, 0600)
			nextLo, _ := packetTimes(t, nextPath, "a:0")
			if gap := nextLo - hi; gap < -.002 || gap > .05 {
				t.Errorf("adjacent AAC segments have a %.3f second gap/overlap", gap)
			}
			want := session.boundaries[n] + session.clockBase
			t.Logf("audio clock %.3f..%.3f expected %.3f", lo, hi, want)
			if math.Abs(lo-want) > .04 {
				t.Error("audio clock does not match video")
			}
			playlist := getBytes(t, local.URL+"/media/"+id+"/video.m3u8")
			if !strings.Contains(string(playlist), "#EXT-X-ENDLIST") {
				t.Fatal("not seekable VOD")
			}
			if session.video.File != nil {
				if strings.Count(string(playlist), "#EXT-X-MAP:") != 1 || !strings.Contains(string(playlist), `#EXT-X-MAP:URI="video/0/init.mp4"`) {
					t.Fatal("file video should use one initialization map")
				}
				getBytes(t, local.URL+"/media/"+id+"/video/7/init.mp4")
				getBytes(t, local.URL+"/media/"+id+"/video/7/segment.m4s")
			}
			session.mu.Lock()
			position := session.Position
			session.mu.Unlock()
			if position < session.boundaries[n] {
				t.Error("current position was not tracked")
			}
			// Emulate a player's late-segment selection for fMP4. FFmpeg 6.1's
			// HLS -ss has upstream bug #7359 even on unmodified fMP4 playlists.
			// A selected window still reads/decode-checks the actual late URLs.
			seeked := filepath.Join(dir, "seeked-"+id+".mkv")
			if strings.Contains(string(playlist), "#EXT-X-MAP:") {
				windowMaster := filepath.Join(dir, "window-"+id+".m3u8")
				for _, rendition := range []struct{ name, route string }{{"video", "video.m3u8"}, {"audio", "track/" + it.ID + ".m3u8"}} {
					url := local.URL + "/media/" + id + "/" + rendition.route
					h, err := ParseHLS(getBytes(t, url), Origin{URL: url})
					if err != nil {
						t.Fatal(err)
					}
					window, _, err := h.Window(session.boundaries[n], 12, func(u string) string { return u })
					if err != nil {
						t.Fatal(err)
					}
					os.WriteFile(filepath.Join(dir, rendition.name+"-"+id+".m3u8"), []byte(window), 0600)
				}
				os.WriteFile(windowMaster, fmt.Appendf(nil, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"Italian\",LANGUAGE=\"it\",DEFAULT=YES,URI=\"audio-%s.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,AUDIO=\"a\"\nvideo-%s.m3u8\n", id, id), 0600)
				command(t, "ffmpeg", "-nostdin", "-v", "error", "-protocol_whitelist", "file,http,tcp,crypto", "-allowed_extensions", "ALL", "-i", windowMaster, "-t", "2", "-map", "0:v:0", "-map", "0:a:m:language:it", "-c", "copy", seeked)
			} else {
				command(t, "ffmpeg", "-nostdin", "-v", "error", "-ss", "42", "-i", stream.URL, "-t", "2", "-map", "0:v:0", "-map", "0:a:m:language:it", "-c", "copy", seeked)
			}
			videoStart, videoEnd := packetTimes(t, seeked, "v:0")
			audioStart, audioEnd := packetTimes(t, seeked, "a:0")
			if videoEnd-videoStart < 1 || audioEnd-audioStart < 1 {
				t.Fatalf("seek did not produce video and Italian audio: video %.3f..%.3f, audio %.3f..%.3f", videoStart, videoEnd, audioStart, audioEnd)
			}
			command(t, "ffmpeg", "-v", "error", "-xerror", "-i", seeked, "-frames:v", "2", "-an", "-f", "null", "-")
			if session.video.File != nil {
				content, e := server.Engine.Audio(context.Background(), it, 42, 24, 2.4, 1)
				if e != nil {
					t.Fatal(e)
				}
				seenPID := map[uint16]bool{}
				for off := 0; off+188 <= len(content); off += 188 {
					packet := content[off : off+188]
					if packet[0] != 0x47 {
						t.Fatal("generated audio lost MPEG-TS packet alignment")
					}
					pid := uint16(packet[1]&0x1f)<<8 | uint16(packet[2])
					if seenPID[pid] {
						continue
					}
					seenPID[pid] = true
					if packet[3]&0x20 == 0 || packet[4] == 0 || packet[5]&0x80 == 0 {
						t.Fatalf("first MPEG-TS packet for PID %d does not mark its counter reset", pid)
					}
				}
				if len(seenPID) < 3 {
					t.Fatalf("incomplete generated audio transport: %d PIDs", len(seenPID))
				}
				encoded := filepath.Join(dir, "content.ts")
				os.WriteFile(encoded, content, 0600)
				pcm := command(t, "ffmpeg", "-v", "error", "-i", encoded, "-ac", "1", "-ar", "11025", "-f", "s16le", "pipe:1")
				decoded := make([]int16, len(pcm)/2)
				for i := range decoded {
					decoded[i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
				}
				lag, score, e := MatchPCM(context.Background(), decoded, signalPCM(84))
				if e != nil || math.Abs(lag-39.6) > .15 || score < .68 {
					t.Fatalf("shifted audio content: lag %.3f score %.3f error %v", lag, score, e)
				}
			}
			// Controls persist to disk and source identity, with same-origin protection.
			if os.Getenv("DUBLIFT_VLC_TESTS") == "1" {
				checkVLCPlaybackAndSeek(t, stream.URL, 54)
			}
			postJSON(t, local.URL+"/api/sessions/"+id+"/offset", `{"offset":-1.25}`, 200)
			if got := server.Alignments.Get(session.Key).At(42, .68); got != -1.25 {
				t.Fatal(got)
			}
			postJSON(t, local.URL+"/api/sessions/"+id+"/offset", `{"offset":null}`, 200)
			if session.video.File != nil {
				getBytes(t, local.URL+"/media/"+id+"/video/7/segment.m4s") // video controls the requested area
				postJSON(t, local.URL+"/api/sessions/"+id+"/realign", `{}`, 202)
				until := time.Now().Add(15 * time.Second)
				for time.Now().Before(until) {
					session.mu.Lock()
					active := session.Aligning
					session.mu.Unlock()
					if !active {
						break
					}
					time.Sleep(25 * time.Millisecond)
				}
				aligned := server.Alignments.Get(session.Key)
				if len(aligned.Boundaries) != 1 || aligned.Boundaries[0].SourceTime != session.boundaries[7] {
					t.Fatalf("realignment boundary missing: %+v", aligned)
				}
				reloaded, e := OpenAlignments(filepath.Join(filepath.Dir(cfg.path), "alignment.json"))
				if e != nil {
					t.Fatal(e)
				}
				if len(reloaded.Get(session.Key).Boundaries) != 1 {
					t.Fatal("realignment was not persisted")
				}
			}
		})
	}
	t.Run("MKV_first_sample_from_zero_before_playback", func(t *testing.T) {
		gated := cfg.Get()
		gated.AlignmentSampleSeconds = 8 // changing length must start a fresh run
		if err := cfg.Save(gated); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := cfg.Save(settings); err != nil {
				t.Error(err)
			}
		}()
		var fresh struct {
			Streams []Stream `json:"streams"`
		}
		getJSON(t, local.URL+"/stream/movie/tmdb:603.json", &fresh)
		id := strings.Split(strings.TrimPrefix(fresh.Streams[0].URL, local.URL+"/media/"), "/")[0]
		resp, err := http.Post(local.URL+"/api/sessions/"+id+"/prepare", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Contains(body, []byte(`"aligned":true`)) {
			t.Fatalf("manual preparation: %s", body)
		}
		session := server.getSession(id)
		if session.Playing || session.Aligning {
			t.Fatal("manual preparation did not finish before playback")
		}
		alignment := server.Alignments.Get(session.Key)
		if alignment.Confidence < .68 || math.Abs(server.playbackOffset(session, 0)-2.4) > .12 {
			t.Fatal("prepared offset unavailable", alignment)
		}
		master := getBytes(t, fresh.Streams[0].URL)
		if !bytes.Contains(master, []byte("Italian · Vixsrc")) {
			t.Fatal("Italian master missing")
		}
		if server.playbackOffset(session, 0) != alignment.Offset {
			t.Fatal("playback lost prepared synchronization")
		}
		if len(alignment.AutoSamples) == 0 || alignment.AutoWindow != 8 || math.Abs(alignment.AutoSamples[0].SourceTime-14) > .15 {
			t.Fatalf("first source sample did not start at 00:00: %+v; errors: %+v", alignment, session.Errors)
		}
	})
	t.Run("failed_preparation_aligns_after_playback_seeks", func(t *testing.T) {
		cfgValue := cfg.Get()
		cfgValue.AlignmentSampleSeconds = 8
		if err := cfg.Save(cfgValue); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := cfg.Save(settings); err != nil {
				t.Error(err)
			}
		}()
		var fresh struct {
			Streams []Stream `json:"streams"`
		}
		getJSON(t, local.URL+"/stream/movie/tmdb:603.json", &fresh)
		id := strings.Split(strings.TrimPrefix(fresh.Streams[0].URL, local.URL+"/media/"), "/")[0]
		// Force reanalysis even if a preceding subtest aligned this edition.
		session := server.getSession(id)
		if err := server.prepareSession(context.Background(), session); err != nil {
			t.Fatal(err)
		}
		if err := server.awaitPreparedAlignment(context.Background(), session); err != nil {
			t.Fatal(err)
		}
		rejectEarlyEnglish.Store(true)
		defer rejectEarlyEnglish.Store(false)
		server.Engine.Cache.Clear()
		if !server.startAlignment(session, nil) {
			t.Fatal("manual reanalysis did not start")
		}
		resp, err := http.Post(local.URL+"/api/sessions/"+id+"/prepare", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Contains(body, []byte(`"aligned":false`)) {
			t.Fatalf("failed manual alignment was reported successful: %s", body)
		}
		getBytes(t, fresh.Streams[0].URL)
		for n := 7; n < 14; n++ {
			getBytes(t, local.URL+"/media/"+id+"/video/"+strconv.Itoa(n)+"/segment.m4s")
		}
		if err := server.awaitPreparedAlignment(context.Background(), session); err != nil {
			t.Fatal(err)
		}
		aligned := server.Alignments.Get(session.Key)
		if aligned.Confidence < .68 || math.Abs(aligned.Offset-2.4) > .12 || len(aligned.AutoSamples) != 1 || aligned.AutoSamples[0].SourceTime < 42 {
			t.Fatalf("playback retry did not align at the seeked area: %+v; errors: %+v", aligned, session.Errors)
		}
	})
	var series struct {
		Streams []Stream `json:"streams"`
	}
	var repeated struct {
		Streams []Stream `json:"streams"`
	}
	getJSON(t, local.URL+"/stream/movie/tmdb:603.json", &repeated)
	if repeated.Streams[0].URL == streams.Streams[0].URL {
		t.Fatal("new lookup retained old session links")
	}
	getJSON(t, local.URL+"/stream/series/tmdb:81349:1:1.json", &series)
	if len(series.Streams) != 3 {
		t.Fatal("episode streams missing")
	}
	request, _ := http.NewRequest("GET", local.URL+"/api/settings", nil)
	request.Header.Set("Origin", "https://evil.example")
	resp, e := http.DefaultClient.Do(request)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("private settings exposed cross-origin")
	}
}

func assertInitialTransportReset(t *testing.T, data []byte) {
	t.Helper()
	seen := make(map[uint16]bool)
	for i := 0; i+188 <= len(data); i += 188 {
		packet := data[i : i+188]
		pid := uint16(packet[1]&0x1f)<<8 | uint16(packet[2])
		if pid == 0x1fff || seen[pid] {
			continue
		}
		seen[pid] = true
		if packet[3]&0x20 == 0 || packet[4] == 0 || packet[5]&0x80 == 0 {
			t.Fatalf("audio PID %d did not mark a seek discontinuity", pid)
		}
	}
	if len(seen) < 3 {
		t.Fatalf("incomplete transport after seek: %d PIDs", len(seen))
	}
}
func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	b := getBytes(t, url)
	if e := json.Unmarshal(b, v); e != nil {
		t.Fatalf("JSON: %v: %s", e, b)
	}
}
func getBytes(t *testing.T, url string) []byte {
	t.Helper()
	resp, e := http.Get(url)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
	}
	return b
}
func postJSON(t *testing.T, url, body string, status int) {
	t.Helper()
	resp, e := http.Post(url, "application/json", strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
	}
}
