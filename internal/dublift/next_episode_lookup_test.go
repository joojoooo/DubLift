package dublift

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitProbeStarted(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("automatic preparation did not start its probe")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNextEpisodeUnrelatedAddonLookupCancelsUnusedWork(t *testing.T) {
	for _, stage := range []string{"searching", "preparing", "ready"} {
		for _, target := range []string{"movie/tmdb:603", "series/tmdb:999:1:2", "series/tmdb:100:1:3", "series/tmdb:100:1:1"} {
			t.Run(stage+"/"+target, func(t *testing.T) {
				f := newNextFixture(t)
				started := ""
				if stage == "searching" {
					f.block = make(chan struct{})
				}
				if stage == "preparing" {
					started = filepath.Join(t.TempDir(), "probe-started")
					script := fmt.Sprintf("#!/bin/sh\n: > '%s'\nexec sleep 60\n", strings.ReplaceAll(started, "'", "'\\''"))
					if err := os.WriteFile(f.s.Config.Get().FFprobe, []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
				}
				p := f.trigger(t, 300)
				switch stage {
				case "searching":
					select {
					case <-f.entered:
					case <-time.After(5 * time.Second):
						t.Fatal("automatic search not started")
					}
				case "preparing":
					waitProbeStarted(t, started)
				case "ready":
					waitNext(t, p)
					if !p.ready {
						t.Fatal("automatic result not ready")
					}
				}
				f.s.Engine.Cache.Put(f.parent.ctx, "foreground", []byte("keep playing"))
				var warmed *Session
				f.s.mu.Lock()
				if p.expected >= 0 {
					warmed = p.sessions[p.expected]
				}
				f.s.mu.Unlock()
				if warmed != nil {
					f.s.Engine.Cache.Put(warmed.ctx, "automatic", []byte("discard unused preparation"))
				}
				response := make(chan *httptest.ResponseRecorder, 1)
				go func() { response <- nextRequest(t, f.s, "GET", "/stream/"+target+".json", "") }()
				// Cancellation happens before ordinary discovery finishes, even when the
				// newly requested source lookup is waiting on the same slow provider.
				waitNext(t, p)
				if stage == "searching" {
					select {
					case <-f.canceled:
					case <-time.After(3 * time.Second):
						t.Fatal("obsolete origin read not canceled")
					}
					close(f.block)
				}
				var w *httptest.ResponseRecorder
				select {
				case w = <-response:
				case <-time.After(5 * time.Second):
					t.Fatal("ordinary discovery did not finish")
				}
				if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || f.unrelated.Load() != 1 {
					t.Fatal("new lookup did not use full ordinary discovery", w.Code, w.Body.String())
				}
				if p.ctx.Err() == nil || f.s.nextEpisode != nil || !f.parent.Playing || f.parent.ctx.Err() != nil {
					t.Fatal("lookup left speculative work or interrupted current playback")
				}
				if _, ok := f.s.Engine.Cache.Lookup(f.parent.ctx, "foreground"); !ok {
					t.Fatal("lookup cleared foreground playback cache")
				}
				if warmed != nil {
					if warmed.ctx.Err() == nil {
						t.Fatal("unused prepared session survived")
					}
					if _, ok := f.s.Engine.Cache.Lookup(warmed.ctx, "automatic"); ok {
						t.Fatal("unused next-episode cache survived")
					}
					if stage == "preparing" {
						select {
						case <-warmed.ready:
						default:
							t.Fatal("canceled preparation process did not stop")
						}
					}
				}
				view := automaticStatus(t, f.s)
				if view.State != "canceled" || !strings.Contains(view.Message, "requested streams for a different episode or title") {
					t.Fatal("dashboard did not explain lookup cancellation", view)
				}
				before := f.selected.Load()
				f.trigger(t, 550)
				if f.s.nextEpisode != nil || f.selected.Load() != before || f.s.nextEpisodeStage(p, "ready", "stale worker") {
					t.Fatal("obsolete playback requests or worker recreated prefetch")
				}
			})
		}
	}
}

func TestNextEpisodeRelatedLookupsAndDashboardProbesPreserveWork(t *testing.T) {
	f := newNextFixture(t)
	f.block = make(chan struct{})
	p := f.trigger(t, 300)
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("search not started")
	}
	ordinary := make(chan *httptest.ResponseRecorder, 1)
	go func() { ordinary <- nextRequest(t, f.s, "GET", "/stream/series/tmdb:100:1:2.json", "") }()
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("normal next-episode discovery not started promptly")
	}
	if p.ctx.Err() != nil || f.s.nextEpisode != p {
		t.Fatal("expected next-episode lookup canceled pending work")
	}
	close(f.block)
	select {
	case w := <-ordinary:
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("normal discovery blocked")
	}
	waitNext(t, p)
	if !p.ready || f.s.nextEpisode != p || f.unrelated.Load() != 1 {
		t.Fatal("expected unfinished lookup did not preserve prefetch and fall back to all sources")
	}
	for _, request := range []struct {
		method, path, body string
		code               int
	}{
		{"HEAD", "/stream/series/tmdb:999:1:1.json", "", 200},
		{"POST", "/api/resolve", `{"type":"movie","id":"tmdb:603"}`, 200},
		{"GET", "/api/status", "", 200},
		{"GET", "/stream/series/invalid.json", "", 400},
		{"OPTIONS", "/stream/series/tmdb:999:1:1.json", "", 204},
	} {
		w := nextRequest(t, f.s, request.method, request.path, request.body)
		if w.Code != request.code || f.s.nextEpisode != p || p.ctx.Err() != nil {
			t.Fatal("dashboard/probe request canceled or consumed automatic state", request.path, w.Code)
		}
	}
	before := f.selected.Load()
	w := nextRequest(t, f.s, "GET", "/stream/series/tmdb:100:1:2.json", "")
	if w.Code != 200 || f.s.nextEpisode != nil || f.selected.Load() != before || p.sessions[p.expected].ctx.Err() != nil {
		t.Fatal("expected first GET did not receive ready result")
	}
	// Once handed off, these are normal owned playback URLs. Later lookups must
	// not invalidate them, even if the old episode is still the active selection.
	nextRequest(t, f.s, "GET", "/stream/movie/tmdb:603.json", "")
	warmed := p.sessions[p.expected]
	if warmed.ctx.Err() != nil || f.s.getSession(warmed.ID) != warmed || !f.parent.Playing {
		t.Fatal("unrelated lookup invalidated handed-off URLs")
	}
}

func TestNextEpisodeLookupWhileIdentityResolves(t *testing.T) {
	for _, mode := range []string{"expected", "unrelated", "two different lookups"} {
		t.Run(mode, func(t *testing.T) {
			f := newNextFixture(t)
			entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			transport := f.s.Net.Client.Transport
			var first atomic.Bool
			f.s.Net.Client.Transport = movyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasPrefix(r.URL.Path, "/selected/meta/") && first.CompareAndSwap(false, true) {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						close(canceled)
						return nil, r.Context().Err()
					}
				}
				return transport.RoundTrip(r)
			})
			p := f.trigger(t, 300)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("metadata lookup not started")
			}
			path := "/stream/series/tmdb:100:1:2.json"
			if mode == "unrelated" {
				path = "/stream/movie/tmdb:603.json"
			}
			if w := nextRequest(t, f.s, "GET", path, ""); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if mode == "two different lookups" {
				if w := nextRequest(t, f.s, "GET", "/stream/series/tmdb:100:1:3.json", ""); w.Code != 200 {
					t.Fatal(w.Code)
				}
				waitNext(t, p)
				select {
				case <-canceled:
				case <-time.After(3 * time.Second):
					t.Fatal("different pending lookups did not immediately cancel obsolete metadata")
				}
			}
			close(release)
			waitNext(t, p)
			if mode == "expected" {
				if !p.ready || f.s.nextEpisode != p || f.selected.Load() != 2 || f.unrelated.Load() != 1 {
					t.Fatal("expected lookup was guessed to be unrelated before resolution")
				}
			} else {
				if p.ctx.Err() == nil || f.s.nextEpisode != nil || len(p.sessions) != 0 {
					t.Fatal("unrelated lookup published automatic sessions after metadata resolution")
				}
				want := int64(1)
				if mode == "two different lookups" {
					want = 2
				}
				if f.selected.Load() != want || f.unrelated.Load() != want {
					t.Fatal("obsolete prefetch searched streams after unrelated request", f.selected.Load(), f.unrelated.Load())
				}
				if view := automaticStatus(t, f.s); view.State != "canceled" {
					t.Fatal(view)
				}
			}
		})
	}
}

func TestNextEpisodeUnrelatedLookupRacesConsumption(t *testing.T) {
	for range 12 {
		f := newNextFixture(t)
		p := f.trigger(t, 300)
		waitNext(t, p)
		expected, _ := ParseContent("series", "tmdb:100:1:2")
		unrelated, _ := ParseContent("movie", "tmdb:603")
		start := make(chan struct{})
		var handed []*Session
		var wg sync.WaitGroup
		wg.Go(func() { <-start; handed = f.s.consumeNextEpisode(expected) })
		wg.Go(func() { <-start; f.s.consumeNextEpisode(unrelated) })
		close(start)
		wg.Wait()
		if f.s.nextEpisode != nil || !f.parent.Playing || f.parent.ctx.Err() != nil {
			t.Fatal("race retained speculative state or stopped playback")
		}
		warmed := p.sessions[p.expected]
		if handed == nil {
			if warmed.ctx.Err() == nil {
				t.Fatal("lookup won race but unused preparation survived")
			}
		} else if warmed.ctx.Err() != nil || f.s.getSession(warmed.ID) != warmed {
			t.Fatal("lookup discarded an already transferred playback session")
		}
	}
}
