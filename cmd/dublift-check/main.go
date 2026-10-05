// dublift-check is an explicit, read-only live provider check. Normal tests
// never depend on these titles or on third-party availability.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"dublift/internal/dublift"
)

type title struct{ ID, Name, Type string }

var titles = []title{{"81349", "Devs", "series"}, {"299939", "Monster: The Lizzie Borden Story", "series"}, {"95350", "Lanterns", "series"}, {"94997", "House of the Dragon", "series"}, {"94605", "Arcane", "series"}, {"687163", "Project Hail Mary", "movie"}, {"1204680", "Coyote vs. Acme", "movie"}, {"603", "The Matrix", "movie"}, {"27205", "Inception", "movie"}, {"157336", "Interstellar", "movie"}}

type result struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Streams        int      `json:"httpStreams"`
	HLS            int      `json:"hlsHints"`
	UpstreamErrors []string `json:"upstreamErrors,omitempty"`
	VixLanguages   []string `json:"vixLanguages,omitempty"`
	VixError       string   `json:"vixError,omitempty"`
}

func main() {
	path := flag.String("config", ".local/config.json", "private config path")
	out := flag.String("out", ".local/live-report.json", "local report path")
	flag.Parse()
	cfg, e := dublift.OpenConfig(*path)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	settings := cfg.Get()
	network := dublift.NewNetwork()
	results := make([]result, len(titles))
	slots := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, t := range titles {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			r := result{ID: t.ID, Name: t.Name, Type: t.Type}
			id := "tmdb:" + t.ID
			season, episode := 0, 0
			if t.Type == "series" {
				id += ":1:1"
				season = 1
				episode = 1
			}
			for _, addon := range settings.Sources {
				if addon.Type != "addon" || addon.Disabled {
					continue
				}
				u, e := url.Parse(addon.ManifestURL)
				if e != nil {
					continue
				}
				u.Path = strings.TrimSuffix(u.Path, "/manifest.json") + "/stream/" + t.Type + "/" + id + ".json"
				u.RawPath = ""
				b, _, e := network.Fetch(ctx, dublift.Origin{URL: u.String(), Headers: http.Header{"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"}}}, 4<<20)
				if e != nil {
					r.UpstreamErrors = append(r.UpstreamErrors, addon.Name+": "+e.Error())
					continue
				}
				var data struct {
					Streams []dublift.Stream `json:"streams"`
				}
				if e = json.Unmarshal(b, &data); e != nil {
					r.UpstreamErrors = append(r.UpstreamErrors, "invalid upstream JSON")
					continue
				}
				for _, s := range data.Streams {
					if strings.HasPrefix(s.URL, "https://") || strings.HasPrefix(s.URL, "http://") {
						r.Streams++
						if strings.Contains(strings.ToLower(s.URL), ".m3u8") || strings.HasSuffix(strings.ToLower(s.BehaviorHints.Filename), ".m3u8") {
							r.HLS++
						}
					}
				}
			}
			origin, e := network.ResolveVix(ctx, settings.Source("vixsrc").BaseURL, t.Type, t.ID, season, episode)
			if e == nil {
				h, err := network.LoadHLS(ctx, origin)
				e = err
				if e == nil && h.Master != nil {
					seen := map[string]bool{}
					for _, track := range h.Master.Renditions {
						if string(track.Type) == "AUDIO" && !seen[track.Language] {
							seen[track.Language] = true
							r.VixLanguages = append(r.VixLanguages, track.Language)
						}
					}
				}
			}
			if e != nil {
				r.VixError = e.Error()
			}
			results[i] = r
		})
	}
	wg.Wait()
	report := struct {
		Checked time.Time `json:"checked"`
		Scope   string    `json:"scope"`
		Results []result  `json:"results"`
	}{time.Now(), "Manifests and stream lists only; no complete media downloaded", results}
	b, _ := json.MarshalIndent(report, "", "  ")
	if e = os.WriteFile(*out, append(b, '\n'), 0600); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	fmt.Println(string(b))
}
