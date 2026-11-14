package dublift

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func standaloneSession(t *testing.T, s *Server, stream Stream) *Session {
	t.Helper()
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, stream)
	v.SourceName = "Movy"
	v.video = &Asset{ID: "synthetic-video"}
	v.boundaries = []float64{0, 6, 12, 13}
	v.clockBase = 1.4
	v.Duration = 13
	v.registerStandaloneSubtitles()
	v.prepare.Do(func() { close(v.ready) })
	return v
}

func subtitleRequest(s *Server, v *Session, track Track, part string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/media/"+v.ID+"/track/"+track.ID+part, nil))
	return w
}

func TestStandaloneSubtitleRegistrationAndPlaybackMetadata(t *testing.T) {
	s := lifecycleServer(t)
	raw := `{"name":"Movy","url":"https://video.test/stream?secret=video","custom":{"keep":true},"subtitles":[{"id":"../../audio","url":"https://sub.test/one?secret=subtitle","lang":" ENG "},{"id":"../../audio","url":"https://sub.test/two","lang":"en"},{"url":"https://sub.test/one?secret=subtitle","lang":"English"},{"url":"https://sub.test/one?secret=subtitle","lang":"FRA"},{"url":"https://sub.test/pt","lang":"por_BR"},{"url":"https://sub.test/unknown","lang":""},{"url":"file:///bad","lang":"en"}]}`
	var stream Stream
	if err := json.Unmarshal([]byte(raw), &stream); err != nil {
		t.Fatal(err)
	}
	v := standaloneSession(t, s, stream)
	wantLang := []string{"en", "en", "fr", "pt-br", "und"}
	if len(v.tracks) != len(wantLang) {
		t.Fatal(v.tracks)
	}
	ids := map[string]bool{}
	for i, track := range v.tracks {
		if track.Lang != wantLang[i] || !track.Original || !track.Subtitle || track.Asset != nil || track.Standalone == nil || ids[track.ID] || !regexp.MustCompile(`^standalone-[a-f0-9]{32}$`).MatchString(track.ID) {
			t.Fatal("invalid standalone track", track)
		}
		ids[track.ID] = true
	}
	if v.tracks[0].Name != "English · Movy" || v.tracks[1].Name != "English · Movy (2)" {
		t.Fatal("ambiguous source labels", v.tracks)
	}
	transformed, _ := json.Marshal(stream.dubbed("http://local.test/master.m3u8"))
	var fields map[string]json.RawMessage
	json.Unmarshal(transformed, &fields)
	if _, ok := fields["subtitles"]; ok || fields["custom"] == nil {
		t.Fatal("incorrect transformed JSON", string(transformed))
	}
	native, _ := json.Marshal(stream.localPlayback("http://local.test/native.m3u8", true))
	json.Unmarshal(native, &fields)
	if fields["subtitles"] == nil {
		t.Fatal("native playback changed")
	}
	original, _ := json.Marshal(v.stream)
	if !reflect.DeepEqual(jsonValue(t, original), jsonValue(t, []byte(raw))) {
		t.Fatal("fallback metadata changed")
	}
	v.ContentName = "Fixture"
	ticket := s.sealPlayback(v)
	s.mu.Lock()
	s.discardLocked(v)
	s.mu.Unlock()
	restored, err := s.restorePlayback(v.ID, ticket)
	if err != nil {
		t.Fatal(err)
	}
	restored.registerStandaloneSubtitles()
	restoredJSON, _ := json.Marshal(restored.stream)
	if !reflect.DeepEqual(v.tracks, restored.tracks) || !bytes.Equal(restoredJSON, original) {
		t.Fatal("ticket restoration changed tracks or original metadata")
	}
	// Cap registration even for non-Movy upstreams; never trust their IDs.
	stream.Subtitles = nil
	for i := range 205 {
		stream.Subtitles = append(stream.Subtitles, Subtitle{URL: fmt.Sprintf("https://sub.test/%d", i), Lang: "jpn"})
	}
	limited := standaloneSession(t, s, stream)
	if len(limited.tracks) != 200 || limited.tracks[199].Lang != "ja" {
		t.Fatal("standalone track cap changed")
	}
}

func TestStandaloneSubtitleHeaderIdentity(t *testing.T) {
	var regularReads, forcedReads atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Video-Key") != "inherited" {
			t.Error("subtitle lost inherited headers")
		}
		selection := r.Header.Get("X-Subtitle-ID")
		switch selection {
		case "regular":
			regularReads.Add(1)
		case "forced":
			forcedReads.Add(1)
		default:
			t.Error("unexpected subtitle selection", selection)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "WEBVTT\n\n00:05.000 --> 00:07.000\n%s subtitle\n", selection)
	}))
	defer origin.Close()
	s := lifecycleServer(t)
	stream := Stream{
		URL:     origin.URL + "/video",
		Headers: map[string]string{"X-Subtitle-ID": "regular", "X-Video-Key": "inherited"},
		Subtitles: []Subtitle{
			{URL: origin.URL + "/subtitle", Lang: "en"},
			{URL: origin.URL + "/subtitle", Lang: "eng", Headers: map[string]string{"X-Subtitle-ID": "forced", "X-Video-Key": "inherited"}},
			// Equivalent effective headers still deduplicate, including when
			// their casing or explicit/inherited representation differs.
			{URL: origin.URL + "/subtitle", Lang: "English", Headers: map[string]string{"x-subtitle-id": "regular", "x-video-key": "inherited"}},
			{URL: origin.URL + "/subtitle", Lang: "en", Headers: map[string]string{"X-Subtitle-ID": "forced"}},
		},
	}
	v := standaloneSession(t, s, stream)
	if len(v.tracks) != 2 || v.tracks[0].ID == v.tracks[1].ID {
		t.Fatal("distinct subtitle requests must have distinct tracks")
	}
	if regularReads.Load() != 0 || forcedReads.Load() != 0 {
		t.Fatal("registration fetched subtitle bodies")
	}
	for n := range 2 {
		for i, want := range []string{"regular subtitle", "forced subtitle"} {
			w := subtitleRequest(s, v, v.tracks[i], fmt.Sprintf("/%d.vtt", n))
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
				t.Fatal("subtitle response used the wrong request or cache entry", w.Code, w.Body.String())
			}
		}
	}
	if regularReads.Load() != 1 || forcedReads.Load() != 1 {
		t.Fatal("each subtitle conversion must be downloaded and cached separately", regularReads.Load(), forcedReads.Load())
	}
	rebuilt := &Session{SourceName: v.SourceName, stream: stream}
	rebuilt.registerStandaloneSubtitles()
	if !reflect.DeepEqual(v.tracks, rebuilt.tracks) {
		t.Fatal("rebuilding subtitle tracks changed header-based identity")
	}
}

func TestStandaloneSubtitleLazyCoalescingAndSeekWindows(t *testing.T) {
	var requests atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "\ufeff1\r\n00:00:00,000 --> 00:00:01,000\r\nStart\r\n\r\n2\r\n00:00:05,000 --> 00:00:07,000\r\n<i>Across</i>\r\nsecond line\r\n\r\n3\r\n00:00:12,000 --> 00:00:15,000\r\nFinal\r\n")
	}))
	defer origin.Close()
	s := lifecycleServer(t)
	v := standaloneSession(t, s, Stream{URL: origin.URL + "/video", Subtitles: []Subtitle{{URL: origin.URL + "/extensionless?private=secret", Lang: "en"}}})
	track := v.tracks[0]
	master, err := s.master(v)
	if err != nil || !bytes.Contains(master, []byte(`TYPE=SUBTITLES`)) || !bytes.Contains(master, []byte(`SUBTITLES="subs"`)) || bytes.Contains(master, []byte("secret")) || bytes.Contains(master, []byte(origin.URL)) {
		t.Fatal(string(master), err)
	}
	w := subtitleRequest(s, v, track, ".m3u8")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "#EXT-X-PLAYLIST-TYPE:VOD") || !strings.Contains(w.Body.String(), track.ID+"/2.vtt") || !strings.Contains(w.Body.String(), "#EXTINF:1.000000") || strings.Contains(w.Body.String(), "resource/") || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("standalone used an origin proxy playlist", w.Body.String())
	}
	if requests.Load() != 0 {
		t.Fatal("registration or playlists fetched a subtitle body")
	}
	responses := make([]*httptest.ResponseRecorder, 12)
	var wg sync.WaitGroup
	for i := range responses {
		wg.Go(func() { responses[i] = subtitleRequest(s, v, track, fmt.Sprintf("/%d.vtt", i%3)) })
	}
	<-started
	once.Do(func() { close(release) })
	wg.Wait()
	const header = "WEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:126000\n\n"
	const across = "00:00:05.000 --> 00:00:07.000\n<i>Across</i>\nsecond line\n\n"
	wantSegments := []string{
		header + "00:00:00.000 --> 00:00:01.000\nStart\n\n" + across,
		header + across,
		header + "00:00:12.000 --> 00:00:15.000\nFinal\n\n",
	}
	for i, response := range responses {
		if response.Code != 200 || response.Header().Get("Content-Type") != "text/vtt" {
			t.Fatal(i, response.Code, response.Body.String())
		}
		if response.Body.String() != wantSegments[i%3] {
			t.Fatal("incorrect overlapping cues, full timing, or clock", response.Body.String())
		}
	}
	for _, n := range []int{2, 0, 1, 2, 0} {
		if w := subtitleRequest(s, v, track, fmt.Sprintf("/%d.vtt", n)); w.Code != 200 {
			t.Fatal("seek failed", w.Code)
		}
	}
	if requests.Load() != 1 {
		t.Fatal("subtitle downloaded more than once", requests.Load())
	}
	if w := subtitleRequest(s, v, track, "/3.vtt"); w.Code != 404 {
		t.Fatal("accepted out-of-range final segment")
	}
	// Cues ending at the window start or starting at its end do not overlap.
	// An empty window remains a valid, mapped WebVTT segment.
	data, err := s.subtitles(v.ctx, v, track, 7, 5, 99, v.clockBase)
	if err != nil || string(data) != header {
		t.Fatal(string(data), err)
	}
	// The direct helper used the same owner and therefore the same conversion.
	if requests.Load() != 1 {
		t.Fatal("subsequent window missed normalized cache")
	}
}

func TestStandaloneSubtitleFormatsAndBounds(t *testing.T) {
	for _, text := range []string{
		"\ufeffWEBVTT\r\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:999999\r\n\r\nid\r\n00:01.000 --> 00:02.500 align:start\r\n<b>Hello</b>\r\nworld\r\n",
		"1\n00:00:01,000 --> 00:00:02,500\n<b>Hello</b>\nworld\n",
		"00:00:01.000 --> 00:00:02.500\n<b>Hello</b>\nworld\n",
	} {
		data, err := normalizeStandaloneSubtitle(t.Context(), []byte(text))
		if err != nil || len(data) < 24 || binary.LittleEndian.Uint64(data) != 1000 || binary.LittleEndian.Uint64(data[8:]) != 2500 || !bytes.Contains(data, []byte("<b>Hello</b>\nworld")) {
			t.Fatal("format conversion failed", err)
		}
	}
	for name, text := range map[string]string{
		"empty": "", "html": "<!doctype html><html>challenge</html>", "ass": "[Script Info]\nTitle: unsupported",
		"encoding": "\xff\xfeW\x00E\x00B\x00", "nul": "WEBVTT\n\n\x00", "html-header": "WEBVTT\n<html>challenge</html>",
		"bad-time": "WEBVTT\n\n00:61.000 --> 00:62.000\nBad", "reverse": "1\n00:00:03,000 --> 00:00:02,000\nBad",
		"missing-time": "WEBVTT\n\nnot a cue", "missing-payload": "WEBVTT\n\n00:01.000 --> 00:02.000",
		"bad-block":    "WEBVTT\n\n00:01.000 --> 00:02.000\nGood\n\nmalformed",
		"html-payload": "WEBVTT\n\n00:01.000 --> 00:02.000\n<script>bad</script>", "style": "WEBVTT\n\nSTYLE\n::cue { color: red; }",
		"nan": "WEBVTT\n\nNaN --> Inf\nBad", "setting": "WEBVTT\n\n00:01.000 --> 00:02.000 align:\"private\"\nBad",
		"long-line":  "WEBVTT\n\n00:01.000 --> 00:02.000\n" + strings.Repeat("x", standaloneLineBytes+1),
		"long-cue":   "WEBVTT\n\n00:01.000 --> 00:02.000\n" + strings.Repeat(strings.Repeat("x", 1000)+"\n", 70),
		"many-lines": "WEBVTT\n\n00:01.000 --> 00:02.000\n" + strings.Repeat("x\n", 1024),
		"many-cues":  "WEBVTT\n\n" + strings.Repeat("00:01.000 --> 00:02.000\nx\n\n", standaloneCueLimit+1),
		"input":      strings.Repeat("x", standaloneInputLimit+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeStandaloneSubtitle(t.Context(), []byte(text)); err == nil {
				t.Fatal("accepted invalid or oversized subtitle")
			}
		})
	}
	// Millions of blank lines must not allocate a slice per line/block.
	blank := []byte("WEBVTT\n\n" + strings.Repeat("\n", standaloneInputLimit-8))
	allocs := testing.AllocsPerRun(2, func() {
		if _, err := normalizeStandaloneSubtitle(t.Context(), blank); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 30 {
		t.Fatal("blank-line parsing allocations grew with input", allocs)
	}
}

func TestStandaloneSubtitleFailuresAreIsolated(t *testing.T) {
	for _, mode := range []string{"malformed", "missing", "oversized-length", "oversized-chunked"} {
		t.Run(mode, func(t *testing.T) {
			var hits atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/good" {
					io.WriteString(w, "WEBVTT\n\n00:01.000 --> 00:02.000\nGood")
					return
				}
				hits.Add(1)
				switch mode {
				case "malformed":
					io.WriteString(w, "<html>private-signed-query</html>")
				case "missing":
					w.WriteHeader(404)
				case "oversized-length":
					w.Header().Set("Content-Length", fmt.Sprint(standaloneInputLimit+1))
				case "oversized-chunked":
					w.(http.Flusher).Flush()
					io.WriteString(w, strings.Repeat("x", standaloneInputLimit+1))
				}
			}))
			defer origin.Close()
			s := lifecycleServer(t)
			v := standaloneSession(t, s, Stream{URL: origin.URL + "/video", Subtitles: []Subtitle{{URL: origin.URL + "/bad?private-signed-query", Lang: "en"}, {URL: origin.URL + "/good", Lang: "it"}}})
			for range 2 {
				w := subtitleRequest(s, v, v.tracks[0], "/0.vtt")
				if w.Code != 502 || strings.Contains(w.Body.String(), "private-signed-query") || strings.Contains(w.Body.String(), origin.URL) {
					t.Fatal("subtitle error was successful or exposed origin data", w.Code, w.Body.String())
				}
			}
			if hits.Load() != 2 {
				t.Fatal("failed conversion was cached as success")
			}
			if w := subtitleRequest(s, v, v.tracks[1], "/0.vtt"); w.Code != 200 || !strings.Contains(w.Body.String(), "Good") {
				t.Fatal("failed subtitle broke another track", w.Code)
			}
			if _, err := s.master(v); err != nil || v.prepareErr != nil || v.ctx.Err() != nil {
				t.Fatal("subtitle failure changed playback state", err)
			}
			for _, msg := range v.Errors {
				if strings.Contains(msg, "private-signed-query") || strings.Contains(msg, origin.URL) {
					t.Fatal("diagnostics exposed provider data")
				}
			}
		})
	}
}

func TestStandaloneSubtitleHeadersCookiesAndRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		auth, cookie := "", "own=target"
		if r.URL.Path == "/explicit" {
			auth, cookie = "subtitle-token", "subtitle=cookie; own=target"
		}
		if r.Header.Get("Authorization") != auth || r.Header.Get("Cookie") != cookie || r.Header.Get("X-Video-Key") != "" || r.Header.Get("Referer") != "https://player.test/" || r.Header.Get("Origin") != "https://player.test" {
			t.Error("wrong cross-origin headers", r.Header)
		}
		io.WriteString(w, "WEBVTT\n\n00:01.000 --> 00:02.000\nGood")
	}))
	defer target.Close()
	// Different hostname ensures the cookie jar scopes video cookies too.
	targetURL := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	var sameHits atomic.Int32
	video := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sameHits.Add(1)
		if r.Header.Get("Authorization") != "video-token" || !strings.Contains(r.Header.Get("Cookie"), "video=cookie") || r.Header.Get("X-Video-Key") != "private" {
			t.Error("same-origin subtitle lost required headers", r.Header)
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, targetURL, 302)
			return
		}
		io.WriteString(w, "WEBVTT\n\n00:01.000 --> 00:02.000\nGood")
	}))
	defer video.Close()
	s := lifecycleServer(t)
	u, _ := url.Parse(targetURL)
	s.Net.Client.Jar.SetCookies(u, []*http.Cookie{{Name: "own", Value: "target"}})
	v := standaloneSession(t, s, Stream{URL: video.URL + "/video", Headers: map[string]string{"Authorization": "video-token", "Cookie": "video=cookie", "X-Video-Key": "private", "Referer": "https://player.test/", "Origin": "https://player.test"}, Subtitles: []Subtitle{{URL: video.URL + "/same", Lang: "en"}, {URL: targetURL, Lang: "it"}, {URL: video.URL + "/redirect", Lang: "de"}, {URL: targetURL + "/explicit", Lang: "fr", Headers: map[string]string{"Cookie": "subtitle=cookie", "Authorization": "subtitle-token"}}}})
	for _, track := range v.tracks {
		if w := subtitleRequest(s, v, track, "/0.vtt"); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if sameHits.Load() != 2 || targetHits.Load() != 3 {
		t.Fatal("unexpected subtitle requests")
	}
}

func TestStandaloneSubtitleCancellationAndInvalidation(t *testing.T) {
	for _, mode := range []string{"request", "session", "clear", "owner"} {
		t.Run(mode, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-release:
					io.WriteString(w, "WEBVTT\n\n00:01.000 --> 00:02.000\nGood")
				case <-r.Context().Done():
				}
			}))
			defer origin.Close()
			s := lifecycleServer(t)
			v := standaloneSession(t, s, Stream{URL: origin.URL + "/video", Subtitles: []Subtitle{{URL: origin.URL + "/sub", Lang: "en"}}})
			ctx, cancel := sessionRequestContext(t.Context(), v)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := s.subtitles(ctx, v, v.tracks[0], 0, 6, 0, 1)
				done <- err
			}()
			<-started
			switch mode {
			case "request":
				cancel()
			case "session":
				s.mu.Lock()
				s.discardLocked(v)
				s.mu.Unlock()
			case "clear":
				s.Engine.Cache.Clear()
			case "owner":
				s.Engine.Cache.DeleteOwner(v.ID)
			}
			once.Do(func() { close(release) })
			err := <-done
			if (mode == "request" || mode == "session") && !errors.Is(err, context.Canceled) {
				t.Fatal("subtitle download ignored cancellation", err)
			}
			if s.Engine.Cache.Used() != 0 {
				t.Fatal("discarded subtitle conversion repopulated cache")
			}
		})
	}
}

func TestStandaloneSubtitleCanceledDownloadRetriesAndStaysSessionOwned(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "WEBVTT\n\n00:05.000 --> 00:07.000\nGood")
	}))
	defer origin.Close()
	s := lifecycleServer(t)
	stream := Stream{URL: origin.URL + "/video", Subtitles: []Subtitle{{URL: origin.URL + "/sub", Lang: "en"}}}
	v := standaloneSession(t, s, stream)
	ctx, cancel := sessionRequestContext(t.Context(), v)
	defer cancel()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := s.subtitles(ctx, v, v.tracks[0], 0, 6, 0, 1)
		first <- err
	}()
	<-started
	go func() {
		_, err := s.subtitles(v.ctx, v, v.tracks[0], 6, 6, 0, 1)
		second <- err
	}()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal("first download ignored cancellation", err)
	}
	if err := <-second; err != nil || hits.Load() != 2 {
		t.Fatal("active request did not retry canceled conversion", err, hits.Load())
	}
	other := standaloneSession(t, s, stream)
	if w := subtitleRequest(s, other, other.tracks[0], "/0.vtt"); w.Code != 200 || hits.Load() != 3 {
		t.Fatal("subtitle cache leaked between session owners", w.Code, hits.Load())
	}
}

func TestStandaloneSubtitlesPrepareWithoutBodyReads(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "13", "-c:v", "libx264", "-preset", "ultrafast", "-g", "144", "-bf", "0", "-c:a", "aac", "-metadata:s:a:0", "language=eng", "-f", "hls", "-hls_time", "6", "-hls_list_size", "0", filepath.Join(dir, "video.m3u8"))
	var subtitleReads atomic.Int32
	files := http.FileServer(http.Dir(dir))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/official", "/old.vtt", "/vix.vtt":
			subtitleReads.Add(1)
			io.WriteString(w, "WEBVTT\n\n00:05.000 --> 00:07.000\nCue")
		case "/master.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"old\",NAME=\"Original English\",LANGUAGE=\"en\",URI=\"old.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,CODECS=\"avc1.42c00a,mp4a.40.2\",SUBTITLES=\"old\"\nvideo.m3u8\n")
		case "/old.m3u8", "/vix.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:13\n#EXTINF:13,\n%s.vtt\n#EXT-X-ENDLIST\n", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".m3u8"))
		default:
			files.ServeHTTP(w, r)
		}
	}))
	defer origin.Close()
	s := lifecycleServer(t)
	vix, err := s.loadSubtitle(t.Context(), Origin{URL: origin.URL + "/vix.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: origin.URL + "/master.m3u8", Subtitles: []Subtitle{{URL: origin.URL + "/official", Lang: "fr"}, {URL: origin.URL + "/unavailable", Lang: "it"}}})
	v.SourceName = "Movy"
	v.listedVix = []Track{{ID: "vix-sub", Name: "English · Vixsrc", Lang: "en", Asset: vix, Subtitle: true}}
	if err := s.prepareSession(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	if subtitleReads.Load() != 0 {
		t.Fatal("preparation fetched subtitle bodies")
	}
	master, err := s.master(v)
	if err != nil || bytes.Count(master, []byte("TYPE=SUBTITLES")) != 4 || !bytes.Contains(master, []byte("French · Movy")) || !bytes.Contains(master, []byte(`CODECS="avc1.42c00a,mp4a.40.2"`)) {
		t.Fatal("missing or changed renditions", string(master), err)
	}
	var official Track
	for _, track := range v.tracks {
		if track.Standalone != nil && track.Lang == "fr" {
			official = track
		} else if track.Standalone != nil {
			if w := subtitleRequest(s, v, track, "/0.vtt"); w.Code != 502 {
				t.Fatal("unavailable subtitle succeeded", w.Code)
			}
		}
	}
	// The failed track must leave real source video and audio usable.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/media/"+v.ID+"/video.m3u8", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/resource/") {
		t.Fatal("subtitle failure broke the video playlist", w.Code)
	}
	for _, track := range v.tracks {
		if !track.Subtitle {
			if w := subtitleRequest(s, v, track, "/0.m4s"); w.Code != 200 || w.Body.Len() == 0 {
				t.Fatal("subtitle failure broke source audio", w.Code, w.Body.String())
			}
			break
		}
	}
	before := subtitleRequest(s, v, official, "/0.vtt")
	if before.Code != 200 || subtitleReads.Load() != 1 {
		t.Fatal("official track was not lazy", before.Code)
	}
	beforeVix := subtitleRequest(s, v, v.listedVix[0], "/0.vtt")
	for _, mode := range []string{"automatic", "boundary", "manual"} {
		delay := 2.0
		if err := s.Alignments.UpdateContext(v.ctx, v.Key, func(a *Alignment) {
			a.Offset, a.Confidence, a.Manual, a.Boundaries = 2, 1, nil, nil
			if mode == "boundary" {
				a.Offset = 0
				a.Boundaries = []Boundary{{SourceTime: 0, Offset: 2, Confidence: 1}}
			} else if mode == "manual" {
				a.Manual = &delay
			}
		}); err != nil {
			t.Fatal(err)
		}
		after := subtitleRequest(s, v, official, "/0.vtt")
		afterVix := subtitleRequest(s, v, v.listedVix[0], "/0.vtt")
		if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) || !strings.Contains(beforeVix.Body.String(), "00:00:05.000 --> 00:00:06.000") || strings.Contains(afterVix.Body.String(), "Cue") {
			t.Fatal("VixSrc delay changed source subtitles or failed to shift VixSrc", mode, beforeVix.Body.String(), afterVix.Body.String())
		}
	}
	if math.Abs(v.clockBase) < .001 {
		t.Fatal("fixture lost its nonzero media clock")
	}
	v.ContentName = "Fixture"
	ticket := s.sealPlayback(v)
	s.mu.Lock()
	s.discardLocked(v)
	s.mu.Unlock()
	restored, err := s.restorePlayback(v.ID, ticket)
	if err != nil {
		t.Fatal(err)
	}
	restored.listedVix = v.listedVix
	reads := subtitleReads.Load()
	if err := s.prepareSession(t.Context(), restored); err != nil || subtitleReads.Load() != reads {
		t.Fatal("restoration fetched subtitle bodies or failed preparation", err)
	}
	var restoredOfficial Track
	for _, track := range restored.tracks {
		if track.Standalone != nil && track.Lang == "fr" {
			restoredOfficial = track
		}
	}
	if restoredOfficial.ID != official.ID || !reflect.DeepEqual(restored.stream.Subtitles, v.stream.Subtitles) {
		t.Fatal("prepared restoration changed official tracks")
	}
	if w := subtitleRequest(s, restored, restoredOfficial, "/0.vtt"); w.Code != 200 || subtitleReads.Load() != reads+1 {
		t.Fatal("restored subtitle reused discarded state", w.Code)
	}
}
