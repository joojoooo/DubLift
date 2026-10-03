package dublift

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var cueRE = regexp.MustCompile(`(?m)^(\d{2,}:)?\d{2}:\d{2}\.\d{3}\s+-->\s+(?:\d{2,}:)?\d{2}:\d{2}\.\d{3}[^\r\n]*`)
var mapRE = regexp.MustCompile(`X-TIMESTAMP-MAP=LOCAL:([^,\r\n]+),MPEGTS:(\d+)`)

func parseVTTTime(s string) (float64, error) {
	p := strings.Split(s, ":")
	if len(p) < 2 || len(p) > 3 {
		return 0, errors.New("invalid WebVTT timestamp")
	}
	v := 0.0
	for _, x := range p {
		n, e := strconv.ParseFloat(x, 64)
		if e != nil {
			return 0, e
		}
		v = v*60 + n
	}
	return v, nil
}
func vttTime(t float64) string {
	ms := int64(math.Round(max(0, t) * 1000))
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, (ms/60000)%60, (ms/1000)%60, ms%1000)
}

type cue struct {
	start, end     float64
	text, settings string
}

func parseVTT(b []byte, segmentStart float64) ([]cue, error) {
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(strings.TrimPrefix(text, "\ufeff"), "WEBVTT") {
		return nil, errors.New("only WebVTT subtitle segments are supported")
	}
	shift := 0.0
	// X-TIMESTAMP-MAP commonly maps local zero to the segment's media clock.
	// Cue clocks near zero are segment-local, otherwise they are global VOD time.
	local := 0.0
	if m := mapRE.FindStringSubmatch(text); m != nil {
		local, _ = parseVTTTime(m[1])
		ticks, _ := strconv.ParseFloat(m[2], 64)
		shift = ticks/90000 - local
		shift -= math.Floor((shift-segmentStart+47721.8588)/95443.7177) * 95443.7177
	}
	var out []cue
	for _, block := range strings.Split(text, "\n\n") {
		loc := cueRE.FindStringIndex(block)
		if loc == nil {
			continue
		}
		line := block[loc[0]:loc[1]]
		parts := strings.Split(line, "-->")
		start, e := parseVTTTime(strings.TrimSpace(parts[0]))
		if e != nil {
			return nil, e
		}
		right := strings.Fields(parts[1])
		end, e := parseVTTTime(right[0])
		if e != nil {
			return nil, e
		}
		settings := ""
		if len(right) > 1 {
			settings = " " + strings.Join(right[1:], " ")
		}
		payload := strings.TrimLeft(block[loc[1]:], "\n")
		out = append(out, cue{start + shift, end + shift, payload, settings})
	}
	return out, nil
}
func (s *Server) subtitles(ctx context.Context, v *Session, t Track, start, duration, offset, clockBase float64) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "WEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:%d\n\n", int64(math.Round(clockBase*90000))&((1<<33)-1))
	seen := map[string]bool{}
	if t.Asset.HLS == nil {
		return nil, errors.New("embedded file subtitles are not yet extracted")
	}
	for _, seg := range t.Asset.HLS.Segments {
		if seg.Start+seg.Duration < start-offset || seg.Start > start+duration-offset {
			continue
		}
		if seg.Key != "" && !strings.Contains(seg.Key, "METHOD=NONE") {
			return nil, errors.New("encrypted WebVTT is unsupported")
		}
		o := Origin{seg.URI, t.Asset.Origin.Headers}
		var data []byte
		var err error
		if seg.Range != "" {
			parts := strings.Split(seg.Range, "@")
			size, _ := strconv.ParseInt(parts[0], 10, 64)
			at, _ := strconv.ParseInt(parts[1], 10, 64)
			br := fmt.Sprintf("bytes=%d-%d", at, at+size-1)
			resp, e := s.Net.request(ctx, o, "GET", br)
			if e != nil {
				return nil, e
			}
			err = validateMediaResponse(resp, br)
			if err == nil {
				data, err = readBounded(resp.Body, manifestLimit)
			}
			resp.Body.Close()
		} else {
			fetch := func() ([]byte, error) { b, _, e := s.Net.Fetch(ctx, o, manifestLimit); return b, e }
			if s.Engine != nil {
				data, err = s.Engine.Cache.Get(ctx, "subtitle:"+t.Asset.ID+":"+identity(seg.URI), fetch)
			} else {
				data, err = fetch()
			}
		}
		if err != nil {
			return nil, err
		}
		cues, err := parseVTT(data, seg.Start)
		if err != nil {
			return nil, err
		}
		nativeBase := 0.0
		if mapRE.Match(data) && t.Original {
			// Original cues share the video's presentation clock, including
			// resets at source discontinuities. Unmapped cues are VOD-relative.
			nativeBase = clockBase
		} else if mapRE.Match(data) && v.vixEnglish != nil && s.Engine != nil {
			b, e := s.Engine.Cache.Get(ctx, "subtitle-clock:"+v.vixEnglish.Asset.ID, func() ([]byte, error) {
				p, e := s.Engine.Probe(ctx, v.vixEnglish.Asset)
				if e != nil {
					return nil, e
				}
				base, e := strconv.ParseFloat(p.Format.StartTime, 64)
				if e != nil {
					return nil, e
				}
				return []byte(decimal(base)), nil
			})
			if e != nil {
				return nil, e
			}
			nativeBase, _ = strconv.ParseFloat(string(b), 64)
		}
		for _, c := range cues {
			a, z := c.start-nativeBase+offset, c.end-nativeBase+offset
			if z <= start || a >= start+duration {
				continue
			}
			key := fmt.Sprintf("%.3f:%.3f:%s", a, z, c.text)
			if seen[key] {
				continue
			}
			seen[key] = true
			fmt.Fprintf(&b, "%s --> %s%s\n%s\n\n", vttTime(max(a, start)), vttTime(min(z, start+duration)), c.settings, c.text)
		}
	}
	return []byte(b.String()), nil
}
