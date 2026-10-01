package dublift

import (
	"context"
	"encoding/json"
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

func TestDevelopmentRedirectIsTemporaryAndSkipsPreparation(t *testing.T) {
	s := lifecycleServer(t)
	var requests atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "must not be fetched", 500)
	}))
	defer origin.Close()
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: origin.URL + "/original.mkv?token=opaque%2Fvalue", Name: "Original"})
	v.ticket = s.sealPlayback(v)
	playbackURL := s.playbackURL(httptest.NewRequest("GET", "http://local.test/", nil), v)
	toggle := func(enabled bool, originHeader string) *httptest.ResponseRecorder {
		body := `{"enabled":false}`
		if enabled {
			body = `{"enabled":true}`
		}
		r := httptest.NewRequest("POST", "/api/dev/redirect-original", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", originHeader)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if s.redirectOriginal.Load() {
		t.Fatal("redirect enabled by default")
	}
	if w := toggle(true, "http://evil.test"); w.Code != 403 || s.redirectOriginal.Load() {
		t.Fatal("cross-origin toggle was accepted", w.Code)
	}
	configBefore, err := os.ReadFile(s.Config.path)
	if err != nil {
		t.Fatal(err)
	}
	if w := toggle(true, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.status(httptest.NewRequest("GET", "/api/status", nil))["redirectOriginal"] != true {
		t.Fatal("toggle absent from status")
	}
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(method, playbackURL, nil))
		if w.Code != 307 || w.Header().Get("Location") != v.stream.URL || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("incorrect redirect", w.Code, w.Header())
		}
	}
	// Redirects also cover URLs cached by players after result cleanup.
	s.mu.Lock()
	s.discardLocked(v)
	s.mu.Unlock()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", playbackURL, nil))
	if w.Code != 307 || w.Header().Get("Location") != v.stream.URL || s.getSession(v.ID) != nil {
		t.Fatal("resume redirect started a session", w.Code)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", playbackURL+"tampered", nil))
	if w.Code != 410 || w.Header().Get("Location") != "" {
		t.Fatal("invalid ticket redirected", w.Code)
	}
	if requests.Load() != 0 || v.preparationStarted || v.Playing {
		t.Fatal("redirect fetched or prepared media")
	}
	configAfter, err := os.ReadFile(s.Config.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(configBefore) != string(configAfter) {
		t.Fatal("temporary toggle changed config")
	}
	// A new server sharing the same config starts with diagnostics disabled.
	restarted, err := NewServer(s.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.redirectOriginal.Load() {
		t.Fatal("toggle survived restart")
	}
	if w := toggle(false, ""); w.Code != 200 || s.redirectOriginal.Load() {
		t.Fatal("could not disable redirect")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("HEAD", playbackURL, nil))
	if w.Code != 200 || w.Header().Get("Location") != "" {
		t.Fatal("normal playback remained redirected")
	}
}

func TestPreparationTimeoutIncludesInspectionAndProbe(t *testing.T) {
	s := lifecycleServer(t)
	probe := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexec sleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config.Get()
	cfg.PreparationTimeoutSeconds = 1
	cfg.FFprobe = probe
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	var manifestRequests, prefixRequests atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/source.m3u8":
			manifestRequests.Add(1)
			time.Sleep(300 * time.Millisecond)
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n")
		case "/segment.ts":
			prefixRequests.Add(1)
			time.Sleep(300 * time.Millisecond)
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(make([]byte, 512))
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: origin.URL + "/source.m3u8"})
	started := time.Now()
	err := s.prepareSession(context.Background(), v)
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "source video could not be read") {
		t.Fatal("probe did not time out", err)
	}
	if elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Fatal("whole preparation deadline not enforced", elapsed)
	}
	if manifestRequests.Load() != 1 || prefixRequests.Load() != 1 {
		t.Fatal("source inspection did not precede probing")
	}
	if v.Status != "Preparation failed" {
		t.Fatal(v.Status)
	}
	state := s.status(httptest.NewRequest("GET", "/api/status", nil))
	b, _ := json.Marshal(state)
	if !strings.Contains(string(b), `"preparationDone":true`) || strings.Contains(string(b), `"ready":true`) {
		t.Fatal("timed out preparation reported ready", string(b))
	}
}
