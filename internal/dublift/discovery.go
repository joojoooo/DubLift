package dublift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
	"github.com/bluenviron/gohlslib/v2/pkg/playlist/primitives"
)

type vixBundle struct {
	tracks  []Track
	english *Track
	err     error
}

func (s *Server) resolveVixBundle(ctx context.Context, c Content) vixBundle {
	cfg := s.Config.Get()
	for _, binary := range []string{cfg.FFmpeg, cfg.FFprobe} {
		if _, err := exec.LookPath(binary); err != nil {
			return vixBundle{err: errors.New("FFmpeg/ffprobe unavailable")}
		}
	}
	id, err := s.Net.ResolveTMDB(ctx, c, cfg.TMDBToken)
	if err != nil {
		return vixBundle{err: err}
	}
	origin, err := s.Net.ResolveVix(ctx, cfg.VixBaseURL, c.Type, id, c.Season, c.Episode)
	if err != nil {
		return vixBundle{err: err}
	}
	h, err := s.Net.LoadHLS(ctx, origin)
	if err != nil {
		return vixBundle{err: err}
	}
	v := &Session{}
	err = s.vixTracks(ctx, v, h)
	return vixBundle{v.tracks, v.vixEnglish, err}
}

func (s *Server) inspectSource(ctx context.Context, stream Stream) (*Asset, *HLS, *playlist.MultivariantVariant, error) {
	a, err := s.Engine.OpenAsset(ctx, stream.Origin(), isHLSURL(stream.URL, stream.BehaviorHints.Filename))
	if err != nil {
		return nil, nil, nil, err
	}
	var master *HLS
	var variant *playlist.MultivariantVariant
	if a.HLS != nil && a.HLS.Master != nil {
		master = a.HLS
		variant, err = master.BestVariant()
		if err != nil {
			return nil, nil, nil, err
		}
		var origin Origin
		origin, err = master.Child(variant.URI)
		if err != nil {
			return nil, nil, nil, err
		}
		a, err = s.loadMedia(ctx, origin)
	}
	if err == nil && a.HLS != nil {
		err = s.checkVideoResources(ctx, a)
	}
	return a, master, variant, err
}

// A valid manifest is not evidence that its video still exists. Check only
// tiny prefixes of the first media resource and its initialization map during
// preparation; broken origins must not become audio-only streams.
func (s *Server) checkVideoResources(ctx context.Context, a *Asset) error {
	if len(a.HLS.Segments) == 0 {
		return errors.New("source video playlist is empty")
	}
	seg := a.HLS.Segments[0]
	type prefix struct{ url, byteRange string }
	resources := []prefix{{seg.URI, seg.Range}}
	if seg.Map != "" {
		var attrs primitives.Attributes
		if err := attrs.Unmarshal(strings.TrimPrefix(seg.Map, "#EXT-X-MAP:")); err != nil {
			return err
		}
		u, err := resolveURL(a.HLS.Origin.URL, attrs["URI"])
		if err != nil {
			return err
		}
		resources = append(resources, prefix{u, attrs["BYTERANGE"]})
	}
	for _, resource := range resources {
		start, length := int64(0), int64(512)
		if resource.byteRange != "" {
			var size int64
			if _, err := fmt.Sscanf(resource.byteRange, "%d@%d", &size, &start); err != nil || size <= 0 || start < 0 {
				return errors.New("invalid source byte range")
			}
			length = min(length, size)
		}
		rangeHeader := fmt.Sprintf("bytes=%d-%d", start, start+length-1)
		resp, err := s.Net.request(ctx, Origin{resource.url, a.Origin.Headers}, "GET", rangeHeader)
		if err != nil {
			return fmt.Errorf("source video check: %w", err)
		}
		if resp.StatusCode != 200 && resp.StatusCode != 206 {
			resp.Body.Close()
			return fmt.Errorf("source video unavailable: origin HTTP %d", resp.StatusCode)
		}
		if resource.byteRange != "" && resp.StatusCode != 206 {
			resp.Body.Close()
			return errors.New("source video origin ignores required byte ranges")
		}
		if resp.StatusCode == 200 && resp.ContentLength > segmentLimit {
			resp.Body.Close()
			return errors.New("source video segment exceeds the bounded read limit")
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, length))
		resp.Body.Close()
		s.Net.Bytes.Add(int64(len(data)))
		contentType := strings.ToLower(resp.Header.Get("Content-Type"))
		if readErr != nil || len(data) == 0 || strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/json") {
			return errors.New("source video resource is unavailable or returned an error page")
		}
	}
	return nil
}

func (s *Server) streams(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/stream/"), "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".json") {
		http.NotFound(w, r)
		return
	}
	c, err := ParseContent(parts[0], strings.TrimSuffix(parts[1], ".json"))
	if err != nil {
		failure(w, 400, err)
		return
	}
	generation, lookup := s.beginLookup()
	ctx, cancel := context.WithCancel(lookup)
	stop := context.AfterFunc(r.Context(), cancel)
	defer func() { stop(); cancel() }()
	// Discovery fetches upstream results and Vixsrc audio only. Source media
	// is inspected when the user prepares or plays a selected stream.
	fetchCtx, fetchCancel := context.WithTimeout(ctx, 40*time.Second)
	defer fetchCancel()
	cfg := s.Config.Get()
	type addonResult struct {
		streams []Stream
		err     error
	}
	addons := make([]addonResult, len(cfg.Addons))
	var wg sync.WaitGroup
	for i, addon := range cfg.Addons {
		wg.Go(func() {
			u, _ := url.Parse(addon.ManifestURL)
			id := c.ID
			if c.TMDB != "" && !strings.HasPrefix(id, "tmdb:") {
				id = "tmdb:" + id
			}
			u.Path = strings.TrimSuffix(u.Path, "/manifest.json") + "/stream/" + c.Type + "/" + id + ".json"
			u.RawPath = ""
			b, _, e := s.Net.Fetch(fetchCtx, Origin{u.String(), http.Header{"User-Agent": {userAgent}}}, manifestLimit)
			var response struct {
				Streams []Stream `json:"streams"`
			}
			if e == nil {
				e = json.Unmarshal(b, &response)
			}
			addons[i] = addonResult{response.Streams, e}
		})
	}
	bundleReady := make(chan vixBundle, 1)
	go func() {
		work, done := context.WithTimeout(fetchCtx, 15*time.Second)
		defer done()
		bundleReady <- s.resolveVixBundle(work, c)
	}()
	wg.Wait()
	var upstream []Stream
	for i, result := range addons {
		if result.err != nil {
			s.event(cfg.Addons[i].Name + ": " + result.err.Error())
		}
		for _, stream := range result.streams {
			if stream.ExternalURL != "https://pengu.uk/donate" {
				upstream = append(upstream, stream)
			}
		}
	}
	bundle := <-bundleReady
	fetchCancel()
	results := append([]Stream{}, upstream...)
	s.mu.Lock()
	if generation != s.generation {
		s.mu.Unlock()
		jsonResponse(w, 200, map[string]any{"streams": []Stream{}, "cacheMaxAge": 0, "staleRevalidate": 0, "staleError": 0})
		return
	}
	if bundle.err != nil {
		s.lookupStatus = "Vixsrc Italian audio unavailable; returning original links…"
	} else {
		s.lookupStatus = "Vixsrc Italian audio confirmed; returning streams…"
	}
	sessions := make([]*Session, len(upstream))
	for i, stream := range upstream {
		v := s.newSessionLocked(c, stream)
		v.listedReady = make(chan struct{})
		v.Order = i
		v.ticket = s.sealPlayback(v)
		v.Status = "Waiting in upstream order"
		v.ContentName = fallbackContentName(c, stream)
		sessions[i] = v
	}
	s.mu.Unlock()
	italianResults := 0
	for i, stream := range upstream {
		v := sessions[i]
		v.mu.Lock()
		_, urlErr := httpURL(stream.URL)
		if bundle.err == nil && urlErr == nil {
			v.listedVix, v.listedEnglish = bundle.tracks, bundle.english
			v.Status = "Italian audio found · ready to prepare or play"
			results[i] = stream.dubbed(s.playbackURL(r, v))
			italianResults++
		} else {
			v.Passthrough = true
			v.FallbackReason = "Upstream torrent or external stream"
			if bundle.err != nil {
				v.FallbackReason = bundle.err.Error()
			}
			v.Status = "Original stream · unchanged upstream link"
		}
		v.mu.Unlock()
		close(v.listedReady)
	}
	s.mu.Lock()
	current := generation == s.generation
	if current {
		if bundle.err != nil {
			s.lookupStatus = fmt.Sprintf("Vixsrc Italian audio unavailable · %d original streams returned", len(results))
		} else {
			s.lookupStatus = fmt.Sprintf("%d Italian results · %d streams returned in upstream order", italianResults, len(results))
		}
	}
	s.mu.Unlock()
	if !current {
		results = []Stream{}
	} else {
		go s.resolveContentName(c, cfg)
	}
	jsonResponse(w, 200, map[string]any{"streams": results, "cacheMaxAge": 0, "staleRevalidate": 0, "staleError": 0})
}

func fallbackContentName(c Content, stream Stream) string {
	for _, value := range []string{stream.Title, stream.Description} {
		line := strings.TrimSpace(strings.Split(value, "\n")[0])
		if line != "" {
			return line
		}
	}
	return strings.ToUpper(c.Type[:1]) + c.Type[1:] + " · " + c.BaseID
}

func (s *Server) resolveContentName(c Content, cfg Settings) {
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	defer cancel()
	urls := []Origin{}
	if c.TMDB != "" && cfg.TMDBToken != "" {
		kind := "movie"
		if c.Type == "series" {
			kind = "tv"
		}
		urls = append(urls, Origin{URL: "https://api.themoviedb.org/3/" + kind + "/" + c.TMDB, Headers: http.Header{"Authorization": {"Bearer " + cfg.TMDBToken}}})
	}
	for _, a := range cfg.Addons {
		u, err := httpURL(a.ManifestURL)
		if err != nil {
			continue
		}
		id := c.BaseID
		if c.TMDB != "" {
			id = "tmdb:" + c.TMDB
		}
		u.Path = strings.TrimSuffix(u.Path, "/manifest.json") + "/meta/" + c.Type + "/" + id + ".json"
		u.RawPath = ""
		urls = append(urls, Origin{u.String(), http.Header{"User-Agent": {userAgent}}})
	}
	if c.TMDB == "" {
		urls = append(urls, Origin{URL: "https://v3-cinemeta.strem.io/meta/" + c.Type + "/" + c.BaseID + ".json"})
	}
	for _, origin := range urls {
		work, done := context.WithTimeout(ctx, 2*time.Second)
		b, _, err := s.Net.Fetch(work, origin, manifestLimit)
		done()
		if err != nil {
			continue
		}
		var response struct {
			Title string `json:"title"`
			Name  string `json:"name"`
			Meta  struct {
				Name string `json:"name"`
			} `json:"meta"`
		}
		if json.Unmarshal(b, &response) != nil {
			continue
		}
		name := response.Meta.Name
		if name == "" {
			name = response.Title
		}
		if name == "" {
			name = response.Name
		}
		if name == "" {
			continue
		}
		if c.Type == "series" {
			name += fmt.Sprintf(" · S%02dE%02d", c.Season, c.Episode)
		}
		s.mu.Lock()
		for _, v := range s.sessions {
			if v.Content.ID == c.ID && v.Content.Type == c.Type {
				v.mu.Lock()
				v.ContentName = name
				v.mu.Unlock()
			}
		}
		s.mu.Unlock()
		return
	}
}
