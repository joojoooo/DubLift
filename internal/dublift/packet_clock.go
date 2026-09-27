package dublift

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Read FFmpeg's framecrc timebase and first packet PTS, then discard further
// records. This uses constant memory even for large remuxed output segments.
type packetClock struct {
	line           string
	scale, first   float64
	known, invalid bool
}

func (p *packetClock) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 && !p.known && !p.invalid {
		i := 0
		for i < len(data) && data[i] != '\n' {
			i++
		}
		if len(p.line)+i > 4096 {
			p.invalid = true
			break
		}
		p.line += string(data[:i])
		data = data[i:]
		if len(data) == 0 {
			break
		}
		data = data[1:]
		p.parse(p.line)
		p.line = ""
	}
	return n, nil
}
func (p *packetClock) parse(line string) {
	if strings.HasPrefix(line, "#tb 0:") {
		var numerator, denominator int64
		if _, err := fmt.Sscanf(line, "#tb 0: %d/%d", &numerator, &denominator); err == nil && numerator > 0 && denominator > 0 {
			p.scale = float64(numerator) / float64(denominator)
		}
		return
	}
	if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
		return
	}
	fields := strings.Split(line, ",")
	if len(fields) < 6 || strings.TrimSpace(fields[0]) != "0" || p.scale == 0 {
		p.invalid = true
		return
	}
	pts, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
	if err != nil || math.Abs(float64(pts)*p.scale) > 24*3600 {
		p.invalid = true
		return
	}
	p.first, p.known = float64(pts)*p.scale, true
}
