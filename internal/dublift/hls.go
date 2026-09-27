package dublift

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
	"github.com/bluenviron/gohlslib/v2/pkg/playlist/primitives"
)

var uriRE = regexp.MustCompile(`URI="([^"]+)"`)

type HLSSegment struct {
	URI             string
	Start, Duration float64
	Key, Map        string
	Range           string
	Sequence        int
	Discontinuity   bool
}
type HLS struct {
	Origin   Origin
	Raw      string
	Master   *playlist.Multivariant
	Segments []HLSSegment
	Duration float64
	Target   int
}

func (n *Network) LoadHLS(ctx context.Context, o Origin) (*HLS, error) {
	b, base, e := n.Fetch(ctx, o, manifestLimit)
	if e != nil {
		return nil, e
	}
	o.URL = base
	return ParseHLS(b, o)
}
func ParseHLS(b []byte, o Origin) (*HLS, error) {
	h := &HLS{Origin: o, Raw: string(b)}
	p, e := playlist.Unmarshal(b)
	if e != nil {
		return nil, fmt.Errorf("invalid HLS: %w", e)
	}
	if m, ok := p.(*playlist.Multivariant); ok {
		h.Master = m
		return h, nil
	}
	m := p.(*playlist.Media)
	if !m.Endlist {
		return nil, errors.New("only finite VOD HLS is supported")
	}
	if len(m.Segments) > 50000 {
		return nil, errors.New("HLS has too many segments")
	}
	h.Target = m.TargetDuration
	var key, mapLine, br, lastRangeURI string
	var dur float64
	var end uint64
	discontinuity := false
	seq := m.MediaSequence
	for _, raw := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(l, "#EXT-X-KEY:"):
			var a primitives.Attributes
			if e = a.Unmarshal(strings.TrimPrefix(l, "#EXT-X-KEY:")); e != nil {
				return nil, e
			}
			if a["METHOD"] != "NONE" && a["METHOD"] != "AES-128" {
				return nil, errors.New("DRM / SAMPLE-AES media is unsupported")
			}
			key = l
		case strings.HasPrefix(l, "#EXT-X-MAP:"):
			mapLine = l
		case strings.HasPrefix(l, "#EXTINF:"):
			dur, e = strconv.ParseFloat(strings.Split(strings.TrimPrefix(l, "#EXTINF:"), ",")[0], 64)
			if e != nil || dur <= 0 || math.IsNaN(dur) || dur > 86400 {
				return nil, errors.New("invalid or oversized HLS segment duration")
			}
		case strings.HasPrefix(l, "#EXT-X-BYTERANGE:"):
			br = strings.TrimPrefix(l, "#EXT-X-BYTERANGE:")
		case l == "#EXT-X-DISCONTINUITY":
			discontinuity = true
		case l != "" && !strings.HasPrefix(l, "#"):
			u, e := resolveURL(o.URL, l)
			if e != nil {
				return nil, e
			}
			if br != "" {
				p := strings.Split(br, "@")
				size, e := strconv.ParseUint(p[0], 10, 64)
				if e != nil || size == 0 {
					return nil, errors.New("invalid HLS byte range")
				}
				start := end
				if len(p) == 2 {
					start, e = strconv.ParseUint(p[1], 10, 64)
					if e != nil {
						return nil, e
					}
				} else if lastRangeURI != u {
					return nil, errors.New("implicit byte range has no preceding range")
				}
				br = fmt.Sprintf("%d@%d", size, start)
				end = start + size
				lastRangeURI = u
			}
			h.Segments = append(h.Segments, HLSSegment{URI: u, Start: h.Duration, Duration: dur, Key: key, Map: mapLine, Range: br, Sequence: seq, Discontinuity: discontinuity})
			h.Duration += dur
			seq++
			br = ""
			dur = 0
			discontinuity = false
		}
	}
	if len(h.Segments) == 0 || h.Duration <= 0 {
		return nil, errors.New("empty HLS playlist")
	}
	return h, nil
}
func (h *HLS) Child(ref string) (Origin, error) {
	u, e := resolveURL(h.Origin.URL, ref)
	return Origin{u, h.Origin.Headers}, e
}
func (h *HLS) BestVariant() (*playlist.MultivariantVariant, error) {
	if h.Master == nil {
		return nil, errors.New("not a master playlist")
	}
	var best *playlist.MultivariantVariant
	for _, v := range h.Master.Variants {
		if best == nil || v.Bandwidth > best.Bandwidth {
			best = v
		}
	}
	if best == nil {
		return nil, errors.New("no HLS variants")
	}
	return best, nil
}
func language(s string) string {
	switch strings.ToLower(s) {
	case "eng", "english", "en":
		return "en"
	case "ita", "italian", "italiano", "it":
		return "it"
	}
	return strings.ToLower(s)
}

// Window builds a finite playlist containing only the segments needed for a
// short extraction. It preserves sequence numbers (AES IVs), keys, maps and
// explicit byte offsets. FFmpeg never receives a complete movie playlist.
func (h *HLS) Window(start, duration float64, rewrite func(string) string) (string, float64, error) {
	if h.Master != nil {
		return "", 0, errors.New("window needs a media playlist")
	}
	if duration <= 0 || duration > 400 {
		return "", 0, errors.New("audio window exceeds duration budget")
	}
	first := -1
	last := -1
	for i, s := range h.Segments {
		if s.Start+s.Duration > start && s.Start < start+duration {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return "", 0, errors.New("window is outside media duration")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-PLAYLIST-TYPE:VOD\n", h.Target, h.Segments[first].Sequence)
	oldKey, oldMap := "", ""
	for _, s := range h.Segments[first : last+1] {
		if s.Discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		for i, l := range []string{s.Key, s.Map} {
			old := oldKey
			if i == 1 {
				old = oldMap
			}
			if l != "" && l != old {
				b.WriteString(rewriteURI(l, h.Origin.URL, rewrite) + "\n")
			}
			if i == 0 {
				oldKey = l
			} else {
				oldMap = l
			}
		}
		if s.Range != "" {
			b.WriteString("#EXT-X-BYTERANGE:" + s.Range + "\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n%s\n", s.Duration, rewrite(s.URI))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String(), h.Segments[first].Start, nil
}
func rewriteURI(l, base string, rewrite func(string) string) string {
	return uriRE.ReplaceAllStringFunc(l, func(m string) string {
		v := uriRE.FindStringSubmatch(m)[1]
		u, e := resolveURL(base, v)
		if e != nil {
			return `URI="invalid"`
		}
		return `URI="` + rewrite(u) + `"`
	})
}
func isHLSURL(raw, filename string) bool {
	u, _ := url.Parse(raw)
	return u != nil && strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") || strings.HasSuffix(strings.ToLower(filename), ".m3u8")
}
