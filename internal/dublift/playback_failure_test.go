package dublift

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func assertErrorFrame(t *testing.T, data []byte) {
	t.Helper()
	frame, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	bounds := frame.Bounds()
	white := 0
	for y := bounds.Dy() / 5; y < bounds.Dy()*4/5; y++ {
		for x := bounds.Dx() / 10; x < bounds.Dx()*9/10; x++ {
			r, g, b, _ := frame.At(x, y).RGBA()
			if r > 30000 && g > 30000 && b > 30000 {
				white++
			}
		}
	}
	if white < bounds.Dx()*bounds.Dy()/1000 {
		t.Fatalf("decoded error frame has no visible centered text (%d bright pixels)", white)
	}
}

func TestPreparationFailureProducesPlayableError(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x72:rate=10", "-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-g", "20", "-movflags", "+faststart", filepath.Join(dir, "source.mp4"))
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "source.mp4"), "-c", "copy", filepath.Join(dir, "source.mkv"))
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "source.mp4"), "-c", "copy", "-hls_time", "2", "-hls_playlist_type", "vod", filepath.Join(dir, "video.m3u8"))
	for _, name := range []string{"video.m3u8", "source.mp4", "source.mkv"} {
		for _, stage := range []string{"inspection", "probe"} {
			t.Run(name+"/"+stage, func(t *testing.T) {
				files := http.FileServer(http.Dir(dir))
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stage == "inspection" {
						http.Error(w, "source unavailable", http.StatusForbidden)
						return
					}
					files.ServeHTTP(w, r)
				}))
				defer origin.Close()
				s := lifecycleServer(t)
				if stage == "probe" {
					probe := filepath.Join(t.TempDir(), "ffprobe")
					if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s\\n' \"Cannot read source %{literal} 'quoted' details\" >&2\nexit 1\n"), 0700); err != nil {
						t.Fatal(err)
					}
					cfg := s.Config.Get()
					cfg.FFprobe = probe
					if err := s.Config.Save(cfg); err != nil {
						t.Fatal(err)
					}
				}
				v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: origin.URL + "/" + name})
				local := httptest.NewServer(s)
				defer local.Close()
				base := local.URL + "/media/" + v.ID
				master := getBytes(t, base+"/master.m3u8")
				if !bytes.Contains(master, []byte("error.m3u8")) {
					t.Fatal("preparation failure did not publish the error stream", string(master))
				}
				mediaPlaylist := getBytes(t, base+"/error.m3u8")
				if bytes.Count(mediaPlaylist, []byte("#EXTINF:")) != 1 || !bytes.Contains(mediaPlaylist, []byte("#EXT-X-ENDLIST")) || bytes.Contains(master, []byte("TYPE=AUDIO")) {
					t.Fatal("startup error should be one finite video clip", string(mediaPlaylist))
				}
				frame := command(t, "ffmpeg", "-nostdin", "-v", "error", "-xerror", "-i", base+"/master.m3u8", "-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1")
				assertErrorFrame(t, frame)
				data := append(getBytes(t, base+"/error-init.mp4"), getBytes(t, base+"/error.m4s")...)
				path := filepath.Join(t.TempDir(), "error.mp4")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				var p Probe
				if err := json.Unmarshal(command(t, "ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", path), &p); err != nil {
					t.Fatal(err)
				}
				if len(p.Streams) != 1 || p.Streams[0].CodecName != "h264" || p.Format.Duration != "30.000000" {
					t.Fatalf("unexpected startup clip: %+v", p)
				}
				command(t, "ffmpeg", "-nostdin", "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
				v.mu.Lock()
				failed, published := v.Status == "Preparation failed" && len(v.Errors) > 0, v.masterLoaded
				v.mu.Unlock()
				if !failed || published || v.prepareErr == nil {
					t.Fatal("startup failure was not retained separately from normal publication")
				}
			})
		}
	}
}

func TestFileFailuresAfterStartupReturnHTTPErrorAndAllowRetry(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-t", "18", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-sc_threshold", "0", "-movflags", "+faststart", filepath.Join(dir, "source.mp4"))
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "source.mp4"), "-c", "copy", filepath.Join(dir, "source.mkv"))
	for _, name := range []string{"source.mp4", "source.mkv"} {
		t.Run(name, func(t *testing.T) {
			var fail atomic.Bool
			files := http.FileServer(http.Dir(dir))
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if fail.Load() {
					http.Error(w, "download unavailable", http.StatusForbidden)
					return
				}
				files.ServeHTTP(w, r)
			}))
			defer origin.Close()
			s := lifecycleServer(t)
			s.videoPrefetch = nil
			v := fileVideoTestSession(t, s, origin.URL+"/"+name)
			v.clockBase = 1
			local := httptest.NewServer(s)
			defer local.Close()
			base := local.URL + "/media/" + v.ID
			master := getBytes(t, base+"/master.m3u8")
			init := getBytes(t, base+"/video/0/init.mp4")
			getBytes(t, base+"/video/0/segment.m4s")
			// A seek after eviction must surface the unavailable origin.
			v.mu.Lock()
			window := v.videoWindow
			window.superseded = true
			window.cancel()
			v.videoWindow = nil
			v.mu.Unlock()
			select {
			case <-window.ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("old window did not stop")
			}
			s.Engine.Cache.DeleteOwner(v.ID)
			fail.Store(true)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest("GET", "/media/"+v.ID+"/video/2/segment.m4s", nil))
			if w.Code != http.StatusBadGateway || w.Header().Get("Content-Type") == "video/mp4" || !strings.Contains(w.Body.String(), "403") {
				t.Fatal("failed file download did not return an HTTP error", w.Code, w.Body.String())
			}
			if got := getBytes(t, base+"/video/0/init.mp4"); !bytes.Equal(got, init) {
				t.Fatal("download failure changed the shared initialization map")
			}
			if v.ctx.Err() != nil || v.prepareErr != nil || !bytes.Equal(master, getBytes(t, base+"/master.m3u8")) {
				t.Fatal("media failure canceled the session or changed the master")
			}
			v.mu.Lock()
			if len(v.Errors) == 0 {
				t.Error("media failure missing from dashboard")
			}
			v.mu.Unlock()
			fail.Store(false)
			// Initialization may use indexed metadata without an origin read.
			// Force the encoder itself to fail to test this error path reliably.
			ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
			if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf '%s\\n' 'video remux failed' >&2\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg := s.Config.Get()
			originalFFmpeg := cfg.FFmpeg
			cfg.FFmpeg = ffmpeg
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			v.mu.Lock()
			v.videoInit = nil
			v.mu.Unlock()
			s.Engine.Cache.DeleteOwner(v.ID)
			for _, route := range []string{"video/0/init.mp4", "video/2/segment.m4s"} {
				w := httptest.NewRecorder()
				s.ServeHTTP(w, httptest.NewRequest("GET", "/media/"+v.ID+"/"+route, nil))
				if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "video remux failed") {
					t.Fatal("failed file remux did not return an HTTP error", route, w.Code, w.Body.String())
				}
			}
			cfg.FFmpeg = originalFFmpeg
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			media := getBytes(t, base+"/video/2/segment.m4s")
			path := filepath.Join(t.TempDir(), "seek.mp4")
			if err := os.WriteFile(path, append(init, media...), 0600); err != nil {
				t.Fatal(err)
			}
			lo, hi := packetTimes(t, path, "v:0")
			if lo < v.boundaries[2]+.95 || hi <= lo+1 {
				t.Fatal("retry lost seek timing", lo, hi, v.boundaries)
			}
			command(t, "ffmpeg", "-nostdin", "-v", "error", "-xerror", "-i", path, "-frames:v", "2", "-f", "null", "-")
		})
	}
}

func TestCanceledPreparationRequestDoesNotBecomeFatal(t *testing.T) {
	s := lifecycleServer(t)
	probe := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(probe, []byte(`#!/bin/sh
printf '%s' '{"streams":[{"codec_type":"video","codec_name":"h264"}],"format":{"start_time":"0"}}'
`), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config.Get()
	cfg.FFprobe = probe
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/video.m3u8" {
			close(entered)
			select {
			case <-proceed:
			case <-r.Context().Done():
				return
			}
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
		} else {
			w.Write(bytes.Repeat([]byte{0x47}, 512))
		}
	}))
	defer origin.Close()
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: origin.URL + "/video.m3u8"})
	defer v.cancel()
	v.listedVix = []Track{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("GET", "/media/"+v.ID+"/master.m3u8", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.ServeHTTP(w, r); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("preparation did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled request kept waiting")
	}
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "error.m3u8") || len(v.Errors) != 0 || v.ctx.Err() != nil {
		t.Fatal("request cancellation became a playback failure", w.Code, w.Body.String())
	}
	close(proceed)
	if err := s.prepareSession(context.Background(), v); err != nil {
		t.Fatal("request cancellation stopped shared preparation", err)
	}
	v.note(errors.New("alignment is unavailable; using offset zero"))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/media/"+v.ID+"/master.m3u8", nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "error.m3u8") || !strings.Contains(w.Body.String(), "video.m3u8") {
		t.Fatal("cancellation or alignment warning triggered an error screen", w.Code, w.Body.String())
	}
}

func TestPlaybackErrorTextBoundsAndRedactsDetails(t *testing.T) {
	text := playbackErrorText(errors.New("download https://example.test/private?token=secret failed: " + strings.Repeat("long-message ", 500)))
	if strings.Contains(text, "secret") || strings.Contains(text, "example.test") || len(strings.Split(text, "\n")) > 9 || !strings.Contains(text, "Please try a different source.") {
		t.Fatal("unsafe or unreadable error text", text)
	}
}

func TestPreparationErrorVideoEscapesTempPath(t *testing.T) {
	ffmpegAvailable(t)
	for _, name := range []string{"space path", "colon:path", "quote'path", `back\slash`, "comma,path", "bracket[path]"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", dir)
			e := testEngine(t)
			data, err := e.preparationErrorVideo(context.Background(), errors.New("source unavailable"))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := splitFMP4(data); err != nil {
				t.Fatal(err)
			}
		})
	}
}
