package dublift

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"math/bits"
	"os"
	"sort"
	"sync"
	"time"

	chroma "github.com/alexgorbatchev/gochromaprint"
)

const pcmRate = 11025
const alignmentWindow = 16.0

type Anchor struct {
	VixTime    float64 `json:"vixTime"`
	SourceTime float64 `json:"sourceTime"`
	Offset     float64 `json:"offset"`
	Confidence float64 `json:"confidence"`
}
type Boundary struct {
	SourceTime float64   `json:"sourceTime"`
	Offset     float64   `json:"offset"`
	Confidence float64   `json:"confidence"`
	Created    time.Time `json:"created"`
}
type Alignment struct {
	AutoSamples   []Anchor   `json:"automaticSamples"`
	AutoExpected  int        `json:"automaticExpected"`
	AutoComplete  bool       `json:"automaticComplete"`
	Anchors       []Anchor   `json:"anchors"`
	Boundaries    []Boundary `json:"boundaries"`
	Offset        float64    `json:"offset"`
	Confidence    float64    `json:"confidence"`
	DifferentEdit bool       `json:"differentEdit"`
	Manual        *float64   `json:"manual,omitempty"`
	Updated       time.Time  `json:"updated"`
}

func (a Alignment) At(t float64, minConfidence float64) float64 {
	if a.Manual != nil {
		return *a.Manual
	}
	off := 0.0
	if a.Confidence >= minConfidence {
		off = a.Offset
	}
	for _, b := range a.Boundaries {
		if b.SourceTime > t {
			break
		}
		if b.Confidence >= minConfidence {
			off = b.Offset
		}
	}
	return off
}
func (a *Alignment) Choose(threshold float64) {
	good := []Anchor{}
	for _, v := range a.Anchors {
		if v.Confidence >= threshold {
			good = append(good, v)
		}
	}
	a.Offset = 0
	a.Confidence = 0
	a.DifferentEdit = false
	if len(good) == 0 {
		return
	}
	sort.Slice(good, func(i, j int) bool { return good[i].SourceTime < good[j].SourceTime })
	a.Offset = good[0].Offset
	a.Confidence = good[0].Confidence
	lo, hi := a.Offset, a.Offset
	offsets := []float64{}
	for _, v := range good {
		lo = min(lo, v.Offset)
		hi = max(hi, v.Offset)
		offsets = append(offsets, v.Offset)
	}
	a.DifferentEdit = hi-lo > .25
	if !a.DifferentEdit {
		sort.Float64s(offsets)
		a.Offset = offsets[len(offsets)/2]
	}
}

type AlignmentStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]Alignment
}

func OpenAlignments(path string) (*AlignmentStore, error) {
	s := &AlignmentStore{path: path, entries: map[string]Alignment{}}
	b, e := os.ReadFile(path)
	if errors.Is(e, os.ErrNotExist) {
		return s, nil
	}
	if e != nil {
		return nil, e
	}
	if e = json.Unmarshal(b, &s.entries); e != nil {
		return nil, e
	}
	return s, nil
}
func (s *AlignmentStore) Get(key string) Alignment {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.entries[key]
	a.Anchors = append([]Anchor{}, a.Anchors...)
	a.AutoSamples = append([]Anchor{}, a.AutoSamples...)
	a.Boundaries = append([]Boundary{}, a.Boundaries...)
	if a.Manual != nil {
		x := *a.Manual
		a.Manual = &x
	}
	return a
}
func (s *AlignmentStore) Update(key string, fn func(*Alignment)) error {
	return s.UpdateContext(context.Background(), key, fn)
}
func (s *AlignmentStore) UpdateContext(ctx context.Context, key string, fn func(*Alignment)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Check while holding the same lock as Prune: canceled work cannot recreate
	// a session's alignment after the idle collector has removed it.
	if err := ctx.Err(); err != nil {
		return err
	}
	a := s.entries[key]
	fn(&a)
	a.Updated = time.Now()
	s.entries[key] = a
	if len(s.entries) > 2000 {
		old := ""
		var t time.Time
		for k, v := range s.entries {
			if k != key && (old == "" || v.Updated.Before(t)) {
				old = k
				t = v.Updated
			}
		}
		delete(s.entries, old)
	}
	return atomicJSON(s.path, s.entries)
}

func (s *AlignmentStore) Prune(now time.Time, active map[string]bool, remove map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for key, a := range s.entries {
		if !active[key] && (remove[key] || now.Sub(a.Updated) >= time.Hour) {
			delete(s.entries, key)
			changed = true
		}
	}
	if changed {
		return atomicJSON(s.path, s.entries)
	}
	return nil
}
func identity(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fingerprint(pcm []int16) ([]uint32, float64, error) {
	c := chroma.New(chroma.AlgorithmDefault)
	if e := c.Start(pcmRate, 1); e != nil {
		return nil, 0, e
	}
	if e := c.Feed(pcm); e != nil {
		return nil, 0, e
	}
	if e := c.Finish(); e != nil {
		return nil, 0, e
	}
	fp, e := c.RawFingerprint()
	return fp, float64(c.ItemDuration()) / float64(c.SampleRate()), e
}

type candidate struct {
	lag   int
	score float64
}

func fingerprints(needle, hay []uint32) []candidate {
	if len(needle) < 30 || len(hay) < len(needle) {
		return nil
	}
	varied := 0
	for _, v := range needle {
		varied += bits.OnesCount32(v ^ needle[0])
	}
	if varied < len(needle)*2 {
		return nil
	}
	var out []candidate
	// Ignore filter warm-up at either end of the short fingerprint.
	trim := min(8, len(needle)/5)
	for lag := 0; lag <= len(hay)-len(needle); lag++ {
		diff := 0
		for j := trim; j < len(needle)-trim; j++ {
			diff += bits.OnesCount32(needle[j] ^ hay[lag+j])
		}
		score := 1 - float64(diff)/float64((len(needle)-trim*2)*16)
		out = append(out, candidate{lag, math.Max(0, score)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })
	var best []candidate
	for _, v := range out {
		separate := true
		for _, b := range best {
			if absInt(v.lag-b.lag) < 12 {
				separate = false
			}
		}
		if separate {
			best = append(best, v)
			if len(best) == 5 {
				break
			}
		}
	}
	return best
}
func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Three band energy envelopes tolerate different mixes/codecs. Correlation
// verifies Chromaprint candidates; a fingerprint alone never establishes sync.
func spectral(pcm []int16) [][3]float64 {
	const hop = 110
	out := make([][3]float64, 0, len(pcm)/hop)
	var low, mid float64
	var energy [3]float64
	a := 1 - math.Exp(-2*math.Pi*250/pcmRate)
	b := 1 - math.Exp(-2*math.Pi*1500/pcmRate)
	for i, v := range pcm {
		x := float64(v) / 32768
		low += a * (x - low)
		mid += b * (x - mid)
		bands := [3]float64{low, mid - low, x - mid}
		for k, z := range bands {
			energy[k] += z * z
		}
		if (i+1)%hop == 0 {
			var f [3]float64
			for k, v := range energy {
				f[k] = math.Log(1e-8 + v/hop)
			}
			out = append(out, f)
			energy = [3]float64{}
		}
	}
	return out
}
func spectralScore(a, b [][3]float64, lag int) float64 {
	if lag < 0 || lag+len(a) > len(b) || len(a) < 100 {
		return 0
	}
	total := 0.0
	for k := 0; k < 3; k++ {
		var sx, sy, sxx, syy, sxy float64
		for i := 15; i < len(a)-15; i++ {
			x := a[i][k]
			y := b[lag+i][k]
			sx += x
			sy += y
			sxx += x * x
			syy += y * y
			sxy += x * y
		}
		n := float64(len(a) - 30)
		den := math.Sqrt(max(0, sxx-sx*sx/n) * max(0, syy-sy*sy/n))
		if den > 1e-6 {
			total += (sxy - sx*sy/n) / den
		}
	}
	return max(0, total/3)
}
func pcmScore(a, b []int16, lag int) float64 {
	if lag < 0 || lag+len(a) > len(b) {
		return 0
	}
	var xy, xx, yy float64
	// Restrict refinement to the central eight seconds, at a decimated rate.
	first := max(0, len(a)/2-4*pcmRate)
	last := min(len(a), first+8*pcmRate)
	for i := first; i < last; i += 8 {
		x := float64(a[i])
		y := float64(b[i+lag])
		xy += x * y
		xx += x * x
		yy += y * y
	}
	if xx*yy < 1 {
		return 0
	}
	return math.Abs(xy / math.Sqrt(xx*yy))
}
func MatchPCM(ctx context.Context, needle, hay []int16) (lag, confidence float64, err error) {
	nf, step, e := fingerprint(needle)
	if e != nil {
		return 0, 0, e
	}
	hf, _, e := fingerprint(hay)
	if e != nil {
		return 0, 0, e
	}
	cs := fingerprints(nf, hf)
	if len(cs) == 0 {
		return 0, 0, errors.New("no distinctive English fingerprint")
	}
	ne, he := spectral(needle), spectral(hay)
	best, bestLag, runner := 0.0, 0.0, 0.0
	for _, c := range cs {
		if e := ctx.Err(); e != nil {
			return 0, 0, e
		}
		if c.score < .35 {
			continue
		}
		center := int(float64(c.lag) * step * pcmRate / 110)
		score, at := 0.0, 0
		for j := center - 45; j <= center+45; j++ {
			s := spectralScore(ne, he, j)
			if s > score {
				score = s
				at = j
			}
		}
		if score < .45 {
			continue
		}
		combined := .35*c.score + .65*score
		if combined > best {
			runner = best
			best = combined
			bestLag = float64(at) * 110 / pcmRate
		} else {
			runner = max(runner, combined)
		}
	}
	if best == 0 {
		return 0, 0, errors.New("fingerprint candidates failed spectral verification")
	}
	if runner > best-.04 {
		best *= .65
	}
	// PCM refines only when the waveform actually agrees; different stereo
	// mixes may correlate spectrally while not correlating sample-for-sample.
	sampleLag := int(math.Round(bestLag * pcmRate))
	wave, bestSample := 0.0, sampleLag
	for d := -220; d <= 220; d++ {
		s := pcmScore(needle, hay, sampleLag+d)
		if s > wave {
			wave = s
			bestSample = sampleLag + d
		}
	}
	if wave > .5 {
		bestLag = float64(bestSample) / pcmRate
		best = min(1, best+.03)
	}
	return bestLag, best, nil
}

type PCMExtractor func(context.Context, float64, float64) ([]int16, error)

func FindAnchor(ctx context.Context, source, vix PCMExtractor, vixAt, expectedOffset, radius, duration float64) (Anchor, error) {
	const window = alignmentWindow
	vixAt = max(0, min(vixAt, duration-window))
	sourceAt := max(0, vixAt+expectedOffset-radius)
	sourceDuration := min(window+2*radius, duration-sourceAt)
	if sourceDuration < window {
		return Anchor{}, errors.New("not enough English audio at this position")
	}
	needle, e := vix(ctx, vixAt, window)
	if e != nil {
		return Anchor{}, e
	}
	hay, e := source(ctx, sourceAt, sourceDuration)
	if e != nil {
		return Anchor{}, e
	}
	lag, confidence, e := MatchPCM(ctx, needle, hay)
	if e != nil {
		return Anchor{}, e
	}
	return Anchor{VixTime: vixAt + window/2, SourceTime: sourceAt + lag + window/2, Offset: sourceAt + lag - vixAt, Confidence: confidence}, nil
}
