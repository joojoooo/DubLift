package dublift

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
	"github.com/bluenviron/gohlslib/v2/pkg/playlist/primitives"
)

func preparedHLSSession(t *testing.T, s *Server, origin Origin) *Session {
	t.Helper()
	h, err := s.Net.LoadHLS(context.Background(), origin)
	if err != nil {
		t.Fatal(err)
	}
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: origin.URL})
	v.video = &Asset{ID: identity(origin.URL), Origin: h.Origin, HLS: h}
	v.Duration = h.Duration
	v.boundaries = []float64{0}
	for _, seg := range h.Segments {
		v.boundaries = append(v.boundaries, seg.Start+seg.Duration)
	}
	v.prepare.Do(func() { close(v.ready) })
	return v
}

func assertProxyPlaylistUnchanged(t *testing.T, v *Session, a *Asset, data []byte) {
	t.Helper()
	actual := string(data)
	v.mu.Lock()
	for id, res := range v.resources {
		actual = strings.ReplaceAll(actual, "/media/"+v.ID+"/resource/"+id, res.Origin.URL)
	}
	v.mu.Unlock()
	var want strings.Builder
	for _, line := range strings.Split(a.HLS.Raw, "\n") {
		l := strings.TrimSpace(line)
		if l != "" && !strings.HasPrefix(l, "#") {
			u, err := resolveURL(a.Origin.URL, l)
			if err != nil {
				t.Fatal(err)
			}
			want.WriteString(u)
		} else {
			want.WriteString(rewriteURI(line, a.Origin.URL, func(u string) string { return u }))
		}
		want.WriteByte('\n')
	}
	if actual != want.String() {
		t.Fatalf("proxy changed source playlist metadata:\n%s\nwant:\n%s", actual, want.String())
	}
}

func TestHLSPlaybackProxiesSourceMediaAndRenditions(t *testing.T) {
	ffmpegAvailable(t)
	for _, kind := range []string{"ts", "fmp4", "encrypted", "byterange"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x72:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-g", "20", "-sc_threshold", "0", "-c:a", "aac", "-hls_time", "2", "-hls_playlist_type", "vod"}
			if kind == "fmp4" || kind == "byterange" {
				args = append(args, "-hls_segment_type", "fmp4")
			}
			if kind == "byterange" {
				args = append(args, "-hls_flags", "single_file")
			}
			if kind == "encrypted" {
				key := filepath.Join(dir, "key.bin")
				if err := os.WriteFile(key, []byte("0123456789abcdef"), 0600); err != nil {
					t.Fatal(err)
				}
				info := filepath.Join(dir, "key.info")
				if err := os.WriteFile(info, []byte("key.bin\n"+key+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "-hls_key_info_file", info)
			}
			command(t, "ffmpeg", append(args, filepath.Join(dir, "video.m3u8"))...)
			// Preserve discontinuity markers and nonzero sequence numbers too.
			raw, err := os.ReadFile(filepath.Join(dir, "video.m3u8"))
			if err != nil {
				t.Fatal(err)
			}
			raw = bytes.Replace(raw, []byte("#EXT-X-MEDIA-SEQUENCE:0"), []byte("#EXT-X-MEDIA-SEQUENCE:17"), 1)
			// AES without an explicit IV derives it from the sequence number.
			if kind == "encrypted" {
				raw = bytes.Replace(raw, []byte("#EXT-X-MEDIA-SEQUENCE:17"), []byte("#EXT-X-MEDIA-SEQUENCE:0"), 1)
			}
			at := bytes.Index(raw, []byte("#EXTINF:"))
			raw = append(bytes.Clone(raw[:at]), append([]byte("#EXT-X-DISCONTINUITY\n"), raw[at:]...)...)
			if err := os.WriteFile(filepath.Join(dir, "video.m3u8"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000", "-t", "6", "-c:a", "aac", "-hls_time", "2", "-hls_playlist_type", "vod", filepath.Join(dir, "audio.m3u8"))
			sub := "WEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:1080000\n\n00:00:01.000 --> 00:00:03.000\nOriginal source cue.\n"
			for name, data := range map[string]string{"sub.vtt": sub, "sub.m3u8": "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsub.vtt\n#EXT-X-ENDLIST\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var rejected atomic.Int64
			files := http.FileServer(http.Dir(dir))
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cookie, _ := r.Cookie("origin-session")
				if r.Header.Get("X-Required") != "source-header" || (!strings.HasSuffix(r.URL.Path, ".m3u8") && (cookie == nil || cookie.Value != "source-cookie")) {
					rejected.Add(1)
					http.Error(w, "headers or cookie lost", 403)
					return
				}
				if strings.HasSuffix(r.URL.Path, ".m3u8") {
					http.SetCookie(w, &http.Cookie{Name: "origin-session", Value: "source-cookie", Path: "/"})
				}
				files.ServeHTTP(w, r)
			}))
			defer origin.Close()
			s := lifecycleServer(t)
			v := preparedHLSSession(t, s, Origin{origin.URL + "/video.m3u8", http.Header{"X-Required": {"source-header"}}})
			v.variant = &playlist.MultivariantVariant{Bandwidth: 500000, Codecs: []string{"avc1.42c00a", "mp4a.40.2"}}
			v.tracks = []Track{{ID: "embedded", Name: "Embedded original", Lang: "en", Original: true, Asset: v.video}}
			for _, rendition := range []struct {
				id, file string
				subtitle bool
			}{{"original-audio", "audio.m3u8", false}, {"original-subs", "sub.m3u8", true}} {
				h, err := s.Net.LoadHLS(context.Background(), Origin{origin.URL + "/" + rendition.file, v.video.Origin.Headers})
				if err != nil {
					t.Fatal(err)
				}
				v.tracks = append(v.tracks, Track{ID: rendition.id, Name: rendition.id, Lang: "en", Original: true, Subtitle: rendition.subtitle, Asset: &Asset{ID: rendition.id, Origin: h.Origin, HLS: h}})
			}
			// Healthy HLS proxying must work without any FFmpeg video remux.
			cfg := s.Config.Get()
			cfg.FFmpeg = filepath.Join(dir, "must-not-run")
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			local := httptest.NewServer(s)
			defer local.Close()
			base := local.URL + "/media/" + v.ID
			var master playlist.Multivariant
			if err := master.Unmarshal(getBytes(t, base+"/master.m3u8")); err != nil {
				t.Fatal(err)
			}
			if len(master.Variants[0].Codecs) != 2 || master.Variants[0].Codecs[0] != "avc1.42c00a" {
				t.Fatal("source codecs removed from master", master.Variants[0].Codecs)
			}
			for _, rendition := range master.Renditions {
				if rendition.Name == "Embedded original" && rendition.URI != nil {
					t.Fatal("embedded original audio was changed to an extracted rendition")
				}
			}
			videoPlaylist := getBytes(t, base+"/video.m3u8")
			assertProxyPlaylistUnchanged(t, v, v.video, videoPlaylist)
			for _, track := range v.tracks[1:] {
				assertProxyPlaylistUnchanged(t, v, track.Asset, getBytes(t, base+"/track/"+track.ID+".m3u8"))
			}
			proxied, err := ParseHLS(videoPlaylist, Origin{URL: base + "/video.m3u8"})
			if err != nil {
				t.Fatal(err)
			}
			for _, seg := range proxied.Segments {
				assertProxiedResource(t, s, v, seg.URI, seg.Range)
				for _, line := range []string{seg.Key, seg.Map} {
					if line == "" {
						continue
					}
					var attrs primitives.Attributes
					if err := attrs.Unmarshal(strings.SplitN(line, ":", 2)[1]); err != nil {
						t.Fatal(err)
					}
					assertProxiedResource(t, s, v, local.URL+attrs["URI"], attrs["BYTERANGE"])
				}
			}
			// Read every proxied original audio/subtitle resource as well.
			v.mu.Lock()
			resources := make(map[string]resource)
			for id, res := range v.resources {
				resources[id] = res
			}
			v.mu.Unlock()
			for id, res := range resources {
				if strings.HasSuffix(res.Origin.URL, "sub.vtt") {
					if got := getBytes(t, base+"/resource/"+id); string(got) != sub {
						t.Fatal("original subtitle timing changed", string(got))
					}
				} else if strings.Contains(res.Origin.URL, "/audio") {
					assertProxiedResource(t, s, v, base+"/resource/"+id, "")
				}
			}
			command(t, "ffmpeg", "-nostdin", "-v", "error", "-xerror", "-protocol_whitelist", "http,tcp,crypto", "-i", base+"/video.m3u8", "-map", "0:v:0", "-f", "null", "-")
			if rejected.Load() != 0 {
				t.Fatal("origin headers/cookies were lost", rejected.Load())
			}
		})
	}
}

func assertProxiedResource(t *testing.T, s *Server, v *Session, u, byteRange string) {
	t.Helper()
	id := u[strings.LastIndex(u, "/")+1:]
	v.mu.Lock()
	res := v.resources[id]
	v.mu.Unlock()
	rangeHeader := ""
	if byteRange != "" {
		var length, start int64
		if _, err := fmt.Sscanf(byteRange, "%d@%d", &length, &start); err != nil {
			t.Fatal(err)
		}
		rangeHeader = fmt.Sprintf("bytes=%d-%d", start, start+length-1)
	}
	original, err := s.Net.request(context.Background(), res.Origin, "GET", rangeHeader)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Body.Close()
	want, err := io.ReadAll(original.Body)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest("GET", u, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Range", rangeHeader)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != original.StatusCode || !bytes.Equal(got, want) || resp.Header.Get("Content-Length") != strconv.Itoa(len(want)) || resp.Header.Get("Content-Range") != original.Header.Get("Content-Range") {
		t.Fatal("source bytes or range response changed", u, resp.StatusCode, original.StatusCode, len(got), len(want))
	}
}

func TestHLSFailuresAfterStartupDoNotReplaceMedia(t *testing.T) {
	s := lifecycleServer(t)
	var mode atomic.Int64
	entered := make(chan struct{}, 1)
	payload := bytes.Repeat([]byte{0x47}, 512)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
			return
		}
		switch mode.Load() {
		case 1:
			http.Error(w, "download unavailable", http.StatusForbidden)
		case 2:
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.Write(payload[:len(payload)/2])
		case 3:
			entered <- struct{}{}
			<-r.Context().Done()
		default:
			http.ServeContent(w, r, "segment.ts", time.Time{}, bytes.NewReader(payload))
		}
	}))
	defer origin.Close()
	v := preparedHLSSession(t, s, Origin{URL: origin.URL + "/video.m3u8"})
	h, err := s.Net.LoadHLS(context.Background(), Origin{URL: origin.URL + "/audio.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	audio := &Asset{ID: "audio", Origin: h.Origin, HLS: h}
	v.tracks = []Track{
		{ID: "original", Name: "Original", Lang: "en", Original: true, Asset: audio},
		{ID: "generated", Name: "Italian", Lang: "it", Selector: "0:a:0", Asset: audio},
	}
	local := httptest.NewServer(s)
	defer local.Close()
	base := local.URL + "/media/" + v.ID
	master := getBytes(t, base+"/master.m3u8")
	video, err := ParseHLS(getBytes(t, base+"/video.m3u8"), Origin{URL: base + "/video.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	originalAudio, err := ParseHLS(getBytes(t, base+"/track/original.m3u8"), Origin{URL: base + "/track/original.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	mode.Store(1)
	for _, u := range []string{video.Segments[0].URI, originalAudio.Segments[0].URI} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "403") || strings.Contains(w.Body.String(), "error.m3u8") {
			t.Fatal("failed media was replaced by error frames or silent audio", w.Code, w.Body.String())
		}
	}
	// A failed extraction also remains an HTTP error after publication.
	ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf '%s\\n' 'audio remux failed' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config.Get()
	cfg.FFmpeg = ffmpeg
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", base+"/track/generated/0.ts", nil))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "audio remux failed") {
		t.Fatal("failed audio extraction was replaced by silence", w.Code, w.Body.String())
	}
	if v.ctx.Err() != nil || v.prepareErr != nil || !bytes.Equal(master, getBytes(t, base+"/master.m3u8")) {
		t.Fatal("failed media permanently changed playback")
	}
	mode.Store(2)
	resp, err := http.Get(video.Segments[0].URI)
	if err != nil {
		t.Fatal(err)
	}
	partial, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !errors.Is(readErr, io.ErrUnexpectedEOF) || !bytes.Equal(partial, payload[:len(payload)/2]) {
		t.Fatal("truncated origin was replaced instead of failing the media read", readErr, len(partial))
	}
	v.mu.Lock()
	errorsBefore := len(v.Errors)
	truncationRecorded := strings.Contains(strings.Join(v.Errors, "\n"), "unexpected EOF")
	v.mu.Unlock()
	if !truncationRecorded {
		t.Fatal("truncated download missing from dashboard")
	}
	mode.Store(3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w = httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.ServeHTTP(w, httptest.NewRequest("GET", video.Segments[0].URI, nil).WithContext(ctx))
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("media request did not reach the origin")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled media request did not stop")
	}
	v.mu.Lock()
	errorsAfter := len(v.Errors)
	v.mu.Unlock()
	if w.Code != http.StatusBadGateway || errorsBefore != errorsAfter || v.ctx.Err() != nil {
		t.Fatal("canceled request became a fatal playback error", w.Code, errorsBefore, errorsAfter)
	}
	mode.Store(0)
	for _, u := range []string{video.Segments[0].URI, originalAudio.Segments[0].URI} {
		if got := getBytes(t, u); !bytes.Equal(got, payload) {
			t.Fatal("retry did not resume source media")
		}
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", base+"/error.m3u8", nil))
	if w.Code != http.StatusNotFound {
		t.Fatal("healthy session exposed a startup error playlist", w.Code)
	}
}
