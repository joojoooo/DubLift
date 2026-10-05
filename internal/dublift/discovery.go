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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
	"github.com/bluenviron/gohlslib/v2/pkg/playlist/primitives"
)

type vixBundle struct {
	tracks  []Track
	english *Track
	master  *HLS
	err     error
}

func (s *Server) resolveVixBundle(ctx context.Context, c Content) vixBundle {
	cfg := s.Config.Get()
	id, err := s.Net.ResolveTMDB(ctx, c, cfg.TMDBToken)
	if err != nil {
		return vixBundle{err: err}
	}
	origin, err := s.Net.ResolveVix(ctx, cfg.Source("vixsrc").BaseURL, c.Type, id, c.Season, c.Episode)
	if err != nil {
		return vixBundle{err: err}
	}
	h, err := s.Net.LoadHLS(ctx, origin)
	if err != nil {
		return vixBundle{err: err}
	}
	for _, binary := range []string{cfg.FFmpeg, cfg.FFprobe} {
		if _, err := exec.LookPath(binary); err != nil {
			return vixBundle{master: h, err: errors.New("FFmpeg/ffprobe unavailable")}
		}
	}
	v := &Session{}
	err = s.vixTracks(ctx, v, h)
	return vixBundle{tracks: v.tracks, english: v.vixEnglish, master: h, err: err}
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
		variant, err = nativeVariant(master, stream.variantURL)
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
	s.listStreams(w, r, c, "")
}

func (s *Server) listStreams(w http.ResponseWriter, r *http.Request, c Content, name string) {
	generation, lookup := s.beginLookup()
	ctx, cancel := context.WithCancel(lookup)
	stop := context.AfterFunc(r.Context(), cancel)
	defer func() { stop(); cancel() }()
	// Discovery reads source listings, VixSrc quality metadata and Italian audio.
	// Video probing, indexing and decoding wait for preparation or playback.
	fetchCtx, fetchCancel := context.WithTimeout(ctx, 40*time.Second)
	defer fetchCancel()
	cfg := s.Config.Get()
	type providerResult struct {
		streams []Stream
		err     error
	}
	providers := make([]providerResult, len(cfg.Sources))
	var wg sync.WaitGroup
	for i, source := range cfg.Sources {
		if source.Disabled || (source.Type != "addon" && source.Type != "movy") {
			continue
		}
		wg.Go(func() {
			if source.Type == "movy" {
				id, err := s.Net.ResolveTMDB(fetchCtx, c, cfg.TMDBToken)
				if err == nil {
					providers[i].streams, err = s.Net.ResolveMovy(fetchCtx, source.BaseURL, c, id)
				}
				providers[i].err = err
				return
			}
			u, _ := url.Parse(source.ManifestURL)
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
			providers[i] = providerResult{response.Streams, e}
		})
	}
	bundleReady := make(chan vixBundle, 1)
	go func() {
		work, done := context.WithTimeout(fetchCtx, 15*time.Second)
		defer done()
		bundleReady <- s.resolveVixBundle(work, c)
	}()
	wg.Wait()
	bundle := <-bundleReady
	fetchCancel()
	type listedStream struct {
		stream Stream
		native *nativePlayback
		source dashboardSource
	}
	var upstream []listedStream
	for i, source := range cfg.Sources {
		if source.Disabled {
			continue
		}
		provider := source.dashboardSource()
		if source.Type == "vixsrc" {
			for _, quality := range vixQualityStreams(bundle.master) {
				upstream = append(upstream, listedStream{stream: quality.stream, native: quality.native, source: provider})
			}
			continue
		}
		result := providers[i]
		if result.err != nil {
			s.event(source.Name + ": " + result.err.Error())
		}
		for _, stream := range result.streams {
			if stream.ExternalURL != "https://pengu.uk/donate" {
				upstream = append(upstream, listedStream{stream: stream, source: provider})
			}
		}
	}
	// Native stream titles describe quality, not the content. Use one title
	// for the entire lookup, including native qualities and addon results.
	if name = strings.TrimSpace(name); name == "" {
		var candidates []Stream
		for _, item := range upstream {
			if item.native == nil {
				candidates = append(candidates, item.stream)
			}
		}
		name = fallbackContentName(c, candidates...)
	} else {
		name = contentHeading(c, name)
	}
	results := make([]Stream, len(upstream))
	s.mu.Lock()
	if generation != s.generation || ctx.Err() != nil {
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
	for i, item := range upstream {
		stream := item.stream
		v := s.newSessionLocked(c, stream)
		v.SourceID, v.SourceName = item.source.ID, item.source.Name
		v.native = item.native
		if item.native != nil {
			v.nativeMaster = bundle.master
		}
		v.listedReady = make(chan struct{})
		v.Order = i
		v.ContentName = name
		v.ticket = s.sealPlayback(v)
		v.Status = "Waiting in source order"
		sessions[i] = v
	}
	s.mu.Unlock()
	italianResults := 0
	for i, item := range upstream {
		stream := item.stream
		results[i] = stream
		v := sessions[i]
		v.mu.Lock()
		_, urlErr := httpURL(stream.URL)
		if item.native != nil {
			v.Status = "Source audio and video · ready to prepare or play"
			results[i] = stream.localPlayback(s.playbackURL(r, v), item.native.Italian)
			if item.native.Italian {
				italianResults++
			}
		} else if bundle.err == nil && urlErr == nil {
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
	current := generation == s.generation && ctx.Err() == nil
	if generation == s.generation && !current {
		for _, v := range sessions {
			s.discardLocked(v)
		}
	}
	if current {
		if bundle.err != nil {
			s.lookupStatus = fmt.Sprintf("Italian audio unavailable for dubbing · %d streams returned in source order", len(results))
		} else {
			s.lookupStatus = fmt.Sprintf("%d Italian results · %d streams returned in source order", italianResults, len(results))
		}
	}
	s.mu.Unlock()
	if !current {
		results = []Stream{}
	} else {
		go s.resolveContentName(c, cfg, sessions)
	}
	jsonResponse(w, 200, map[string]any{"streams": results, "cacheMaxAge": 0, "staleRevalidate": 0, "staleError": 0})
}

var contentYearRE = regexp.MustCompile(`\s+\((?:18|19|20|21)\d{2}\)$`)

func fallbackContentName(c Content, streams ...Stream) string {
	for _, stream := range streams {
		for _, value := range []string{stream.Title, stream.Description} {
			line := strings.TrimSpace(strings.Split(strings.TrimSpace(value), "\n")[0])
			line = strings.TrimLeftFunc(line, func(r rune) bool {
				return unicode.IsSpace(r) || unicode.Is(unicode.So, r) || r == '\uFE0F' || r == '\u200D' || r >= '\U0001F3FB' && r <= '\U0001F3FF'
			})
			line = contentYearRE.ReplaceAllString(line, "")
			if line != "" {
				return contentHeading(c, line)
			}
		}
	}
	return contentHeading(c, strings.ToUpper(c.Type[:1])+c.Type[1:]+" · "+c.BaseID)
}

func contentHeading(c Content, name string) string {
	if c.Type == "series" {
		episode := fmt.Sprintf("S%02dE%02d", c.Season, c.Episode)
		if !strings.HasSuffix(name, episode) {
			name += " · " + episode
		}
	}
	return name
}

func (s *Server) resolveContentName(c Content, cfg Settings, sessions []*Session) {
	if len(sessions) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	defer cancel()
	// Keep title lookup alive for a selected stream, but cancel its reads once
	// all results it belongs to have been discarded.
	var remaining atomic.Int32
	remaining.Store(int32(len(sessions)))
	for _, v := range sessions {
		stop := context.AfterFunc(v.ctx, func() {
			if remaining.Add(-1) == 0 {
				cancel()
			}
		})
		defer stop()
	}
	urls := []Origin{}
	if c.TMDB != "" && cfg.TMDBToken != "" {
		kind := "movie"
		if c.Type == "series" {
			kind = "tv"
		}
		urls = append(urls, Origin{URL: "https://api.themoviedb.org/3/" + kind + "/" + c.TMDB, Headers: http.Header{"Authorization": {"Bearer " + cfg.TMDBToken}}})
	}
	for _, a := range cfg.Sources {
		if a.Type != "addon" || a.Disabled {
			continue
		}
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
		name := strings.TrimSpace(response.Meta.Name)
		if name == "" {
			name = strings.TrimSpace(response.Title)
		}
		if name == "" {
			name = strings.TrimSpace(response.Name)
		}
		if name == "" {
			continue
		}
		name = contentHeading(c, name)
		s.mu.Lock()
		for _, v := range sessions {
			if ctx.Err() == nil && v.ctx.Err() == nil && s.sessions[v.ID] == v {
				v.mu.Lock()
				v.ContentName = name
				// Update the immutable ticket snapshot: native preparation may
				// already be refreshing the live source's selected variant.
				if ticket, err := s.readPlayback(v.ID, v.ticket); err == nil {
					ticket.ContentName = name
					v.ticket = s.sealPlaybackTicket(ticket)
				}
				v.mu.Unlock()
			}
		}
		s.mu.Unlock()
		return
	}
}
