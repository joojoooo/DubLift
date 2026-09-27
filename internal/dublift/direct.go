package dublift

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DirectAccess describes an anonymous playback check. Only manifests, short
// resource prefixes and 16-byte keys are read. No cookies or supplied headers
// are used, so success does not depend on DubLift's authenticated session.
type DirectAccess struct {
	Ready          bool
	RemoteManifest bool
	Clock          float64
	ClockKnown     bool
	Reason         string
}
type Delivery struct {
	VideoDirect           bool            `json:"videoDirect"`
	AudioDirect           bool            `json:"audioDirect"`
	CanStopServer         bool            `json:"canStopServer"`
	OtherTracksNeedServer bool            `json:"otherTracksNeedServer"`
	Forced                bool            `json:"forced"`
	Pending               bool            `json:"pending"`
	Reasons               []string        `json:"reasons"`
	PlayerOffset          *float64        `json:"playerOffset,omitempty"`
	Tracks                map[string]bool `json:"tracks"`
}

func (s *Server) ensureDirectAccess(ctx context.Context, v *Session) {
	if s.Config.Get().PreferProxy {
		return
	}
	v.directOnce.Do(func() {
		go func() {
			defer close(v.directReady)
			work, cancel := context.WithTimeout(context.WithValue(v.ctx, backgroundWorkKey{}, true), 45*time.Second)
			defer cancel()
			net := NewNetwork()
			net.Client.Jar = nil
			defer net.Client.CloseIdleConnections()
			assets := map[string]*Asset{v.video.ID: v.video}
			for _, t := range v.tracks {
				if t.Asset.HLS != nil {
					assets[t.Asset.ID] = t.Asset
				}
			}
			results := map[string]DirectAccess{}
			var mu sync.Mutex
			var wg sync.WaitGroup
			slots := make(chan struct{}, 3)
			for _, asset := range assets {
				a := asset
				wg.Go(func() {
					select {
					case slots <- struct{}{}:
						defer func() { <-slots }()
					case <-work.Done():
						return
					}
					result := checkDirectAsset(work, net, a)
					if result.Ready {
						if a == v.video {
							result.Clock = v.clockBase
							result.ClockKnown = true
						} else {
							subtitleOnly := true
							for _, t := range v.tracks {
								if t.Asset == a && !t.Subtitle {
									subtitleOnly = false
								}
							}
							if subtitleOnly {
								if len(a.HLS.Segments) > 0 {
									b, _, err := net.Fetch(work, Origin{URL: a.HLS.Segments[0].URI}, manifestLimit)
									if err == nil && strings.HasPrefix(strings.TrimPrefix(string(b), "\ufeff"), "WEBVTT") {
										result.ClockKnown = true
										if m := mapRE.FindStringSubmatch(string(b)); m != nil {
											local, _ := parseVTTTime(m[1])
											ticks, _ := strconv.ParseFloat(m[2], 64)
											result.Clock = ticks/90000 - local
										}
									}
								}
							} else {
								p, e := s.Engine.ProbeAt(work, a, 0)
								if e == nil {
									clock, e := strconv.ParseFloat(p.Format.StartTime, 64)
									if e == nil && !math.IsNaN(clock) && !math.IsInf(clock, 0) {
										result.Clock = clock
										result.ClockKnown = true
									}
								}
							}
						}
					}
					mu.Lock()
					results[a.ID] = result
					mu.Unlock()
				})
			}
			wg.Wait()
			v.mu.Lock()
			v.directAccess = results
			v.mu.Unlock()
		}()
	})
	select {
	case <-ctx.Done():
	case <-v.directReady:
	}
}

func checkDirectAsset(ctx context.Context, n *Network, a *Asset) DirectAccess {
	if a.HLS == nil {
		return DirectAccess{Reason: "File video needs on-demand remuxing."}
	}
	h := a.HLS
	if len(h.Segments) == 0 {
		return DirectAccess{Reason: "A finite media playlist is required for direct playback."}
	}
	result := DirectAccess{}
	// A playlist that needs headers can still be served once as a finite local
	// snapshot with external segment URLs. It creates no ongoing media proxy.
	if b, _, err := n.Fetch(ctx, Origin{URL: h.Origin.URL}, manifestLimit); err == nil && strings.HasPrefix(strings.TrimSpace(string(b)), "#EXTM3U") {
		result.RemoteManifest = true
	}
	resources := map[string]bool{}
	// Sample across the whole playlist. Some CDNs rotate hostnames per
	// segment; enumerating every host would itself prefetch the whole title.
	for _, index := range []int{0, len(h.Segments) / 4, len(h.Segments) / 2, len(h.Segments) * 3 / 4, len(h.Segments) - 1} {
		seg := h.Segments[index]
		resources[seg.URI] = false
		for i, line := range []string{seg.Key, seg.Map} {
			if m := uriRE.FindStringSubmatch(line); m != nil {
				raw, e := resolveURL(h.Origin.URL, m[1])
				if e != nil {
					return DirectAccess{Reason: "A media key or map cannot be accessed directly."}
				}
				resources[raw] = i == 0
			}
		}
	}
	urls := make([]string, 0, len(resources))
	for raw := range resources {
		urls = append(urls, raw)
	}
	sort.Strings(urls)
	for _, raw := range urls {
		if resources[raw] {
			b, _, err := n.Fetch(ctx, Origin{URL: raw}, 16)
			if err != nil || len(b) != 16 {
				return DirectAccess{Reason: "An encryption key requires DubLift's request headers or cookies."}
			}
			continue
		}
		resp, err := n.request(ctx, Origin{URL: raw}, "GET", "bytes=0-511")
		if err != nil {
			return DirectAccess{Reason: "Anonymous media access could not be verified."}
		}
		ok := resp.StatusCode == 200 || resp.StatusCode == 206
		contentType := strings.ToLower(resp.Header.Get("Content-Type"))
		if strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/json") {
			ok = false
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		n.Bytes.Add(int64(len(data)))
		if !ok || readErr != nil || len(data) == 0 {
			return DirectAccess{Reason: "Media requires DubLift's request headers or cookies."}
		}
	}
	result.Ready = true
	return result
}

// Finite tolerance is deliberately based on every sample in the last complete
// automatic run, including confidence and coverage, not a single good match.
func directAlignment(a Alignment, cfg Settings, duration, clockDelta float64) (bool, string) {
	if !cfg.DirectPlayback {
		return false, "Automatic direct audio is disabled."
	}
	if cfg.DirectTolerance == nil {
		return true, "Infinite tolerance: adjust audio delay in the player."
	}
	tolerance := *cfg.DirectTolerance
	if !a.AutoComplete {
		return false, "Waiting for all automatic alignment samples."
	}
	if a.AutoExpected < 2 {
		return false, "Finite direct audio requires at least two samples across the runtime."
	}
	if len(a.AutoSamples) != a.AutoExpected {
		return false, "Some alignment samples failed; direct audio was not verified across the runtime."
	}
	if len(a.Boundaries) > 0 {
		return false, "Saved timeline corrections require generated audio."
	}
	if a.DifferentEdit {
		return false, "The sampled editions differ."
	}
	if a.Manual != nil && math.Abs(*a.Manual) > tolerance {
		return false, "The manual offset requires generated audio."
	}
	earliest, latest := math.Inf(1), math.Inf(-1)
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, sample := range a.AutoSamples {
		if sample.Confidence < cfg.MinConfidence {
			return false, "An alignment sample is below minimum confidence."
		}
		if math.Abs(sample.Offset) > tolerance || math.Abs(sample.Offset+clockDelta) > tolerance {
			return false, "The measured offset or media clocks exceed the direct tolerance."
		}
		earliest = min(earliest, sample.SourceTime)
		latest = max(latest, sample.SourceTime)
		lo = min(lo, sample.Offset)
		hi = max(hi, sample.Offset)
	}
	if earliest > max(60, duration*.2) || latest < duration*.7 {
		return false, "Alignment samples do not cover the beginning and later playback."
	}
	if hi-lo > min(.25, 2*tolerance) {
		return false, "The alignment samples do not agree."
	}
	return true, "All automatic samples agree within the direct tolerance."
}

// Matching decoded content alone cannot prove that two native HLS clocks
// reset together. Differing discontinuities need locally generated timestamps.
func compatibleDiscontinuities(video, audio *HLS) bool {
	boundaries := func(h *HLS) []float64 {
		var out []float64
		if h != nil {
			for _, seg := range h.Segments {
				if seg.Discontinuity && seg.Start > 0 {
					out = append(out, seg.Start)
				}
			}
		}
		return out
	}
	a, b := boundaries(video), boundaries(audio)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > .05 {
			return false
		}
	}
	return true
}

func (s *Server) deliveryFor(v *Session) Delivery {
	cfg := s.Config.Get()
	d := Delivery{Reasons: []string{}, Tracks: map[string]bool{}, Forced: cfg.DirectPlayback && cfg.DirectTolerance == nil}
	if cfg.PreferProxy {
		d.Reasons = append(d.Reasons, "Proxying is preferred in settings.")
		return d
	}
	v.mu.Lock()
	access := v.directAccess
	v.mu.Unlock()
	if access == nil {
		d.Pending = true
		d.Reasons = append(d.Reasons, "Checking whether media can play without request headers or cookies.")
		return d
	}
	video := access[v.video.ID]
	d.VideoDirect = video.Ready
	if !video.Ready {
		reason := video.Reason
		if reason == "" {
			reason = "Video still requires DubLift."
		}
		d.Reasons = append(d.Reasons, reason)
	}
	alignment := s.Alignments.Get(v.Key)
	italian := false
	for _, t := range v.tracks {
		a := access[t.Asset.ID]
		if t.Original {
			d.Tracks[t.ID] = a.Ready
			continue
		}
		ready, reason := false, ""
		if !t.Subtitle && t.Selector != "0:a:0" {
			reason = "Muxed Vixsrc audio must be extracted by DubLift."
		} else if !a.Ready {
			reason = a.Reason
			if reason == "" {
				reason = "Direct audio access could not be verified."
			}
		} else {
			clock := a.Clock
			known := a.ClockKnown
			delta := v.clockBase - clock
			if known || cfg.DirectTolerance == nil {
				ready, reason = directAlignment(alignment, cfg, v.Duration, delta)
				if ready && cfg.DirectTolerance != nil && !t.Subtitle && !compatibleDiscontinuities(v.video.HLS, t.Asset.HLS) {
					ready, reason = false, "The native audio and video clocks reset at different positions."
				}
			} else {
				reason = "The native audio clock could not be verified."
			}
			if t.Lang == "it" && !t.Subtitle && known {
				offset := alignment.Offset + delta
				if alignment.Manual != nil {
					offset = *alignment.Manual + delta
				}
				d.PlayerOffset = &offset
			}
		}
		d.Tracks[t.ID] = ready
		if t.Lang == "it" && !t.Subtitle {
			italian = true
			d.AudioDirect = ready
			d.Reasons = append(d.Reasons, reason)
		}
	}
	if !italian {
		d.Reasons = append(d.Reasons, "Italian audio is unavailable; original audio remains selectable.")
	}
	d.CanStopServer = d.VideoDirect && d.AudioDirect
	// Subtitle playlists and optional renditions also need to remain reachable
	// if the player selects them. Generated subtitles mean the app must stay on.
	if d.CanStopServer {
		for _, t := range v.tracks {
			if !d.Tracks[t.ID] {
				d.OtherTracksNeedServer = true
				d.Reasons = append(d.Reasons, "Video and Italian audio can continue directly. Other renditions may still need DubLift; leave those unselected before stopping it.")
				break
			}
		}
	}
	return d
}

func (s *Server) awaitStartupAlignment(ctx context.Context, v *Session) {
	cfg := s.Config.Get()
	if cfg.StartImmediately {
		return
	}
	v.mu.Lock()
	done := v.firstAligned
	running := v.Aligning
	v.mu.Unlock()
	if !running || done == nil {
		return
	}
	// The first attempted sample gates startup. A failed attempt releases the
	// player with offset zero; later samples never delay the selected stream.
	select {
	case <-done:
	case <-ctx.Done():
	case <-v.ctx.Done():
	}
}

func (v *Session) directURI(a *Asset) string {
	v.mu.Lock()
	access := v.directAccess[a.ID]
	v.mu.Unlock()
	if access.RemoteManifest {
		return a.HLS.Origin.URL
	}
	return "/media/" + v.ID + "/direct/" + a.ID + ".m3u8"
}
func (v *Session) asset(id string) *Asset {
	if v.video.ID == id {
		return v.video
	}
	for _, t := range v.tracks {
		if t.Asset.ID == id {
			return t.Asset
		}
	}
	return nil
}
func directPlaylist(h *HLS) string {
	var b strings.Builder
	for _, line := range strings.Split(h.Raw, "\n") {
		l := strings.TrimSpace(line)
		if l != "" && !strings.HasPrefix(l, "#") {
			if u, err := resolveURL(h.Origin.URL, l); err == nil {
				b.WriteString(u)
			}
		} else {
			b.WriteString(rewriteURI(line, h.Origin.URL, func(s string) string { return s }))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (s *Server) serveDirect(w http.ResponseWriter, r *http.Request, v *Session, id string) {
	a := v.asset(strings.TrimSuffix(id, ".m3u8"))
	if a == nil || a.HLS == nil {
		http.NotFound(w, r)
		return
	}
	v.mu.Lock()
	access := v.directAccess[a.ID]
	v.mu.Unlock()
	if !access.Ready {
		failure(w, 409, errors.New("direct access has not been verified"))
		return
	}
	w.Header().Set("X-DubLift-Delivery", "direct-media")
	sendPlaylist(w, []byte(directPlaylist(a.HLS)))
}

func (d Delivery) String() string {
	mode := "Generated audio"
	if d.AudioDirect {
		mode = "Direct audio"
	}
	video := "local video"
	if d.VideoDirect {
		video = "direct video"
	}
	return fmt.Sprintf("%s · %s", video, mode)
}
