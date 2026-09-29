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
	"testing"
	"time"
)

func ffmpegAvailable(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, e := exec.LookPath(name); e != nil {
			t.Skip(name + " required for media integration test")
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
			for _, n := range []int{0, 7, 8} {
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
				command(t, "ffmpeg", "-v", "error", "-i", path, "-map", "0:v:0", "-f", "null", "-")
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

func TestVirtualHLSEndToEnd(t *testing.T) {
	dir := fixture(t)
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "source.mp4"), "-c", "copy", "-hls_time", "6", "-hls_playlist_type", "vod", "-hls_segment_filename", filepath.Join(dir, "hq-%03d.ts"), filepath.Join(dir, "hq.m3u8"))
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "source.mp4"), "-c", "copy", "-hls_time", "6", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-start_number", "17", "-hls_segment_filename", filepath.Join(dir, "fmp4-%03d.m4s"), filepath.Join(dir, "fmp4.m3u8"))
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "english.wav"), "-c:a", "aac", "-hls_time", "6", "-hls_playlist_type", "vod", "-hls_segment_filename", filepath.Join(dir, "en-%03d.ts"), filepath.Join(dir, "en.m3u8"))
	os.WriteFile(filepath.Join(dir, "sub.m3u8"), []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:84\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:84,\nsub.vtt\n#EXT-X-ENDLIST\n"), 0600)
	os.WriteFile(filepath.Join(dir, "sub.vtt"), []byte("WEBVTT\n\n00:00:42.000 --> 00:00:45.000\nCiao, mondo.\n"), 0600)
	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	settings.VixBaseURL = origin.URL
	settings.Addons = []Addon{{"Fixture", origin.URL + "/manifest.json"}}
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
			if !strings.Contains(string(master), `NAME="Italiano · Vixsrc",AUTOSELECT=YES,DEFAULT=YES`) {
				t.Fatalf("Italian not preferred:\n%s", master)
			}
			id := strings.Split(strings.TrimPrefix(stream.URL, local.URL+"/media/"), "/")[0]
			session := server.getSession(id)
			// Immediate file alignment uses a complete playback window. Start
			// with the segment this simulated player watches after seeking.
			if session.video.File != nil {
				for n := 7; n < 14; n++ {
					getBytes(t, local.URL+"/media/"+id+"/video/"+strconv.Itoa(n)+"/segment.m4s")
				}
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
			audioURL := local.URL + "/media/" + id + "/track/" + it.ID + "/" + strconv.Itoa(n) + ".ts"
			audio := getBytes(t, audioURL)
			path := filepath.Join(dir, "audio-"+id+".ts")
			os.WriteFile(path, audio, 0600)
			lo, hi := packetTimes(t, path, "a:0")
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
			if session.video.File != nil || strings.Contains(session.video.HLS.Raw, "#EXT-X-MAP:") {
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
				getBytes(t, audioURL) // infer the current area from a requested segment
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
		gated.StartImmediately = false
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
		master := getBytes(t, fresh.Streams[0].URL)
		if !bytes.Contains(master, []byte("Italiano · Vixsrc")) {
			t.Fatal("Italian master missing")
		}
		id := strings.Split(strings.TrimPrefix(fresh.Streams[0].URL, local.URL+"/media/"), "/")[0]
		session := server.getSession(id)
		alignment := server.Alignments.Get(session.Key)
		if len(alignment.AutoSamples) == 0 || alignment.AutoWindow != 8 || math.Abs(alignment.AutoSamples[0].SourceTime-14) > .15 {
			t.Fatalf("first source sample did not start at 00:00: %+v; errors: %+v", alignment, session.Errors)
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
