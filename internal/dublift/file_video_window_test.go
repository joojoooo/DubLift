package dublift

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fileVideoTestSession(t *testing.T, s *Server, url string) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := s.Engine.OpenAsset(ctx, Origin{URL: url}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Index.Boundaries) < 4 {
		t.Fatalf("too few video boundaries: %v", a.Index.Boundaries)
	}
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: url})
	v.video = a
	v.boundaries = a.Index.Boundaries
	v.Duration = a.Duration()
	v.prepare.Do(func() { close(v.ready) })
	return v
}

func TestFileVideoOutsideStalledWindow(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mp4")
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x72:rate=24", "-t", "18", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-sc_threshold", "0", path)
	origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer origin.Close()
	s := lifecycleServer(t)
	v := fileVideoTestSession(t, s, origin.URL+"/source.mp4")

	// This producer never signals completion, just as a stalled origin read
	// would. A request at its end must create useful work without waiting.
	stalledCtx, cancel := context.WithCancel(v.ctx)
	defer cancel()
	stalled := &fileVideoWindow{first: 0, end: 2, ctx: stalledCtx, cancel: cancel, changed: make(chan struct{}), last: -1}
	v.videoWindow = stalled
	ctx, stop := context.WithTimeout(v.ctx, 10*time.Second)
	defer stop()
	data, err := s.fileVideo(ctx, v, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || stalledCtx.Err() != context.Canceled {
		t.Fatal("outside request did not replace stalled window")
	}
	v.mu.Lock()
	first := v.videoWindow.first
	v.mu.Unlock()
	if first != 2 {
		t.Fatalf("active window begins at %d, want 2", first)
	}
}

func TestSharedFileVideoInitBeforeLateSegment(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "source.mp4")
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x72:rate=24", "-t", "18", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-sc_threshold", "0", base)
	mkv := filepath.Join(dir, "source.mkv")
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", base, "-c", "copy", mkv)
	cases := []string{"source.mp4", "source.mkv"}
	encoders := command(t, "ffmpeg", "-hide_banner", "-encoders")
	if bytes.Contains(encoders, []byte("libx265")) {
		command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x72:rate=24", "-t", "18", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=1:frame-threads=1:keyint=48:min-keyint=48:scenecut=0", filepath.Join(dir, "hevc.mkv"))
		command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "hevc.mkv"), "-c", "copy", filepath.Join(dir, "hevc.mp4"))
		cases = append(cases, "hevc.mkv", "hevc.mp4")
	}
	if bytes.Contains(encoders, []byte("libsvtav1")) {
		command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x72:rate=24", "-t", "18", "-c:v", "libsvtav1", "-preset", "12", "-g", "48", filepath.Join(dir, "av1.mkv"))
		command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", filepath.Join(dir, "av1.mkv"), "-c", "copy", filepath.Join(dir, "av1.mp4"))
		cases = append(cases, "av1.mkv", "av1.mp4")
	}
	origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer origin.Close()
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			s := lifecycleServer(t)
			s.videoPrefetch = nil
			v := fileVideoTestSession(t, s, origin.URL+"/"+name)
			playlist := s.generatedPlaylist(v, nil)
			if strings.Count(playlist, "#EXT-X-MAP:") != 1 || !strings.Contains(playlist, `#EXT-X-MAP:URI="video/0/init.mp4"`) {
				t.Fatal("video playlist lost its one shared map")
			}
			get := func(path string) []byte {
				t.Helper()
				w := httptest.NewRecorder()
				s.media(w, httptest.NewRequest(http.MethodGet, "/media/"+v.ID+path, nil))
				if w.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
				}
				return w.Body.Bytes()
			}
			init := get("/video/0/init.mp4")
			if len(init) == 0 || v.videoWindow != nil {
				t.Fatal("shared init started a media remux")
			}
			if _, ok := s.Engine.Cache.Lookup(v.ctx, videoCacheKey(v, 0)); ok {
				t.Fatal("segment zero was remuxed for the shared init")
			}
			const late = 2
			media := get(fmt.Sprintf("/video/%d/segment.m4s", late))
			if v.videoWindow == nil || v.videoWindow.first != late {
				t.Fatal("late segment did not start its own window")
			}
			if !bytes.Equal(init, get("/video/3/init.mp4")) {
				t.Fatal("init URI changed the shared map")
			}
			raw, ok := s.Engine.Cache.Lookup(v.ctx, videoCacheKey(v, late))
			if !ok {
				t.Fatal("late media missing from cache")
			}
			windowInit, _, err := splitFMP4(raw)
			if err != nil {
				t.Fatal(err)
			}
			for _, names := range [][]string{{"moov", "trak", "mdia", "mdhd"}, {"moov", "trak", "mdia", "minf", "stbl", "stsd"}} {
				if !bytes.Equal(mp4Child(init, names...), mp4Child(windowInit, names...)) {
					t.Fatalf("shared init differs from late media at %v", names)
				}
			}
			out := filepath.Join(dir, "played-"+name+".mp4")
			if err := os.WriteFile(out, append(bytes.Clone(init), media...), 0600); err != nil {
				t.Fatal(err)
			}
			lo, hi := packetTimes(t, out, "v:0")
			// HEVC's reordered pictures may present shortly before their
			// keyframe while retaining the source packet clocks.
			if want := v.boundaries[late] + 1; lo < want-.25 || lo > want+.25 || hi < v.boundaries[late+1]+.75 {
				t.Fatalf("late segment clock %.3f..%.3f, want %.3f..%.3f", lo, hi, want, v.boundaries[late+1]+1)
			}
			command(t, "ffmpeg", "-nostdin", "-v", "error", "-i", out, "-map", "0:v:0", "-frames:v", "2", "-f", "null", "-")
		})
	}
}
