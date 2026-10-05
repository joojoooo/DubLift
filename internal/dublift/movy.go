package dublift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

const movyAPIURL = "https://api.wecollege.net"
const movySubtitleURL = "https://subtitles.vidy.st"
const movyDefaultURL = "https://www.movy.sx"
const movyMinHeight = 1080
const movyQuickScrapeTimeout = 8 * time.Second

var movyNextDataRE = regexp.MustCompile(`(?is)<script\b[^>]*\bid=["']__NEXT_DATA__["'][^>]*>(.*?)</script>`)
var movyScriptRE = regexp.MustCompile(`(?i)\bsrc=["']([^"']+\.js(?:\?[^"']*)?)["']`)
var movyRouteRE = regexp.MustCompile(`/([a-z]{2,24})/sources\b`)
var movyHeightRE = regexp.MustCompile(`(?i)(\d{3,4})\s*p\b`)
var movyEmbedRE = regexp.MustCompile(`(?i)/embed(?:/|\?|$)`)
var movyQualityNames = []struct {
	pattern *regexp.Regexp
	height  int
}{
	{regexp.MustCompile(`(?i)\b8k\b`), 4320},
	{regexp.MustCompile(`(?i)\b(?:4k|uhd)\b`), 2160},
	{regexp.MustCompile(`(?i)\b(?:2k|qhd)\b`), 1440},
	{regexp.MustCompile(`(?i)\b(?:full\s*hd|fhd)\b`), 1080},
	{regexp.MustCompile(`(?i)\bhd\b`), 720},
	{regexp.MustCompile(`(?i)\bsd\b`), 480},
}

type movyRegistry struct {
	base    string
	servers []string
	expires time.Time
}

type movyDetails struct {
	TMDBID      json.Number       `json:"tmdbId"`
	Title       string            `json:"title"`
	TitleGerman string            `json:"titleGerman"`
	MediaType   string            `json:"mediaType"`
	Year        json.Number       `json:"year"`
	IMDBID      string            `json:"imdbId"`
	Seasons     []json.RawMessage `json:"seasons"`
}

type movySource struct {
	URL        string            `json:"url"`
	Type       string            `json:"type"`
	Quality    string            `json:"quality"`
	Headers    map[string]string `json:"headers"`
	DRM        json.RawMessage   `json:"drm"`
	KeySystem  json.RawMessage   `json:"keySystem"`
	LicenseURL json.RawMessage   `json:"licenseUrl"`
	License    json.RawMessage   `json:"license"`
}

type movySubtitle struct {
	URL      string `json:"url"`
	Lang     string `json:"lang"`
	Language string `json:"language"`
}

type movyPayload struct {
	Sources   []movySource   `json:"sources"`
	Subtitles []movySubtitle `json:"subtitles"`
}

type movyChoice struct {
	stream Stream
	height int
}

func movyHeaders(base, referer string) http.Header {
	u, _ := url.Parse(base)
	return http.Header{"User-Agent": {userAgent}, "Referer": {referer}, "Origin": {u.Scheme + "://" + u.Host}, "Cache-Control": {"no-cache"}}
}

func (n *Network) fetchMovy(ctx context.Context, raw string, headers http.Header) ([]byte, error) {
	work, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	b, _, err := n.Fetch(work, Origin{raw, headers}, manifestLimit)
	return b, err
}

func parseMovyPage(data []byte, kind, id string) (movyDetails, error) {
	var page struct {
		Props struct {
			PageProps struct {
				Details movyDetails `json:"details"`
			} `json:"pageProps"`
		} `json:"props"`
	}
	match := movyNextDataRE.FindSubmatch(data)
	if len(match) == 0 || json.Unmarshal(match[1], &page) != nil {
		return movyDetails{}, errors.New("Movy page data unavailable")
	}
	details := page.Props.PageProps.Details
	if details.TMDBID.String() != id || details.MediaType != kind || strings.TrimSpace(details.Title) == "" {
		return movyDetails{}, errors.New("Movy metadata does not match the requested title")
	}
	return details, nil
}

func (n *Network) movyServers(ctx context.Context, base, page string, data []byte) ([]string, error) {
	n.movyMu.Lock()
	cached := n.movyRegistry
	n.movyMu.Unlock()
	if cached.base == base && time.Now().Before(cached.expires) {
		return append([]string{}, cached.servers...), ctx.Err()
	}
	var scripts []string
	seen := map[string]bool{}
	for _, match := range movyScriptRE.FindAllSubmatch(data, -1) {
		raw, err := resolveURL(base+"/", html.UnescapeString(string(match[1])))
		if err != nil || !strings.HasPrefix(raw, base+"/_next/static/") || seen[raw] {
			continue
		}
		seen[raw] = true
		scripts = append(scripts, raw)
		if len(scripts) == 40 {
			break
		}
	}
	for offset := 0; offset < len(scripts); offset += 4 {
		chunks := make([][]byte, min(4, len(scripts)-offset))
		var wg sync.WaitGroup
		for i := range chunks {
			wg.Go(func() { chunks[i], _ = n.fetchMovy(ctx, scripts[offset+i], movyHeaders(base, page)) })
		}
		wg.Wait()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		for _, chunk := range chunks {
			var servers []string
			seen := map[string]bool{}
			for _, match := range movyRouteRE.FindAllSubmatch(chunk, -1) {
				server := string(match[1])
				if !seen[server] {
					seen[server] = true
					servers = append(servers, server)
				}
				if len(servers) == 32 {
					break
				}
			}
			if len(servers) > 0 {
				n.movyMu.Lock()
				if ctx.Err() == nil {
					n.movyRegistry = movyRegistry{base: base, servers: servers, expires: time.Now().Add(30 * time.Minute)}
				}
				n.movyMu.Unlock()
				return servers, ctx.Err()
			}
		}
	}
	return nil, errors.New("Movy player server registry was not found")
}

func (n *Network) movySeed(ctx context.Context, id, base string) (string, error) {
	b, err := n.fetchMovy(ctx, movyAPIURL+"/seed?mediaId="+id, movyHeaders(base, base+"/"))
	if err != nil {
		return "", err
	}
	var response struct {
		Seed string `json:"seed"`
	}
	if json.Unmarshal(b, &response) != nil || response.Seed == "" || len(response.Seed) > 4096 {
		return "", errors.New("missing Movy seed")
	}
	return response.Seed, nil
}

// JavaScript's encodeURIComponent differs from query encoding, particularly
// for spaces. Movy's title and alternate title intentionally get two passes.
func movyEncode(value string) string {
	var b strings.Builder
	for _, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.!~*'()", rune(c)) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (n *Network) movyServer(ctx context.Context, server, base, page, id, seed string, c Content, details movyDetails) ([]movyChoice, error) {
	season, episode := 1, 1
	if c.Type == "series" {
		season, episode = c.Season, c.Episode
	}
	kind := "movie"
	if c.Type == "series" {
		kind = "tv"
	}
	values := url.Values{
		"title": {movyEncode(details.Title)}, "mediaType": {kind}, "year": {details.Year.String()},
		"totalSeasons": {strconv.Itoa(len(details.Seasons))}, "episodeId": {strconv.Itoa(episode)},
		"seasonId": {strconv.Itoa(season)}, "tmdbId": {id}, "imdbId": {details.IMDBID}, "enc": {"2"}, "seed": {seed},
	}
	if server == "munich" {
		values.Set("language", "german")
	}
	if server == "berlin" && details.TitleGerman != "" {
		values.Set("altTitle", movyEncode(details.TitleGerman))
	}
	number, _ := strconv.ParseUint(id, 10, 32)
	for attempt := 0; attempt < 2; attempt++ {
		b, err := n.fetchMovy(ctx, movyAPIURL+"/"+server+"/sources?"+values.Encode(), movyHeaders(base, page))
		var status *HTTPError
		if errors.As(err, &status) && status.Status == http.StatusUnauthorized && attempt == 0 {
			seed, err = n.movySeed(ctx, id, base)
			if err != nil {
				return nil, err
			}
			values.Set("seed", seed)
			continue
		}
		if err != nil {
			return nil, err
		}
		payload := strings.TrimSpace(string(b))
		if strings.HasPrefix(payload, `"`) && json.Unmarshal(b, &payload) != nil {
			return nil, errors.New("invalid Movy source envelope")
		}
		data, err := decodeMovySources(payload, seed, uint32(number))
		if err != nil {
			return nil, err
		}
		choices := n.normalizeMovySources(ctx, data, server, base, page)
		for i := range choices {
			choices[i].stream.Title = details.Title + "\n" + choices[i].stream.Title
		}
		return choices, nil
	}
	return nil, errors.New("Movy seed expired")
}

func movyQuality(label string) (string, int) {
	label = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(label))
	if len(label) > 60 {
		label = string([]rune(label)[:min(60, len([]rune(label)))])
	}
	for _, suffix := range []string{" HLS", " DASH"} {
		if strings.HasSuffix(strings.ToUpper(label), suffix) {
			label = strings.TrimSpace(label[:len(label)-len(suffix)])
		}
	}
	lower := strings.ToLower(label)
	if label == "" || strings.Contains(lower, "auto") {
		return "Auto quality", 0
	}
	if match := movyHeightRE.FindStringSubmatch(label); match != nil {
		height, _ := strconv.Atoi(match[1])
		return fmt.Sprintf("%dp", height), height
	}
	if height, err := strconv.Atoi(label); err == nil && height >= 240 && height <= 9999 {
		return fmt.Sprintf("%dp", height), height
	}
	for _, quality := range movyQualityNames {
		if quality.pattern.MatchString(label) {
			return fmt.Sprintf("%dp", quality.height), quality.height
		}
	}
	return label, 0
}

func movyProtected(value json.RawMessage) bool {
	return len(value) > 0 && string(value) != "null" && string(value) != "false" && string(value) != `""` && string(value) != "0"
}

func (n *Network) normalizeMovySources(ctx context.Context, data movyPayload, server, base, page string) []movyChoice {
	var choices []movyChoice
	for _, source := range data.Sources[:min(40, len(data.Sources))] {
		u, err := httpURL(source.URL)
		if err != nil || movyEmbedRE.MatchString(source.URL) || movyProtected(source.DRM) || movyProtected(source.KeySystem) || movyProtected(source.LicenseURL) || movyProtected(source.License) {
			continue
		}
		_, height := movyQuality(source.Quality)
		if height > 0 && height < movyMinHeight {
			continue
		}
		headers := movyHeaders(base, page)
		for key, value := range source.Headers {
			key = http.CanonicalHeaderKey(key)
			if (key == "Referer" || key == "Origin" || key == "User-Agent" || key == "Cookie") && !strings.ContainsAny(value, "\r\n") {
				headers.Set(key, value)
			}
		}
		path := strings.ToLower(u.Path)
		if strings.EqualFold(source.Type, "dash") || strings.HasSuffix(path, ".mpd") {
			continue // DASH is outside DubLift's media pipeline.
		}
		format := ""
		if strings.HasSuffix(path, ".m3u8") || strings.Contains(strings.ToLower(source.Type+" "+source.Quality), "hls") {
			format = "HLS"
		} else if strings.HasSuffix(path, ".mp4") || strings.HasSuffix(path, ".mkv") || strings.HasSuffix(path, ".webm") {
			format = strings.ToUpper(strings.TrimPrefix(path[strings.LastIndex(path, "."):], "."))
		}
		create := func(quality, variantURL, technical string) movyChoice {
			label, height := movyQuality(quality)
			stream := Stream{URL: source.URL, Name: "Movy · " + label + " · 📡 " + strings.ToUpper(server[:1]) + server[1:], Title: "🎞️ " + technical, Headers: map[string]string{}, variantURL: variantURL}
			for key := range headers {
				stream.Headers[key] = headers.Get(key)
			}
			if format == "HLS" {
				stream.BehaviorHints.Filename = "movy.m3u8"
			}
			appendMovySubtitles(&stream, data.Subtitles)
			return movyChoice{stream, height}
		}
		if format != "" {
			choices = append(choices, create(source.Quality, "", format))
			continue
		}
		// The Nuvio scraper expands extensionless HLS URLs using bounded
		// playlist metadata only. Media reads and probing wait for preparation.
		work, cancel := context.WithTimeout(ctx, 12*time.Second)
		master, err := n.LoadHLS(work, Origin{source.URL, headers})
		cancel()
		if err != nil || master.Master == nil {
			continue
		}
		format = "HLS"
		seen := map[string]bool{}
		variants := append([]*playlist.MultivariantVariant{}, master.Master.Variants...)
		sort.SliceStable(variants, func(i, j int) bool {
			_, hi := variantSize(variants[i])
			_, hj := variantSize(variants[j])
			return hi > hj || hi == hj && variants[i].Bandwidth > variants[j].Bandwidth
		})
		for _, variant := range variants {
			width, height := variantSize(variant)
			if height > 0 && height < movyMinHeight {
				continue
			}
			quality := "Auto"
			if height > 0 {
				quality = fmt.Sprintf("%dp", height)
			}
			origin, err := master.Child(variant.URI)
			if err != nil || seen[quality] {
				continue
			}
			seen[quality] = true
			technical := "HLS"
			if height > 0 {
				technical = fmt.Sprintf("%d×%d · HLS", width, height)
			}
			if len(variant.Codecs) > 0 {
				technical += " · " + strings.Join(variant.Codecs, " / ")
			}
			choice := create(quality, origin.URL, technical)
			bitrate := variant.Bandwidth
			if variant.AverageBandwidth != nil {
				bitrate = *variant.AverageBandwidth
			}
			if label := formatBitrate(bitrate); label != "" {
				choice.stream.Name += " · " + label
			}
			choices = append(choices, choice)
		}
	}
	return choices
}

func appendMovySubtitles(stream *Stream, subtitles []movySubtitle) {
	seen := map[string]bool{}
	for _, subtitle := range stream.Subtitles {
		seen[subtitle.URL+"|"+subtitle.Lang] = true
	}
	for _, subtitle := range subtitles[:min(200, len(subtitles))] {
		if len(stream.Subtitles) >= 200 {
			break
		}
		if _, err := httpURL(subtitle.URL); err != nil {
			continue
		}
		lang := subtitle.Lang
		if lang == "" {
			lang = subtitle.Language
		}
		lang = language(strings.ToLower(strings.TrimSpace(lang)))
		if lang == "" {
			lang = "und"
		}
		key := subtitle.URL + "|" + lang
		if seen[key] {
			continue
		}
		seen[key] = true
		stream.Subtitles = append(stream.Subtitles, struct {
			ID   string `json:"id"`
			URL  string `json:"url"`
			Lang string `json:"lang"`
		}{ID: "movy-" + identity(key)[:16], URL: subtitle.URL, Lang: lang})
	}
}

// ResolveMovy ports the title-page metadata, dynamic server registry, seed
// retry and source decoder from the local Nuvio provider. Four workers seek
// both 4K and 1080p, returning available results after eight seconds of server
// discovery. With no results, requests continue under the caller's deadline.
func (n *Network) ResolveMovy(ctx context.Context, base string, c Content, id string) ([]Stream, error) {
	if _, err := httpURL(base); err != nil {
		return nil, err
	}
	if !numericRE.MatchString(id) {
		return nil, errors.New("Movy requires a numeric TMDB ID")
	}
	if _, err := strconv.ParseUint(id, 10, 32); err != nil {
		return nil, errors.New("invalid Movy TMDB ID")
	}
	base = strings.TrimRight(base, "/")
	kind, path := "movie", "/movie/"+id
	if c.Type == "series" {
		kind, path = "tv", fmt.Sprintf("/tv/%s/%d/%d", id, c.Season, c.Episode)
	}
	page := base + path + "?play=true"
	b, err := n.fetchMovy(ctx, page, movyHeaders(base, base+"/"))
	if err != nil {
		return nil, fmt.Errorf("Movy page: %w", err)
	}
	details, err := parseMovyPage(b, kind, id)
	if err != nil {
		return nil, err
	}
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	var servers []string
	var seed string
	var registryErr, seedErr error
	var subtitles []movySubtitle
	subtitleReady := make(chan []movySubtitle, 1)
	var wg sync.WaitGroup
	wg.Go(func() { servers, registryErr = n.movyServers(work, base, page, b) })
	wg.Go(func() { seed, seedErr = n.movySeed(work, id, base) })
	go func() {
		var found []movySubtitle
		u := movySubtitleURL + "/search?id=" + id
		if kind == "tv" {
			u += fmt.Sprintf("&season=%d&episode=%d", c.Season, c.Episode)
		}
		if data, err := n.fetchMovy(work, u, movyHeaders(base, base+"/")); err == nil {
			_ = json.Unmarshal(data, &found)
		}
		subtitleReady <- found
	}()
	wg.Wait()
	if registryErr != nil {
		return nil, registryErr
	}
	if seedErr != nil {
		return nil, fmt.Errorf("Movy seed: %w", seedErr)
	}
	type result struct {
		index   int
		choices []movyChoice
		err     error
	}
	results := make(chan result, 4)
	groups := make([][]movyChoice, len(servers))
	started := time.Now()
	timer := time.NewTimer(movyQuickScrapeTimeout)
	defer timer.Stop()
	var stop <-chan time.Time
	next, active, found := 0, 0, 0
	heights := map[int]bool{}
	var lastErr error
	scrapeCtx, stopScrape := context.WithCancel(work)
	defer stopScrape()
	for next < len(servers) || active > 0 {
		for active < 4 && next < len(servers) {
			index := next
			next++
			active++
			go func() {
				choices, err := n.movyServer(scrapeCtx, servers[index], base, page, id, seed, c, details)
				results <- result{index, choices, err}
			}()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-stop:
			next, active = len(servers), 0
		case result := <-results:
			active--
			groups[result.index] = result.choices
			if result.err != nil {
				lastErr = result.err
			}
			found += len(result.choices)
			for _, choice := range result.choices {
				heights[choice.height] = true
			}
			if found > 0 {
				stop = timer.C
				if time.Since(started) >= movyQuickScrapeTimeout || heights[2160] && heights[1080] {
					next, active = len(servers), 0
				}
			}
		}
	}
	stopScrape()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if found == 0 && lastErr != nil {
		return nil, fmt.Errorf("Movy sources: %w", lastErr)
	}
	if found > 0 {
		select {
		case subtitles = <-subtitleReady:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var choices []movyChoice
	seen := map[string]bool{}
	for _, group := range groups {
		for _, choice := range group {
			key := choice.stream.URL + "|" + choice.stream.variantURL
			if seen[key] {
				continue
			}
			seen[key] = true
			choices = append(choices, choice)
		}
	}
	sort.SliceStable(choices, func(i, j int) bool { return choices[i].height > choices[j].height })
	streams := []Stream{}
	for _, choice := range choices {
		appendMovySubtitles(&choice.stream, subtitles)
		streams = append(streams, choice.stream)
	}
	return streams, nil
}
