package dublift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

type Stream struct {
	raw           json.RawMessage
	Name          string            `json:"name,omitempty"`
	Title         string            `json:"title,omitempty"`
	Description   string            `json:"description,omitempty"`
	URL           string            `json:"url,omitempty"`
	ExternalURL   string            `json:"externalUrl,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	BehaviorHints struct {
		Filename     string `json:"filename,omitempty"`
		NotWebReady  bool   `json:"notWebReady,omitempty"`
		BingeGroup   string `json:"bingeGroup,omitempty"`
		ProxyHeaders struct {
			Request map[string]string `json:"request,omitempty"`
		} `json:"proxyHeaders,omitempty"`
	} `json:"behaviorHints"`
	Subtitles []struct {
		ID   string `json:"id"`
		URL  string `json:"url"`
		Lang string `json:"lang"`
	} `json:"subtitles,omitempty"`
}

// Retain the complete upstream object, including torrent/external sources and
// provider-specific metadata, so a passthrough response is truly unchanged.
func (s *Stream) UnmarshalJSON(b []byte) error {
	type plain Stream
	if err := json.Unmarshal(b, (*plain)(s)); err != nil {
		return err
	}
	s.raw = append(s.raw[:0], b...)
	return nil
}
func (s Stream) MarshalJSON() ([]byte, error) {
	if s.raw != nil {
		return s.raw, nil
	}
	type plain Stream
	return json.Marshal(plain(s))
}
func (s Stream) dubbed(local string) Stream {
	b, _ := json.Marshal(s)
	var fields map[string]json.RawMessage
	json.Unmarshal(b, &fields)
	fields["name"], _ = json.Marshal("🇮🇹 " + s.Name)
	fields["url"], _ = json.Marshal(local)
	var hints map[string]json.RawMessage
	json.Unmarshal(fields["behaviorHints"], &hints)
	if hints == nil {
		hints = map[string]json.RawMessage{}
	}
	hints["notWebReady"] = json.RawMessage(`true`)
	delete(hints, "proxyHeaders") // Local endpoints do not need origin headers.
	fields["behaviorHints"], _ = json.Marshal(hints)
	delete(fields, "headers")
	b, _ = json.Marshal(fields)
	var out Stream
	json.Unmarshal(b, &out)
	return out
}

func (s Stream) Origin() Origin {
	h := http.Header{}
	for k, v := range s.Headers {
		if !strings.ContainsAny(k+v, "\r\n") {
			h.Set(k, v)
		}
	}
	for k, v := range s.BehaviorHints.ProxyHeaders.Request {
		if !strings.ContainsAny(k+v, "\r\n") {
			h.Set(k, v)
		}
	}
	return Origin{s.URL, h}
}

type Session struct {
	mu                  sync.Mutex
	ID                  string `json:"id"`
	Key                 string `json:"-"`
	lookupKey           string
	ticket              string
	Content             Content `json:"content"`
	Name                string  `json:"name"`
	ContentName         string
	Order               int
	Playing             bool
	PlaybackAt          time.Time
	RequestAt           time.Time
	RequestMethod       string
	SampleIndex         int
	SampleTotal         int
	SamplePhase         string
	Passthrough         bool
	SourceCheckDeferred bool
	FallbackReason      string
	Created             time.Time `json:"created"`
	LastUsed            time.Time `json:"lastUsed"`
	Status              string    `json:"status"`
	Errors              []string  `json:"errors"`
	Position            float64   `json:"position"`
	PositionAt          time.Time `json:"positionAt"`
	Duration            float64   `json:"duration"`
	ProxyReason         string    `json:"proxyReason"`
	Aligning            bool      `json:"aligning"`
	Revision            int       `json:"revision"`
	stream              Stream
	prepare             sync.Once
	preparationStarted  bool
	ready               chan struct{}
	prepareErr          error
	video               *Asset
	variant             *playlist.MultivariantVariant
	clockBase           float64
	tracks              []Track
	sourceEnglish       *Track
	vixEnglish          *Track
	boundaries          []float64
	resources           map[string]resource
	directOnce          sync.Once
	directReady         chan struct{}
	directAccess        map[string]DirectAccess
	delivery            *Delivery
	alignDone           chan struct{}
	firstAligned        chan struct{}
	videoBase           int
	videoHighest        int
	videoSequence       uint64
	videoReady          map[int]bool
	videoChanged        chan struct{}
	startupZero         bool
	listedAsset         *Asset
	listedMaster        *HLS
	listedVariant       *playlist.MultivariantVariant
	listedVix           []Track
	listedEnglish       *Track
	listedReady         chan struct{}
	ctx                 context.Context
	cancel              context.CancelFunc
}
type resource struct {
	Origin   Origin
	Position float64
	Track    bool
	Force    bool
}

func (s *Session) note(err error) {
	// Players cancel requests when they seek, retry, or give up waiting. Those
	// cancellations are not source failures and should not fill the dashboard.
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := err.Error()
	for _, v := range s.Errors {
		if v == msg {
			return
		}
	}
	s.Errors = append(s.Errors, msg)
	if len(s.Errors) > 12 {
		s.Errors = s.Errors[len(s.Errors)-12:]
	}
}
func (s *Session) position(t float64) {
	s.mu.Lock()
	s.Position = t
	s.PositionAt = time.Now()
	s.LastUsed = time.Now()
	s.mu.Unlock()
}
func (s *Session) state(status string) { s.mu.Lock(); s.Status = status; s.mu.Unlock() }
func (s *Session) addResource(o Origin, position float64, track, force bool) string {
	key := identity(o.URL, fmt.Sprint(o.Headers), strconv.FormatBool(force), decimal(position))[:32]
	s.mu.Lock()
	s.resources[key] = resource{o, position, track, force}
	s.mu.Unlock()
	return "/media/" + s.ID + "/resource/" + key
}
func (s *Server) newSession(c Content, stream Stream) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newSessionLocked(c, stream)
}
func (s *Server) newSessionLocked(c Content, stream Stream) *Session {
	return s.newSessionIDLocked(c, stream, token())
}
func (s *Server) newSessionIDLocked(c Content, stream Stream, id string) *Session {
	key := identity(c.Type, c.ID, stream.URL, fmt.Sprint(stream.Origin().Headers))
	ctx, cancel := context.WithCancel(context.WithValue(s.ctx, cacheOwnerKey{}, id))
	v := &Session{ID: id, Key: key, lookupKey: key, Content: c, Name: stream.Name, Created: time.Now(), LastUsed: time.Now(), Status: "Available · waiting for player", Errors: []string{}, stream: stream, ready: make(chan struct{}), directReady: make(chan struct{}), videoBase: -1, videoReady: map[int]bool{}, videoChanged: make(chan struct{}), resources: map[string]resource{}, ctx: ctx, cancel: cancel}
	s.sessions[v.ID] = v
	return v
}
func (s *Server) prepareSession(ctx context.Context, v *Session) error {
	v.prepare.Do(func() {
		v.mu.Lock()
		v.preparationStarted = true
		v.mu.Unlock()
		go func() {
			defer close(v.ready)
			if v.listedReady != nil {
				select {
				case <-v.listedReady:
				case <-v.ctx.Done():
					v.prepareErr = v.ctx.Err()
					return
				}
			}
			if v.Passthrough {
				v.prepareErr = errors.New(v.FallbackReason)
				return
			}
			work, cancel := context.WithTimeout(v.ctx, 2*time.Minute)
			defer cancel()
			v.prepareErr = s.prepareMedia(work, v)
			v.mu.Lock()
			deferred := v.SourceCheckDeferred
			v.SourceCheckDeferred = false
			if deferred && v.prepareErr != nil {
				v.Passthrough = true
				v.FallbackReason = v.prepareErr.Error()
			}
			v.mu.Unlock()
			if v.prepareErr != nil {
				v.note(v.prepareErr)
				if deferred {
					v.state("Source unavailable · original stream")
				} else {
					v.state("Source unavailable")
				}
			} else {
				go s.ensureDirectAccess(v.ctx, v)
			}
		}()
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-v.ready:
		return v.prepareErr
	}
}
func (s *Server) loadMedia(ctx context.Context, o Origin) (*Asset, error) {
	a, e := s.Engine.OpenAsset(ctx, o, true)
	if e != nil {
		return nil, e
	}
	for depth := 0; a.HLS.Master != nil && depth < 3; depth++ {
		best, e := a.HLS.BestVariant()
		if e != nil {
			return nil, e
		}
		o, e = a.HLS.Child(best.URI)
		if e != nil {
			return nil, e
		}
		a, e = s.Engine.OpenAsset(ctx, o, true)
		if e != nil {
			return nil, e
		}
	}
	if a.HLS.Master != nil {
		return nil, errors.New("too many nested HLS masters")
	}
	return a, nil
}

func (s *Server) loadSubtitle(ctx context.Context, o Origin) (*Asset, error) {
	h, err := s.Net.LoadHLS(ctx, o)
	if err != nil {
		return nil, err
	}
	if h.Master != nil {
		return nil, errors.New("subtitle URI must be a WebVTT media playlist")
	}
	return &Asset{ID: identity(o.URL), Origin: h.Origin, HLS: h}, nil
}
func (s *Server) prepareMedia(ctx context.Context, v *Session) error {
	v.state("Inspecting source")
	a, originalMaster, variant := v.listedAsset, v.listedMaster, v.listedVariant
	var err error
	if a == nil {
		check, done := context.WithTimeout(ctx, time.Duration(s.Config.Get().SourceCheckTimeoutSeconds)*time.Second)
		a, originalMaster, variant, err = s.inspectSource(check, v.stream)
		done()
		if err != nil {
			return err
		}
	}
	if variant != nil {
		copyVariant := *variant
		v.variant = &copyVariant
	}
	v.video = a
	v.Duration = a.Duration()
	v.ProxyReason = "Video uses its origin directly when possible; generated tracks remain local."
	if a.File != nil {
		v.clockBase = 1 // room for B-frame decode timestamps before presentation
		v.boundaries = a.Index.Boundaries
		v.ProxyReason = "Ranged file input is required for on-demand video stream-copy remux."
	} else {
		v.boundaries = []float64{0}
		for _, seg := range a.HLS.Segments {
			v.boundaries = append(v.boundaries, seg.Start+seg.Duration)
		}
		if len(a.Origin.Headers) > 0 {
			v.ProxyReason = "The source supplies media headers; direct-access checks determine whether proxying is required."
		}
	}
	p, probeErr := s.Engine.Probe(ctx, a)
	if probeErr != nil {
		return fmt.Errorf("source video could not be read: %w", probeErr)
	}
	haveVideo := false
	for _, stream := range p.Streams {
		if stream.CodecType == "video" {
			haveVideo = true
		}
	}
	if !haveVideo {
		return errors.New("source contains no playable video track")
	}
	if a.HLS != nil {
		v.clockBase, _ = strconv.ParseFloat(p.Format.StartTime, 64)
		// Some fMP4 packagers include the initial encoder gap in EXTINF.
		// The first packet's PTS is then not the origin of the playlist clock.
		// A later segment establishes the ongoing clock without reading ahead
		// beyond one short probe window.
		if len(a.HLS.Segments) > 1 && !a.HLS.Segments[1].Discontinuity {
			at := a.HLS.Segments[1].Start
			if next, e := s.Engine.ProbeAt(ctx, a, at); e == nil {
				if clock, e := strconv.ParseFloat(next.Format.StartTime, 64); e == nil && !math.IsNaN(clock) && !math.IsInf(clock, 0) {
					v.clockBase = clock - at
				}
			} else {
				v.note(fmt.Errorf("source timeline probe: %w; using the initial clock", e))
			}
		}
	}
	for _, t := range p.Streams {
		if t.CodecType != "audio" {
			continue
		}
		lang := language(t.Tags.Language)
		track := Track{ID: fmt.Sprintf("source-%d", t.Index), Name: fmt.Sprintf("Original %s · %s · track %d", lang, strings.ToUpper(t.CodecName), t.Index), Lang: lang, Asset: a, Selector: fmt.Sprintf("0:%d", t.Index), Original: true}
		if t.Tags.Title != "" {
			track.Name = t.Tags.Title
		}
		v.tracks = append(v.tracks, track)
		if lang == "en" && v.sourceEnglish == nil {
			cp := track
			v.sourceEnglish = &cp
		}
	}
	if originalMaster != nil {
		for i, r := range originalMaster.Master.Renditions {
			if r.URI == nil {
				continue
			}
			if r.Type == playlist.MultivariantRenditionTypeAudio && r.GroupID != v.variant.Audio {
				continue
			}
			if r.Type == playlist.MultivariantRenditionTypeSubtitles && r.GroupID != v.variant.Subtitles {
				continue
			}
			if r.Type != playlist.MultivariantRenditionTypeAudio && r.Type != playlist.MultivariantRenditionTypeSubtitles {
				continue
			}
			o, e := originalMaster.Child(*r.URI)
			if e != nil {
				v.note(e)
				continue
			}
			var asset *Asset
			if r.Type == playlist.MultivariantRenditionTypeSubtitles {
				asset, e = s.loadSubtitle(ctx, o)
			} else {
				asset, e = s.loadMedia(ctx, o)
			}
			if e != nil {
				v.note(fmt.Errorf("original rendition: %w", e))
				continue
			}
			track := Track{ID: fmt.Sprintf("original-%d", i), Name: r.Name, Lang: language(r.Language), Asset: asset, Selector: "0:a:0", Original: true, Subtitle: r.Type == playlist.MultivariantRenditionTypeSubtitles}
			v.tracks = append(v.tracks, track)
			if track.Lang == "en" && !track.Subtitle {
				cp := track
				v.sourceEnglish = &cp
			}
		}
	}
	// Bind cached alignment to the actual source identity and duration. Exact
	// signed URL identity is deliberately conservative: different edits cannot
	// accidentally inherit one another's alignment.
	sourceVersion := ""
	if a.File != nil {
		sourceVersion = fmt.Sprintf("%s:%d", a.File.ETag, a.File.Size)
	} else {
		sourceVersion = identity(a.HLS.Raw)
	}
	v.mu.Lock()
	v.Key = identity(v.Content.Type, v.Content.ID, v.stream.URL, fmt.Sprint(v.stream.Origin().Headers), decimal(v.Duration), sourceVersion)
	v.mu.Unlock()
	cfg := s.Config.Get()
	if v.listedVix != nil {
		v.tracks = append(v.tracks, v.listedVix...)
		v.vixEnglish = v.listedEnglish
	} else {
		v.state("Resolving Italian audio")
		bundle := s.resolveVixBundle(ctx, v.Content)
		if bundle.err != nil {
			return bundle.err
		}
		v.tracks = append(v.tracks, bundle.tracks...)
		v.vixEnglish = bundle.english
	}
	if v.vixEnglish == nil || v.sourceEnglish == nil {
		v.note(errors.New("English reference missing: using offset 0; manual synchronization remains available"))
		v.state("Ready · offset 0 fallback")
	} else {
		cached := s.Alignments.Get(v.Key)
		if !cached.Updated.IsZero() && ((cached.AutoComplete && cached.AutoExpected == cfg.AlignmentSamples && cached.AutoWindow == cfg.AlignmentSampleSeconds && cached.AutoRadius == cfg.SearchRadius) || cached.Manual != nil) {
			v.state("Ready · cached alignment")
		} else {
			v.state("Ready · analyzing English audio")
			s.startAlignment(v, nil)
		}
	}
	return nil
}
func (s *Server) vixTracks(ctx context.Context, v *Session, master *HLS) error {
	if master.Master == nil {
		asset := &Asset{ID: identity(master.Origin.URL), Origin: master.Origin, HLS: master}
		p, err := s.Engine.Probe(ctx, asset)
		if err != nil {
			return err
		}
		italian := false
		for _, r := range p.Streams {
			if r.CodecType != "audio" {
				continue
			}
			lang := language(r.Tags.Language)
			if lang != "en" && lang != "it" {
				continue
			}
			name := "Vixsrc English"
			if lang == "it" {
				name = "Italiano · Vixsrc"
				italian = true
			}
			t := Track{ID: fmt.Sprintf("vix-%s-%d", lang, r.Index), Name: name, Lang: lang, Asset: asset, Selector: fmt.Sprintf("0:%d", r.Index)}
			v.tracks = append(v.tracks, t)
			if lang == "en" {
				cp := t
				v.vixEnglish = &cp
			}
		}
		if !italian {
			return errors.New("Vixsrc has no Italian audio track")
		}
		return nil
	}
	have := map[string]bool{}
	for i, r := range master.Master.Renditions {
		lang := language(r.Language)
		if lang != "it" && lang != "en" {
			lang = language(r.Name)
		}
		if lang != "it" && lang != "en" {
			continue
		}
		if r.URI == nil {
			continue
		}
		sub := r.Type == playlist.MultivariantRenditionTypeSubtitles
		if !sub && r.Type != playlist.MultivariantRenditionTypeAudio {
			continue
		}
		key := lang + strconv.FormatBool(sub)
		if have[key] {
			continue
		}
		o, e := master.Child(*r.URI)
		if e != nil {
			v.note(e)
			continue
		}
		var a *Asset
		if sub {
			a, e = s.loadSubtitle(ctx, o)
		} else {
			a, e = s.loadMedia(ctx, o)
		}
		if e != nil {
			v.note(fmt.Errorf("Vixsrc %s rendition: %w", lang, e))
			continue
		}
		name := "Vixsrc English"
		if lang == "it" {
			name = "Italiano · Vixsrc"
		}
		t := Track{ID: fmt.Sprintf("vix-%s-%d", lang, i), Name: name, Lang: lang, Asset: a, Selector: "0:a:0", Subtitle: sub}
		v.tracks = append(v.tracks, t)
		have[key] = true
		if lang == "en" && !sub {
			cp := t
			v.vixEnglish = &cp
		}
	}
	if !have["itfalse"] {
		return errors.New("Vixsrc has no Italian audio rendition")
	}
	return nil
}
func (s *Server) startAlignment(v *Session, from *float64) bool {
	v.mu.Lock()
	if v.Aligning || v.vixEnglish == nil || v.sourceEnglish == nil {
		v.mu.Unlock()
		return false
	}
	v.Aligning = true
	v.alignDone = make(chan struct{})
	v.firstAligned = make(chan struct{})
	done := v.alignDone
	first := v.firstAligned
	v.mu.Unlock()
	go func() {
		var firstOnce sync.Once
		finishFirst := func() { firstOnce.Do(func() { close(first) }) }
		defer finishFirst()
		defer func() { v.mu.Lock(); v.Aligning = false; v.Revision++; close(done); v.mu.Unlock() }()
		cfg := s.Config.Get()
		window := float64(cfg.AlignmentSampleSeconds)
		current := s.Alignments.Get(v.Key)
		duration := min(v.video.Duration(), v.vixEnglish.Asset.Duration())
		samples := alignmentPositions(duration, cfg.AlignmentSamples, window, cfg.SearchRadius)
		immediateFile := from == nil && cfg.StartImmediately && v.video.File != nil
		if from != nil {
			samples = []float64{max(0, *from-current.At(*from, cfg.MinConfidence)-10)}
		} else if immediateFile {
			v.mu.Lock()
			v.SamplePhase = "Waiting for downloaded video playback data"
			v.mu.Unlock()
		}
		if from == nil {
			if err := s.Alignments.UpdateContext(v.ctx, v.Key, func(a *Alignment) {
				a.AutoSamples = nil
				a.AutoExpected = len(samples)
				a.AutoWindow = cfg.AlignmentSampleSeconds
				a.AutoRadius = cfg.SearchRadius
				a.AutoComplete = false
				a.Anchors = nil
				a.Offset = 0
				a.Confidence = 0
				a.DifferentEdit = false
			}); err != nil {
				v.note(err)
			}
			current = Alignment{}
		}
		source := func(ctx context.Context, t, d float64) ([]int16, error) {
			return s.Engine.PCM(ctx, *v.sourceEnglish, t, d)
		}
		vix := func(ctx context.Context, t, d float64) ([]int16, error) {
			return s.Engine.PCM(ctx, *v.vixEnglish, t, d)
		}
		previousAt := 0.0
		previousSequence := uint64(0)
		for i, at := range samples {
			if v.ctx.Err() != nil {
				return
			}
			var anchor Anchor
			var err error
			lastEnd := -1
			sourceExtractor := source
			if from == nil && i == 0 && !cfg.StartImmediately {
				// The gated first check primes the beginning of the source file
				// for playback, then matches a short clip after the radius.
				sourceExtractor = func(ctx context.Context, t, d float64) ([]int16, error) {
					return s.Engine.PCMFromZero(ctx, *v.sourceEnglish, t, d)
				}
			}
			for {
				if immediateFile {
					v.mu.Lock()
					v.SampleIndex = i + 1
					v.SampleTotal = len(samples)
					v.SamplePhase = fmt.Sprintf("Waiting for about %.0fs of downloaded video", window+cfg.SearchRadius)
					v.mu.Unlock()
					minAt := 0.0
					if i > 0 {
						minAt = max(samples[i], previousAt+window)
					}
					at, lastEnd, previousSequence, err = v.playbackSample(v.ctx, minAt, cfg.SearchRadius, duration, window, current.At(minAt, cfg.MinConfidence), lastEnd, previousSequence)
					if err != nil {
						break
					}
				}
				v.mu.Lock()
				v.SampleIndex = i + 1
				v.SampleTotal = len(samples)
				v.SamplePhase = fmt.Sprintf("Matching English audio near %.0fs", at)
				v.mu.Unlock()
				ctx, cancel := context.WithTimeout(context.WithValue(v.ctx, backgroundWorkKey{}, true), 30*time.Second)
				radius := cfg.SearchRadius
				if immediateFile {
					ctx = context.WithValue(ctx, cacheOnlyFileKey{}, true)
				}
				anchor, err = FindAnchor(ctx, sourceExtractor, vix, at, current.At(at, cfg.MinConfidence), radius, duration, window)
				cancel()
				if immediateFile && errors.Is(err, errRangeNotCached) {
					continue
				}
				if immediateFile {
					v.mu.Lock()
					seeked := v.videoSequence != previousSequence
					v.mu.Unlock()
					if seeked {
						lastEnd = -1
						continue
					}
				}
				break
			}
			if v.ctx.Err() != nil {
				return
			}
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					err = errors.New("30-second analysis limit exceeded; source may be too slow")
				}
				v.note(fmt.Errorf("alignment sample at %.0fs: %w", at, err))
				if i == 0 {
					finishFirst()
				}
				continue
			}
			err = s.Alignments.UpdateContext(v.ctx, v.Key, func(a *Alignment) {
				a.Anchors = append(a.Anchors, anchor)
				if len(a.Anchors) > 30 {
					a.Anchors = a.Anchors[len(a.Anchors)-30:]
				}
				if from != nil {
					if anchor.Confidence >= cfg.MinConfidence {
						boundary := Boundary{SourceTime: s.boundaryAt(v, *from), Offset: anchor.Offset, Confidence: anchor.Confidence, Created: time.Now()}
						a.Boundaries = append(a.Boundaries, boundary)
						sort.SliceStable(a.Boundaries, func(i, j int) bool { return a.Boundaries[i].SourceTime < a.Boundaries[j].SourceTime })
						a.Manual = nil
					}
				} else {
					a.AutoSamples = append(a.AutoSamples, anchor)
					a.Choose(cfg.MinConfidence)
				}
			})
			if err != nil {
				v.note(fmt.Errorf("persist alignment: %w", err))
			}
			current = s.Alignments.Get(v.Key)
			previousAt = at
			if i == 0 {
				finishFirst()
			}
			v.mu.Lock()
			v.Revision++
			v.mu.Unlock()
		}
		if v.ctx.Err() != nil {
			return
		}
		if from == nil {
			if err := s.Alignments.UpdateContext(v.ctx, v.Key, func(a *Alignment) { a.AutoComplete = true }); err != nil {
				v.note(err)
			}
		}
		current = s.Alignments.Get(v.Key)
		v.mu.Lock()
		v.SamplePhase = "Analysis complete · new audio segments use the calculated offset"
		v.mu.Unlock()
		if current.Confidence < cfg.MinConfidence {
			v.state("Ready · offset 0 fallback")
		} else if current.DifferentEdit {
			v.state("Ready · editions differ; earliest reliable offset")
		} else {
			v.state("Ready · aligned")
		}
	}()
	return true
}

func alignmentPositions(duration float64, count int, window, radius float64) []float64 {
	// A source clip after the radius lets Vixsrc be searched in both
	// directions; gated startup reads the source prefix from time zero.
	return alignmentPositionsFrom(duration, count, radius, window)
}
func alignmentPositionsFrom(duration float64, count int, first, window float64) []float64 {
	first = max(0, min(first, duration-window))
	last := max(first, min(duration-window, max(min(duration*.8, duration-window-8), first+window*float64(count-1))))
	out := make([]float64, count)
	for i := range out {
		out[i] = first
		if count > 1 {
			out[i] += (last - first) * float64(i) / float64(count-1)
		}
	}
	return out
}
func (v *Session) signalVideoLocked() {
	if v.videoChanged == nil {
		v.videoChanged = make(chan struct{})
	}
	close(v.videoChanged)
	v.videoChanged = make(chan struct{})
}
func (v *Session) videoSegmentRequested(index int) {
	v.mu.Lock()
	if v.videoBase < 0 || index < v.videoBase || index > v.videoHighest+1 {
		v.videoBase, v.videoHighest = index, index
		v.videoSequence++
	} else if index > v.videoHighest {
		v.videoHighest = index
	}
	v.signalVideoLocked()
	v.mu.Unlock()
}
func (v *Session) videoSegmentCompleted(index int) {
	v.mu.Lock()
	if v.videoReady == nil {
		v.videoReady = map[int]bool{}
	}
	v.videoReady[index] = true
	v.signalVideoLocked()
	v.mu.Unlock()
}

// A file sample uses only video segments that the player has actually
// requested and that have finished remuxing into the shared byte cache.
func (v *Session) playbackSample(ctx context.Context, minAt, radius, duration, window, expectedOffset float64, afterEnd int, previousSequence uint64) (float64, int, uint64, error) {
	for {
		v.mu.Lock()
		if v.videoChanged == nil {
			v.videoChanged = make(chan struct{})
		}
		changed := v.videoChanged
		base, sequence := v.videoBase, v.videoSequence
		if sequence != previousSequence {
			afterEnd = -1
			minAt = 0
		}
		if base >= 0 && base < len(v.boundaries)-1 {
			start := v.boundaries[base]
			last := base - 1
			for i := base; i < len(v.boundaries)-1 && v.videoReady[i]; i++ {
				last = i
			}
			end := start
			if last >= base {
				end = v.boundaries[last+1]
			}
			at := max(start+radius-expectedOffset, minAt)
			if afterEnd >= 0 && last > afterEnd {
				// A cache miss can mean older video bytes were evicted. Follow
				// the newly completed part of playback on the next attempt.
				at = max(at, end-window-expectedOffset)
			}
			limit := duration - window - expectedOffset
			if start > limit || at > limit {
				v.mu.Unlock()
				return 0, 0, sequence, errors.New("not enough video remains for an alignment window")
			}
			required := min(duration, at+expectedOffset+window)
			if last > afterEnd && end+0.001 >= required {
				v.mu.Unlock()
				return at, last, sequence, nil
			}
			if last == len(v.boundaries)-2 && afterEnd >= last {
				v.mu.Unlock()
				return 0, 0, sequence, errRangeNotCached
			}
		}
		v.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, 0, 0, ctx.Err()
		}
	}
}
func (s *Server) boundaryAt(v *Session, t float64) float64 {
	for i := len(v.boundaries) - 1; i >= 0; i-- {
		if v.boundaries[i] <= t {
			return v.boundaries[i]
		}
	}
	return 0
}
