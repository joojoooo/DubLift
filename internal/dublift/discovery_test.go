package dublift

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestListingFetchDeadlinePreservesHealthyResults(t *testing.T) {
	for _, dashboard := range []bool{false, true} {
		for _, outcome := range []string{"deadline", "request canceled", "lookup replaced", "playback selected"} {
			name := "addon/" + outcome
			if dashboard {
				name = "dashboard/" + outcome
			}
			t.Run(name, func(t *testing.T) {
				s := lifecycleServer(t)
				cfg := s.Config.Get()
				cfg.Sources = withMovyDisabled([]Source{
					{Type: "addon", Name: "Slow", ManifestURL: "https://addon.test/slow/manifest.json"},
					{Type: "addon", Name: "First", ManifestURL: "https://addon.test/first/manifest.json"},
					{Type: "addon", Name: "Second", ManifestURL: "https://addon.test/second/manifest.json"},
					{Type: "vixsrc", BaseURL: "https://vix.test", Disabled: true},
				})
				if err := s.Config.Save(cfg); err != nil {
					t.Fatal(err)
				}
				// All requests use the virtual clock, including the real 40-second
				// fetch deadline. No live provider or wall-clock sleep is needed.
				synctest.Test(t, func(t *testing.T) {
					// Cancel lookup/session contexts inside their clock bubble.
					defer s.Close()
					want := []string{
						`{"name":"first","externalUrl":"https://player.test/first","custom":{"keep":17}}`,
						`{"name":"second","infoHash":"abcdef","unknown":42}`,
					}
					var fetchErr error
					s.Net.Client.Transport = movyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						status, body := http.StatusOK, ""
						switch r.URL.Path {
						case "/slow/stream/movie/tmdb:603.json":
							<-r.Context().Done()
							fetchErr = r.Context().Err()
							return nil, fetchErr
						case "/first/stream/movie/tmdb:603.json":
							body = `{"streams":[` + want[0] + `]}`
						case "/second/stream/movie/tmdb:603.json":
							body = `{"streams":[` + want[1] + `]}`
						case "/slow/meta/movie/tmdb:603.json":
							body = `{"meta":{"name":"Fixture"}}`
						case "/api/movie/603":
							status = http.StatusNotFound
						default:
							t.Errorf("unexpected discovery request: %s", r.URL)
							status = http.StatusNotFound
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
					})
					r := httptest.NewRequest("GET", "/stream/movie/tmdb:603.json", nil)
					if dashboard {
						r = httptest.NewRequest("POST", "/api/resolve", strings.NewReader(`{"type":"movie","id":"tmdb:603"}`))
						r.Header.Set("Content-Type", "application/json")
					}
					ctx, cancel := context.WithCancel(r.Context())
					defer cancel()
					w, done := httptest.NewRecorder(), make(chan struct{})
					started := time.Now()
					go func() { s.ServeHTTP(w, r.WithContext(ctx)); close(done) }()
					synctest.Wait()
					var selected *Session
					switch outcome {
					case "request canceled":
						cancel()
					case "lookup replaced":
						s.beginLookup()
					case "playback selected":
						c, _ := ParseContent("movie", "tmdb:603")
						selected = s.newSession(c, Stream{Name: "selected"})
						if !s.playerRequest(selected, httptest.NewRequest("GET", "/master.m3u8", nil)) {
							t.Fatal("playback selection failed")
						}
					}
					<-done
					synctest.Wait()
					var response struct {
						Streams []json.RawMessage `json:"streams"`
					}
					if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil {
						t.Fatal(w.Code, w.Body.String())
					}
					if outcome == "deadline" {
						if fetchErr != context.DeadlineExceeded || time.Since(started) != 40*time.Second {
							t.Fatal("slow provider did not exhaust the fetch deadline", fetchErr, time.Since(started))
						}
						if len(response.Streams) != len(want) || len(s.sessions) != len(want) {
							t.Fatal("fetch deadline discarded healthy results", w.Body.String())
						}
						for i, stream := range response.Streams {
							if !reflect.DeepEqual(jsonValue(t, stream), jsonValue(t, []byte(want[i]))) {
								t.Fatal("healthy stream changed or lost source order", string(stream))
							}
						}
						for _, v := range s.sessions {
							if v.ctx.Err() != nil || v.preparationStarted {
								t.Fatal("returned result was canceled or prepared during listing")
							}
						}
					} else {
						if fetchErr != context.Canceled || len(response.Streams) != 0 {
							t.Fatal("obsolete lookup returned results", fetchErr, w.Body.String())
						}
						if selected == nil {
							if len(s.sessions) != 0 {
								t.Fatal("obsolete lookup published sessions")
							}
						} else if len(s.sessions) != 1 || s.sessions[selected.ID] != selected || selected.ctx.Err() != nil {
							t.Fatal("obsolete lookup interrupted selected playback")
						}
					}
				})
			})
		}
	}
}
