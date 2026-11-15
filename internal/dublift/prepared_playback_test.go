package dublift

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Reproduce an IMDb episode transition through a stream URL fetched before
// prefetch, without consuming /stream again once preparation becomes ready.
func cachedEpisodeFixture(t *testing.T) (*nextFixture, Stream) {
	t.Helper()
	f := newNextFixture(t)
	f.videos = []episodeMetadata{{Season: 1, Episode: 2}, {Season: 1, Episode: 3}, {Season: 1, Episode: 4}}
	target, _ := url.Parse(f.origin.URL)
	f.s.Net.Client.Transport = contentMetadataTransport{base: f.s.Net.Client.Transport, target: target}
	current, _ := ParseContent("series", "tt2879552:1:2")
	v := f.s.newSession(current, f.parent.stream)
	v.source = f.parent.source
	v.Duration = 600
	if !f.s.playerRequest(v, httptest.NewRequest("GET", "/master.m3u8", nil)) {
		t.Fatal("parent playback not selected")
	}
	f.parent = v
	response := nextRequest(t, f.s, "GET", "http://player.test/stream/series/tt2879552:1:3.json", "")
	var listing struct {
		Streams []Stream `json:"streams"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Streams) != 4 {
		t.Fatal(response.Code, response.Body.String())
	}
	return f, listing.Streams[1]
}

func assertFollowingIMDbPanel(t *testing.T, f *nextFixture) {
	t.Helper()
	view := automaticStatus(t, f.s)
	if view.State != "waiting" || view.ParentContent != "tt2879552:1:3" || view.Content != "tt2879552:1:4" || view.StreamName != "" || view.AudioTiming != "" || view.PreparationStatus != "" {
		t.Fatalf("stale canceled/current-episode panel: %+v", view)
	}
	if f.s.nextEpisode != nil {
		t.Fatal("episode transition started another prefetch")
	}
}

func TestPreparedPlaybackAdoptsEarlierIMDbStreamURL(t *testing.T) {
	for _, mode := range []string{"unconsumed response", "response already consumed", "restored earlier ticket"} {
		t.Run(mode, func(t *testing.T) {
			f, earlier := cachedEpisodeFixture(t)
			u, _ := url.Parse(earlier.URL)
			oldID := strings.Split(strings.TrimPrefix(u.Path, "/media/"), "/")[0]
			old := f.s.getSession(oldID)
			p := f.trigger(t, 300)
			waitNext(t, p)
			if !p.ready {
				t.Fatal("prefetch did not prepare IMDb episode", p.sessions[1].Errors)
			}
			warmed := p.sessions[p.expected]
			manual := 2.25
			if err := f.s.Alignments.UpdateContext(warmed.ctx, warmed.Key, func(a *Alignment) { a.Manual = &manual }); err != nil {
				t.Fatal(err)
			}
			if mode == "response already consumed" {
				c, _ := ParseContent("series", "tt2879552:1:3")
				if f.s.consumeNextEpisode(c) == nil {
					t.Fatal("handoff failed")
				}
			}
			if mode == "restored earlier ticket" {
				f.s.mu.Lock()
				f.s.discardLocked(old)
				f.s.mu.Unlock()
			}
			before, unrelated := f.selected.Load(), f.unrelated.Load()
			head := nextRequest(t, f.s, "HEAD", earlier.URL, "")
			view := automaticStatus(t, f.s)
			if head.Code != 200 || warmed.Playing || !f.parent.Playing || view.State == "waiting" {
				t.Fatal("HEAD selected or advanced playback", head.Code, view)
			}
			w := nextRequest(t, f.s, "GET", earlier.URL, "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "/media/"+warmed.ID+"/") {
				t.Fatal("old URL did not serve warmed media", w.Code, w.Body.String())
			}
			master, err := ParseHLS(w.Body.Bytes(), Origin{URL: earlier.URL})
			if err != nil || master.Master == nil {
				t.Fatal("invalid adopted master", err)
			}
			childURL, _ := resolveURL(earlier.URL, master.Master.Variants[0].URI)
			child := nextRequest(t, f.s, "GET", childURL, "")
			if child.Code != 200 || !strings.Contains(child.Body.String(), "/media/"+warmed.ID+"/") {
				t.Fatal("video playlist did not use warmed endpoints", child.Code, child.Body.String())
			}
			for _, rendition := range master.Master.Renditions {
				if rendition.URI == nil {
					continue
				}
				if !strings.HasPrefix(*rendition.URI, "http://player.test/media/"+warmed.ID+"/") {
					t.Fatal("audio/subtitle playlist escaped warmed session", *rendition.URI)
				}
				childURL, _ := resolveURL(earlier.URL, *rendition.URI)
				if child := nextRequest(t, f.s, "GET", childURL, ""); child.Code != 200 {
					t.Fatal("adopted rendition could not be loaded", child.Code, child.Body.String())
				}
			}
			if f.s.getSession(warmed.ID) != warmed || !warmed.Playing || warmed.ctx.Err() != nil || f.parent.ctx.Err() == nil || old.ctx.Err() == nil || f.s.playbackOffset(warmed, 0) != manual || warmed.nextEpisodeAttempted {
				t.Fatal("prepared state was discarded or rebuilt")
			}
			assertFollowingIMDbPanel(t, f)
			for _, method := range []string{"HEAD", "GET", "GET"} {
				if w := nextRequest(t, f.s, method, earlier.URL, ""); w.Code != 200 {
					t.Fatal("old master alias was lost", w.Code, w.Body.String())
				}
			}
			if f.selected.Load() != before || f.unrelated.Load() != unrelated {
				t.Fatal("cached playback launched a stream search")
			}
			state := f.s.status(httptest.NewRequest("GET", "/api/status", nil))
			if sessions := state["sessions"].([]map[string]any); len(sessions) != 1 || sessions[0]["id"] != warmed.ID {
				t.Fatal("alias duplicated dashboard sessions", sessions)
			}
			// Playback adoption removes the unused response; subsequent listing is full discovery.
			w = nextRequest(t, f.s, "GET", "/stream/series/tt2879552:1:3.json", "")
			if w.Code != 200 || f.selected.Load() != before+1 || f.unrelated.Load() != unrelated+1 || warmed.ctx.Err() != nil {
				t.Fatal("adoption left a restricted listing cache or discarded playback")
			}
			nextRequest(t, f.s, "GET", earlier.URL, "")
			if !warmed.Playing || f.s.getSession(warmed.ID) != warmed {
				t.Fatal("normal refresh lost active prepared playback")
			}
			f.s.mu.Lock()
			f.s.discardLocked(warmed)
			alias, aliasID := f.s.playbackAlias, f.s.playbackAliasID
			f.s.mu.Unlock()
			if alias != nil || aliasID != "" {
				t.Fatal("expired playback retained its master alias")
			}
		})
	}
}

func TestPreparedPlaybackConcurrentEarlierMasterRequests(t *testing.T) {
	f, earlier := cachedEpisodeFixture(t)
	p := f.trigger(t, 300)
	waitNext(t, p)
	warmed := p.sessions[p.expected]
	before, unrelated := f.selected.Load(), f.unrelated.Load()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			w := nextRequest(t, f.s, "GET", earlier.URL, "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "/media/"+warmed.ID+"/") {
				t.Errorf("concurrent master lost prepared playback: %d", w.Code)
			}
		})
	}
	wg.Wait()
	if !warmed.Playing || warmed.ctx.Err() != nil || f.selected.Load() != before || f.unrelated.Load() != unrelated {
		t.Fatal("concurrent selection discarded/rebuilt preparation")
	}
	assertFollowingIMDbPanel(t, f)
}

func TestPreparedPlaybackAdoptedMasterPreservesPublicPath(t *testing.T) {
	f, earlier := cachedEpisodeFixture(t)
	cfg := f.s.Config.Get()
	cfg.PublicURL = "https://public.test/dublift/"
	if err := f.s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	publicBase := strings.TrimRight(cfg.PublicURL, "/")
	publicMaster, _ := url.Parse(earlier.URL)
	publicMaster.Scheme, publicMaster.Host = "https", "public.test"
	publicMaster.Path = "/dublift" + publicMaster.Path
	request := func(raw string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", raw, nil)
		// Model the reverse proxy stripping its public mount path.
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/dublift")
		w := httptest.NewRecorder()
		f.s.ServeHTTP(w, r)
		return w
	}
	p := f.trigger(t, 300)
	waitNext(t, p)
	if !p.ready {
		t.Fatal("prefetch did not prepare")
	}
	warmed := p.sessions[p.expected]
	for range 2 {
		w := request(publicMaster.String())
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		master, err := ParseHLS(w.Body.Bytes(), Origin{URL: publicMaster.String()})
		if err != nil || master.Master == nil {
			t.Fatal("invalid adopted master", err)
		}
		children := []string{master.Master.Variants[0].URI}
		for _, rendition := range master.Master.Renditions {
			if rendition.URI != nil {
				children = append(children, *rendition.URI)
			}
		}
		for _, uri := range children {
			childURL, err := resolveURL(publicMaster.String(), uri)
			if err != nil || !strings.HasPrefix(childURL, publicBase+"/media/"+warmed.ID+"/") {
				t.Fatal("adopted playlist lost the public path or warmed session", childURL, err)
			}
			if child := request(childURL); child.Code != http.StatusOK {
				t.Fatal("adopted child playlist could not be loaded", child.Code, child.Body.String())
			}
		}
	}
}

func TestPreparedPlaybackDoesNotSubstituteDifferentSelectedMedia(t *testing.T) {
	for _, difference := range []string{"origin URL", "source connection", "binge group", "subtitles", "quality variant"} {
		t.Run(difference, func(t *testing.T) {
			f, earlier := cachedEpisodeFixture(t)
			u, _ := url.Parse(earlier.URL)
			oldID := strings.Split(strings.TrimPrefix(u.Path, "/media/"), "/")[0]
			selected := f.s.getSession(oldID)
			switch difference {
			case "origin URL":
				f.streams[1] = json.RawMessage(fmt.Sprintf(`{"name":"expected","url":%q,"behaviorHints":{"bingeGroup":%q}}`, f.origin.URL+"/source.m3u8?new-signed-url", exactNextGroup))
			case "source connection":
				selected.source = f.s.Config.Get().Sources[1]
			case "binge group":
				selected.stream.BehaviorHints.BingeGroup = "different group from an earlier listing"
			case "subtitles":
				selected.stream.Subtitles = []Subtitle{{ID: "old", Lang: "en", URL: f.origin.URL + "/old.vtt"}}
			case "quality variant":
				selected.lookupKey = "a different selected variant"
			}
			p := f.trigger(t, 300)
			waitNext(t, p)
			if !p.ready {
				t.Fatal("fixture not ready", p.sessions[1].Errors)
			}
			warmed := p.sessions[p.expected]
			// Selection is enough to check ownership; preparation of the user's
			// different stream follows the normal playback path independently.
			actual, ok := f.s.selectPlayerSession(selected, httptest.NewRequest("GET", earlier.URL, nil))
			if !ok || actual != selected || !selected.Playing || selected.ctx.Err() != nil || warmed.ctx.Err() == nil || warmed.Playing {
				t.Fatal("different selected stream was silently substituted")
			}
			assertFollowingIMDbPanel(t, f)
		})
	}
}

func TestPreparedPlaybackDifferentEpisodeStillCancelsSpeculation(t *testing.T) {
	f, _ := cachedEpisodeFixture(t)
	p := f.trigger(t, 300)
	waitNext(t, p)
	c, _ := ParseContent("series", "tt2879552:1:5")
	other := f.s.newSession(c, Stream{})
	f.s.playerRequest(other, httptest.NewRequest("GET", "/master.m3u8", nil))
	view := automaticStatus(t, f.s)
	if p.sessions[p.expected].ctx.Err() == nil || view.State != "canceled" || !strings.Contains(view.Message, "different episode or title") {
		t.Fatal("unrelated playback was treated as prepared playback", view)
	}
}

func TestPreparedPlaybackAliasIsMasterOnly(t *testing.T) {
	f, earlier := cachedEpisodeFixture(t)
	p := f.trigger(t, 300)
	waitNext(t, p)
	w := nextRequest(t, f.s, "GET", earlier.URL, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	u, _ := url.Parse(earlier.URL)
	oldID := strings.Split(strings.TrimPrefix(u.Path, "/media/"), "/")[0]
	for _, suffix := range []string{"video.m3u8", "resource/unowned"} {
		r := httptest.NewRequest(http.MethodGet, "/media/"+oldID+"/"+suffix, nil)
		w := httptest.NewRecorder()
		f.s.ServeHTTP(w, r)
		if w.Code != 410 {
			t.Fatal("old ID gained access to unrelated media resources", suffix, w.Code)
		}
	}
}
