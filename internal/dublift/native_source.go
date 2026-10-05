package dublift

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
	"github.com/bluenviron/gohlslib/v2/pkg/playlist/primitives"
)

var hlsDefaultRE = regexp.MustCompile(`([:,])DEFAULT=(?:YES|NO)\b`)
var hlsAutoselectRE = regexp.MustCompile(`([:,])AUTOSELECT=NO\b`)

// Native playback retains the source's media clock, audio and subtitles. Only
// manifests and bounded resources are proxied; no alignment or decoding runs.
type nativePlayback struct {
	ProviderURL string `json:"providerURL,omitempty"`
	Quality     string `json:"quality,omitempty"`
	VariantURL  string `json:"variantURL,omitempty"`
	Italian     bool   `json:"italian"`
}

type sourceQuality struct {
	stream Stream
	native *nativePlayback
}

func variantSize(v *playlist.MultivariantVariant) (width, height int) {
	fmt.Sscanf(v.Resolution, "%dx%d", &width, &height)
	return
}

func formatBitrate(bits int) string {
	if bits <= 0 {
		return ""
	}
	if bits < 1000000 {
		return fmt.Sprintf("%d kbps", (bits+500)/1000)
	}
	precision := 2
	if bits >= 10000000 {
		precision = 1
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(float64(bits)/1000000, 'f', precision, 64), "0"), ".") + " Mbps"
}

func masterHasItalian(h *HLS, variant *playlist.MultivariantVariant) bool {
	if h.Master == nil {
		return false
	}
	for _, r := range h.Master.Renditions {
		if r.Type == playlist.MultivariantRenditionTypeAudio && (variant == nil || r.GroupID == variant.Audio) && (language(r.Language) == "it" || language(r.Name) == "it") {
			return true
		}
	}
	return false
}

func vixQualityStreams(h *HLS) []sourceQuality {
	if h == nil {
		return nil
	}
	create := func(quality, detail, variantURL string, italian bool) sourceQuality {
		stream := Stream{Name: "VixSrc · " + quality, Title: detail, URL: h.Origin.URL, Headers: map[string]string{}}
		for key := range h.Origin.Headers {
			stream.Headers[key] = h.Origin.Headers.Get(key)
		}
		stream.BehaviorHints.Filename = "vixsrc-" + quality + ".m3u8"
		return sourceQuality{stream, &nativePlayback{ProviderURL: h.Origin.Headers.Get("Origin"), Quality: quality, VariantURL: variantURL, Italian: italian}}
	}
	if h.Master == nil || len(h.Master.Variants) == 0 {
		return []sourceQuality{create("Auto quality", "VixSrc · Original audio and subtitles", "", masterHasItalian(h, nil))}
	}
	variants := append([]*playlist.MultivariantVariant{}, h.Master.Variants...)
	sort.SliceStable(variants, func(i, j int) bool {
		wi, hi := variantSize(variants[i])
		wj, hj := variantSize(variants[j])
		if hi != hj {
			return hi > hj
		}
		if wi != wj {
			return wi > wj
		}
		return variants[i].Bandwidth > variants[j].Bandwidth
	})
	seen := map[string]bool{}
	qualities := []sourceQuality{}
	for _, variant := range variants {
		width, height := variantSize(variant)
		quality := "Auto quality"
		if height > 0 {
			quality = fmt.Sprintf("%dp", height)
		} else if variant.Bandwidth > 0 {
			quality = formatBitrate(variant.Bandwidth)
		}
		if seen[quality] {
			continue
		}
		origin, err := h.Child(variant.URI)
		if err != nil {
			continue
		}
		seen[quality] = true
		bitrate := variant.Bandwidth
		if variant.AverageBandwidth != nil && *variant.AverageBandwidth > 0 {
			bitrate = *variant.AverageBandwidth
		}
		details := []string{}
		if width > 0 && height > 0 {
			details = append(details, fmt.Sprintf("📺 %d×%d", width, height))
		}
		if label := formatBitrate(bitrate); label != "" {
			details = append(details, "📶 "+label)
		}
		lines := []string{}
		if len(details) > 0 {
			lines = append(lines, strings.Join(details, " · "))
		}
		if len(variant.Codecs) > 0 {
			labels := make([]string, len(variant.Codecs))
			for i, codec := range variant.Codecs {
				name := strings.ToLower(codec)
				switch {
				case strings.HasPrefix(name, "avc1."), strings.HasPrefix(name, "avc3."):
					labels[i] = "H.264"
				case name == "mp4a.40.2", name == "mp4a.40.5", name == "mp4a.40.29":
					labels[i] = "AAC"
				default:
					labels[i] = codec
				}
			}
			lines = append(lines, "🎞️ "+strings.Join(labels, " / "))
		}
		result := create(quality, strings.Join(lines, "\n"), origin.URL, masterHasItalian(h, variant))
		if label := formatBitrate(bitrate); label != "" {
			result.stream.Name += " · " + label
		}
		qualities = append(qualities, result)
	}
	return qualities
}

func nativeVariant(h *HLS, selected string) (*playlist.MultivariantVariant, error) {
	if h.Master == nil {
		return nil, nil
	}
	if selected == "" {
		return h.BestVariant()
	}
	for _, variant := range h.Master.Variants {
		origin, err := h.Child(variant.URI)
		if err == nil && origin.URL == selected {
			return variant, nil
		}
	}
	return nil, errors.New("selected source quality is no longer available; request fresh streams")
}

func (s *Server) prepareNative(ctx context.Context, v *Session) error {
	h := v.nativeMaster
	var err error
	if h == nil {
		// A restored ticket has no cookie jar or cached master. Resolve through
		// the original provider again; cookies stay in the HTTP client's jar.
		provider, quality := v.native.ProviderURL, v.native.Quality
		if provider == "" {
			provider = v.stream.Origin().Headers.Get("Origin")
		}
		if quality == "" {
			// Tickets issued before quality metadata was added retain this label.
			quality = strings.TrimSuffix(strings.TrimPrefix(v.stream.BehaviorHints.Filename, "vixsrc-"), ".m3u8")
		}
		cfg := s.Config.Get()
		id, resolveErr := s.Net.ResolveTMDB(ctx, v.Content, cfg.TMDBToken)
		if resolveErr != nil {
			return resolveErr
		}
		origin, resolveErr := s.Net.ResolveVix(ctx, provider, v.Content.Type, id, v.Content.Season, v.Content.Episode)
		if resolveErr != nil {
			return resolveErr
		}
		h, err = s.Net.LoadHLS(ctx, origin)
		if err != nil {
			return err
		}
		matched := false
		for _, candidate := range vixQualityStreams(h) {
			if candidate.native.Quality == quality {
				v.native.VariantURL = candidate.native.VariantURL
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("selected source quality is no longer available; request fresh streams")
		}
	}
	v.nativeMaster = h
	variant, err := nativeVariant(h, v.native.VariantURL)
	if err != nil {
		return err
	}
	video := h
	for depth := 0; video.Master != nil; depth++ {
		if depth >= 3 {
			return errors.New("too many nested HLS masters")
		}
		selected := variant
		if depth > 0 {
			selected, err = video.BestVariant()
			if err != nil {
				return err
			}
		}
		origin, err := video.Child(selected.URI)
		if err != nil {
			return err
		}
		video, err = s.Net.LoadHLS(ctx, origin)
		if err != nil {
			return err
		}
	}
	v.video = &Asset{ID: identity(video.Origin.URL), Origin: video.Origin, HLS: video}
	v.Duration = video.Duration
	v.boundaries = []float64{0}
	for _, seg := range video.Segments {
		v.boundaries = append(v.boundaries, seg.Start+seg.Duration)
	}
	if h.Master != nil {
		for i, r := range h.Master.Renditions {
			if variant != nil && !renditionMatches(r.Type, r.GroupID, variant) {
				continue
			}
			v.tracks = append(v.tracks, Track{ID: fmt.Sprintf("native-%d", i), Name: r.Name, Lang: language(r.Language), Original: true, Subtitle: r.Type == playlist.MultivariantRenditionTypeSubtitles})
		}
	}
	v.state("Ready · original source audio and video")
	return ctx.Err()
}

func renditionMatches(kind playlist.MultivariantRenditionType, group string, v *playlist.MultivariantVariant) bool {
	switch kind {
	case playlist.MultivariantRenditionTypeAudio:
		return group == v.Audio
	case playlist.MultivariantRenditionTypeSubtitles:
		return group == v.Subtitles
	case playlist.MultivariantRenditionTypeVideo:
		return group == v.Video
	case playlist.MultivariantRenditionTypeClosedCaptions:
		return group == v.ClosedCaptions
	}
	return false
}

func (v *Session) addNativeResource(origin Origin, position float64, track, video bool, depth int) string {
	key := identity(origin.URL, fmt.Sprint(origin.Headers), strconv.FormatBool(track), strconv.FormatBool(video), decimal(position), "native", strconv.Itoa(depth))[:32]
	v.mu.Lock()
	// Cancellation must not repopulate a discarded session's resource state.
	if v.ctx.Err() == nil {
		v.resources[key] = resource{Origin: origin, Position: position, Track: track, Video: video, PlaylistDepth: depth}
	}
	v.mu.Unlock()
	return "/media/" + v.ID + "/resource/" + key
}

// Preserve raw HLS tags, including unknown attributes, encryption and ranges.
// A selected quality keeps only its variant and associated rendition groups.
func (s *Server) nativePlaylist(v *Session, h *HLS, selected string, video bool, depth int) ([]byte, error) {
	var variant *playlist.MultivariantVariant
	var err error
	if selected != "" {
		variant, err = nativeVariant(h, selected)
		if err != nil {
			return nil, err
		}
	}
	italianDefaults := map[string]string{}
	if h.Master != nil {
		for _, r := range h.Master.Renditions {
			if r.Type == playlist.MultivariantRenditionTypeAudio && (language(r.Language) == "it" || language(r.Name) == "it") {
				if _, exists := italianDefaults[r.GroupID]; !exists || r.Default {
					italianDefaults[r.GroupID] = r.Name
				}
			}
		}
	}
	var b strings.Builder
	pending := ""
	index := 0
	for _, raw := range strings.Split(h.Raw, "\n") {
		line := strings.TrimSpace(raw)
		if h.Master == nil && strings.HasPrefix(line, "#EXT-X-BYTERANGE:") && index < len(h.Segments) {
			// Segment URLs include playback positions, so consecutive ranges no
			// longer share a URI. Emit the parser's resolved size@offset instead.
			b.WriteString("#EXT-X-BYTERANGE:" + h.Segments[index].Range + "\n")
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			pending = raw
			continue
		}
		if line != "" && !strings.HasPrefix(line, "#") {
			origin, err := h.Child(line)
			if err != nil {
				return nil, err
			}
			position, childDepth, track := 0.0, 0, h.Master == nil
			if h.Master != nil {
				if selected != "" && origin.URL != selected {
					pending = ""
					continue
				}
				if pending != "" {
					b.WriteString(pending + "\n")
					pending = ""
				}
				childDepth = depth + 1
			} else if index < len(h.Segments) {
				position = h.Segments[index].Start
				index++
			}
			b.WriteString(v.addNativeResource(origin, position, track, video, childDepth) + "\n")
			continue
		}
		if variant != nil && strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:") {
			continue
		}
		isRendition := strings.HasPrefix(line, "#EXT-X-MEDIA:")
		childVideo := video
		if isRendition {
			var attrs primitives.Attributes
			if err := attrs.Unmarshal(strings.TrimPrefix(line, "#EXT-X-MEDIA:")); err != nil {
				return nil, err
			}
			if variant != nil && !renditionMatches(playlist.MultivariantRenditionType(attrs["TYPE"]), attrs["GROUP-ID"], variant) {
				continue
			}
			childVideo = attrs["TYPE"] == "VIDEO"
			if preferred, ok := italianDefaults[attrs["GROUP-ID"]]; attrs["TYPE"] == "AUDIO" && ok {
				italian := attrs["NAME"] == preferred
				// DEFAULT is explicit for every audio track; other attributes stay intact.
				defaultValue := "NO"
				if italian {
					defaultValue = "YES"
				}
				if _, ok := attrs["DEFAULT"]; ok {
					raw = hlsDefaultRE.ReplaceAllString(raw, "${1}DEFAULT="+defaultValue)
				} else {
					raw += ",DEFAULT=" + defaultValue
				}
				if italian {
					raw = hlsAutoselectRE.ReplaceAllString(raw, "${1}AUTOSELECT=YES")
				}
			}
		}
		playlistURI := isRendition || strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:") || strings.HasPrefix(line, "#EXT-X-RENDITION-REPORT:")
		b.WriteString(rewriteURI(raw, h.Origin.URL, func(url string) string {
			childDepth := 0
			if playlistURI {
				childDepth = depth + 1
			}
			return v.addNativeResource(Origin{URL: url, Headers: h.Origin.Headers}, 0, false, childVideo, childDepth)
		}) + "\n")
	}
	return []byte(b.String()), nil
}
