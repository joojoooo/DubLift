package dublift

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVideoWindowOverflowPolicy(t *testing.T) {
	if err := videoWindowOverflow(7, 2, 1, 8); !errors.Is(err, errVideoWindowFull) {
		t.Fatalf("overflow after a published segment = %v, want the partial-window signal", err)
	}
	if err := videoWindowOverflow(7, 2, 0, 8); !errors.Is(err, errVideoWindowNoSegment) {
		t.Fatalf("overflow before any segment is published = %v, want a bounded window failure", err)
	}
	if err := videoWindowOverflow(7, 1, 1, 8); err != nil {
		t.Fatalf("output at the byte budget was rejected: %v", err)
	}
}

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

func TestVideoWindowRetriesMissingTimestampsAfterReadBudget(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x72:rate=24", "-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-sc_threshold", "0", filepath.Join(dir, "source.mp4"))
	origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer origin.Close()
	// Emulate a successful FFmpeg exit after scanning input without finding
	// a marked key packet. The fallback runs the real stream-copy command.
	ffmpeg := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte(`#!/bin/sh
input=
previous=
unmarked=false
for arg do
	if [ "$previous" = -i ]; then input=$arg; fi
	if [ "$arg" = -copyinkf ]; then unmarked=true; fi
	previous=$arg
done
if [ "$unmarked" = true ]; then
	ffmpeg "$@"
else
	ffmpeg -nostdin -v error -i "$input" -map 0:v:0 -c:v copy -f null - >/dev/null 2>&1
fi
exit 0
`), 0700); err != nil {
		t.Fatal(err)
	}
	for _, failFallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback_fails=%t", failFallback), func(t *testing.T) {
			engine := testEngine(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			asset, err := engine.OpenAsset(ctx, Origin{URL: origin.URL + "/source.mp4"}, false)
			if err != nil {
				t.Fatal(err)
			}
			cfg := engine.Config.Get()
			cfg.FFmpeg = ffmpeg
			if err := engine.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			// Exhaust the real input budget without reading a 256 MiB fixture.
			// Each attempt creates a separate job and packet clock.
			seen := map[*mediaJob]bool{}
			input := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				engine.mu.Lock()
				job := engine.jobs[strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]]
				if job != nil {
					seen[job] = true
					if !asset.unmarkedVideo.Load() || failFallback {
						job.budget.Store(0)
					}
				}
				engine.mu.Unlock()
				engine.serveJob(w, r)
			}))
			defer input.Close()
			engine.base = input.URL
			bounds := asset.Index.Boundaries[:3]
			var segments [][]byte
			err = engine.VideoWindow(ctx, asset, bounds, func(_ int, data []byte) error {
				segments = append(segments, data)
				return nil
			})
			engine.mu.Lock()
			attempts := len(seen)
			engine.mu.Unlock()
			if attempts != 2 || !asset.unmarkedVideo.Load() {
				t.Fatalf("missing-keyframe retry: attempts=%d, active=%t, err=%v", attempts, asset.unmarkedVideo.Load(), err)
			}
			if failFallback {
				if err == nil || !strings.Contains(err.Error(), "upstream video download failed: media download exceeds the 256 MiB extraction limit") || len(segments) != 0 {
					t.Fatalf("fallback lost the budget failure: segments=%d, err=%v", len(segments), err)
				}
				return
			}
			if err != nil || len(segments) != len(bounds)-1 {
				t.Fatalf("fallback failed to publish the window: segments=%d, err=%v", len(segments), err)
			}
		})
	}
}

func TestVideoInitSurvivesReadAheadFailure(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mkv")
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=18", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-sc_threshold", "0", path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cutoff atomic.Int64
	cutoff.Store(int64(len(data)))
	var rejected atomic.Int64
	files := http.FileServer(http.Dir(dir))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end, err := requestRange(r.Header.Get("Range"), int64(len(data)))
		limit := cutoff.Load()
		if err == nil && start >= limit {
			rejected.Add(1)
			http.Error(w, "later range unavailable", http.StatusForbidden)
			return
		}
		if err == nil && end >= limit {
			rejected.Add(1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[start:limit])
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer origin.Close()
	engine := testEngine(t)
	asset, err := engine.OpenAsset(context.Background(), Origin{URL: origin.URL + "/source.mkv"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want, err := engine.VideoInit(context.Background(), asset)
	if err != nil {
		t.Fatal(err)
	}
	// FFmpeg may read well past the first packet while building a zero-frame
	// init. The later range fails, but the resulting init remains complete.
	engine.Cache.Resize(0)
	cutoff.Store(3 << 20)
	got, err := engine.VideoInit(context.Background(), asset)
	if rejected.Load() == 0 {
		t.Fatal("fixture did not exercise the failed read-ahead range")
	}
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("valid init was rejected or changed: size=%d, err=%v", len(got), err)
	}
}

func TestFinalVideoSegmentHandlesMatroskaInputFailures(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mkv")
	// The final video GOP ends before the container's audio duration, so it
	// can only be published by the final flush after FFmpeg exits.
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=17", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=18", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-sc_threshold", "0", "-c:a", "aac", path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header, err := ebmlHeader(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	segment, err := ebmlHeader(data[header.end:], header.end)
	if err != nil {
		t.Fatal(err)
	}
	var cueOff int64
	for off := segment.data; off < int64(len(data)); {
		element, err := ebmlHeader(data[off:], off)
		if err != nil || element.end <= off || element.end > int64(len(data)) {
			t.Fatalf("bad Matroska element at %d: %v", off, err)
		}
		if element.id == 0x1c53bb6b {
			cueOff = off
			break
		}
		off = element.end
	}
	if cueOff <= 2<<20 {
		t.Fatal("fixture has no uncached cue range")
	}
	trailingOff := int64(len(data))
	// Cues need not be the last Segment element. A trailing Void exposes a
	// virtual read that begins in Cues but ends outside the validated element.
	trailing := append(bytes.Clone(data), 0xec, 0x83, 0, 0, 0)
	if segment.size >= 0 {
		sizeBytes := int(segment.data - header.end - 4)
		if sizeBytes < 1 || sizeBytes > 8 {
			t.Fatal("invalid Matroska Segment size width")
		}
		segmentSize := uint64(segment.size + 5)
		for i := sizeBytes - 1; i >= 0; i-- {
			trailing[int(header.end)+4+i] = byte(segmentSize)
			segmentSize >>= 8
		}
		trailing[int(header.end)+4] |= byte(1 << (8 - sizeBytes))
	}
	if err := os.WriteFile(filepath.Join(dir, "source-trailing.mkv"), trailing, 0600); err != nil {
		t.Fatal(err)
	}
	files := http.FileServer(http.Dir(dir))
	for _, tc := range []struct {
		name                                      string
		rejectCues, rejectTrailing, truncateMedia bool
		trailingMetadata                          bool
	}{
		{name: "cues_only", rejectCues: true},
		{name: "cues_with_trailing_metadata", rejectCues: true, trailingMetadata: true},
		{name: "trailing_padding_only", rejectTrailing: true, trailingMetadata: true},
		{name: "media_only", truncateMedia: true},
		{name: "cues_then_media", rejectCues: true, truncateMedia: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceData := data
			fileName := "source.mkv"
			if tc.trailingMetadata {
				sourceData = trailing
				fileName = "source-trailing.mkv"
			}
			var fail atomic.Bool
			var rejectedCues, rejectedTrailing, rejectedMedia atomic.Int32
			// Let the final seek read some valid video before the download ends.
			// A final flush alone would accept those truncated media packets.
			cut := cueOff - (1 << 20)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if fail.Load() {
					start, end, err := requestRange(r.Header.Get("Range"), int64(len(sourceData)))
					if err == nil && tc.rejectCues && start >= cueOff {
						rejectedCues.Add(1)
						http.Error(w, "cue range unavailable", http.StatusServiceUnavailable)
						return
					}
					if err == nil && tc.rejectTrailing && end >= trailingOff {
						rejectedTrailing.Add(1)
						if start >= trailingOff {
							http.Error(w, "trailing padding unavailable", http.StatusForbidden)
							return
						}
						w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(sourceData)))
						w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
						w.WriteHeader(http.StatusPartialContent)
						w.Write(sourceData[start:trailingOff])
						return
					}
					if err == nil && tc.truncateMedia && start < cueOff {
						if start >= cut {
							rejectedMedia.Add(1)
							http.Error(w, "media range unavailable", http.StatusForbidden)
							return
						}
						if end >= cut {
							w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(sourceData)))
							w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
							w.WriteHeader(http.StatusPartialContent)
							w.Write(sourceData[start:cut])
							return
						}
					}
				}
				files.ServeHTTP(w, r)
			}))
			defer origin.Close()
			s := lifecycleServer(t)
			s.videoPrefetch = nil
			v := fileVideoTestSession(t, s, origin.URL+"/"+fileName)
			if tc.rejectTrailing && v.video.File.metadataEnd <= v.video.File.cueEnd {
				t.Fatal("index missed the trailing Void element")
			}
			// Simulate the optional tail pin being unavailable during indexing.
			// Keep the cue range uncached through the HTTP handler's retries.
			v.video.File.tail = nil
			s.Engine.Cache.Resize(0)
			fail.Store(true)
			last := len(v.boundaries) - 2
			if tc.truncateMedia {
				// Prove that successful FFmpeg output and a final flush cannot
				// alone distinguish this media failure from the recoverable cues.
				start, end := v.boundaries[last], v.boundaries[last+1]
				raw, err := s.Engine.videoAttempt(v.ctx, v.video, start, end-start)
				if err != nil {
					t.Fatalf("fixture did not produce a successful truncated remux: %v", err)
				}
				out := filepath.Join(t.TempDir(), "partial.mp4")
				if err := os.WriteFile(out, raw, 0600); err != nil {
					t.Fatal(err)
				}
				_, hi := packetTimes(t, out, "v:0")
				if hi >= 17.95 {
					t.Fatal("fixture did not truncate video packets", hi)
				}
			}
			w := httptest.NewRecorder()
			s.media(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/media/%s/video/%d/segment.m4s", v.ID, last), nil))
			if tc.rejectCues && rejectedCues.Load() == 0 {
				t.Fatal("remux did not encounter the unavailable cue range")
			}
			if tc.rejectTrailing && rejectedTrailing.Load() == 0 {
				t.Fatal("remux did not encounter the unavailable trailing padding")
			}
			if tc.truncateMedia {
				if rejectedMedia.Load() == 0 || w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "upstream video download failed: origin HTTP 403") {
					t.Fatalf("truncated final segment: rejected=%d, status=%d, body=%s", rejectedMedia.Load(), w.Code, w.Body.String())
				}
				if len(v.Errors) == 0 {
					t.Fatal("media failure missing from dashboard")
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("final segment: %d %s", w.Code, w.Body.String())
			}
			if len(v.Errors) != 0 {
				t.Fatal("recovered cue failure was reported as a playback error", v.Errors)
			}
			initResponse := httptest.NewRecorder()
			s.media(initResponse, httptest.NewRequest(http.MethodGet, "/media/"+v.ID+"/video/0/init.mp4", nil))
			if initResponse.Code != http.StatusOK {
				t.Fatal(initResponse.Code, initResponse.Body.String())
			}
			out := filepath.Join(t.TempDir(), "final.mp4")
			if err := os.WriteFile(out, append(bytes.Clone(initResponse.Body.Bytes()), w.Body.Bytes()...), 0600); err != nil {
				t.Fatal(err)
			}
			lo, hi := packetTimes(t, out, "v:0")
			if lo < v.boundaries[last]+.95 || lo > v.boundaries[last]+1.05 || hi < 17.95 || hi > 18.1 {
				t.Fatalf("final segment lost video packets: %.3f..%.3f", lo, hi)
			}
			command(t, "ffmpeg", "-nostdin", "-v", "error", "-xerror", "-i", out, "-map", "0:v:0", "-f", "null", "-")
		})
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
