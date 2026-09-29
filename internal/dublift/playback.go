package dublift

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

func sendPlaylist(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}
func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/media/"), "/")
	if len(p) < 2 {
		http.NotFound(w, r)
		return
	}
	v := s.getSession(p[0])
	if v == nil && len(p) == 2 && p[1] == "master.m3u8" && r.URL.Query().Get("resume") != "" {
		encoded := r.URL.Query().Get("resume")
		if r.Method == "HEAD" {
			if _, err := s.readPlayback(p[0], encoded); err == nil {
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				return
			}
		} else {
			v, _ = s.restorePlayback(p[0], encoded)
		}
	}
	if v == nil {
		http.Error(w, "session expired; request fresh streams", 410)
		return
	}
	if p[1] == "master.m3u8" {
		if !s.playerRequest(v, r) {
			http.Error(w, "stream result expired", 410)
			return
		}
		if r.Method == "HEAD" {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			return
		}
	}
	ctx, release := sessionRequestContext(r.Context(), v)
	defer release()
	r = r.WithContext(ctx)
	v.mu.Lock()
	v.LastUsed = time.Now()
	v.mu.Unlock()
	if p[1] == "resource" && len(p) == 3 {
		s.resource(w, r, v, p[2])
		return
	}
	if err := s.prepareSession(r.Context(), v); err != nil {
		v.note(err)
		if p[1] == "master.m3u8" {
			v.mu.Lock()
			v.Passthrough = true
			v.FallbackReason = err.Error()
			v.Status = "Returning original upstream stream"
			v.mu.Unlock()
			http.Redirect(w, r, v.stream.URL, http.StatusTemporaryRedirect)
			return
		}
		failure(w, 502, err)
		return
	}
	switch {
	case p[1] == "master.m3u8":
		v.mu.Lock()
		if v.delivery == nil {
			v.startupZero = s.Config.Get().StartImmediately && v.Aligning
		}
		v.mu.Unlock()
		s.awaitStartupAlignment(r.Context(), v)
		b, err := s.master(v)
		if err != nil {
			failure(w, 500, err)
			return
		}
		sendPlaylist(w, b)
	case p[1] == "direct" && len(p) == 3:
		s.serveDirect(w, r, v, p[2])
	case p[1] == "video.m3u8":
		if v.video.HLS != nil {
			sendPlaylist(w, []byte(s.proxyPlaylist(v, v.video, true)))
		} else {
			sendPlaylist(w, []byte(s.generatedPlaylist(v, nil)))
		}
	case p[1] == "video" && len(p) == 4:
		n, err := strconv.Atoi(p[2])
		if err != nil || n < 0 || n >= len(v.boundaries)-1 || v.video.File == nil || (p[3] != "init.mp4" && p[3] != "segment.m4s") {
			http.NotFound(w, r)
			return
		}
		if p[3] == "init.mp4" {
			v.mu.Lock()
			init := v.videoInit
			v.mu.Unlock()
			if len(init) > 0 {
				serveBytes(w, r, "video/mp4", init)
				return
			}
		}
		start := v.boundaries[n]
		if p[3] == "segment.m4s" {
			v.position(start)
			v.videoSegmentRequested(n)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		ctx = context.WithValue(ctx, videoBytesKey{}, &v.videoDownload)
		data, err := s.fileVideo(ctx, v, n)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("video segment at %.1fs exceeded the 60-second read/remux limit; the source may be too slow", start)
			} else {
				err = fmt.Errorf("video segment at %.1fs: %w", start, err)
			}
			v.note(err)
			failure(w, 502, err)
			return
		}
		init, media, err := splitFMP4(data)
		if err != nil {
			v.note(err)
			failure(w, 502, err)
			return
		}
		v.mu.Lock()
		if v.videoInit == nil {
			v.videoInit = append([]byte(nil), init...)
		}
		v.mu.Unlock()
		if p[3] == "init.mp4" {
			data = init
		} else {
			data = media
			v.videoSegmentCompleted(n)
			s.prefetchFileVideo(v, n+1)
		}
		serveBytes(w, r, "video/mp4", data)
	case p[1] == "track" && len(p) >= 3:
		id := strings.TrimSuffix(p[2], ".m3u8")
		var track *Track
		for _, t := range v.tracks {
			if t.ID == id {
				copy := t
				track = &copy
				break
			}
		}
		if track == nil {
			http.NotFound(w, r)
			return
		}
		if len(p) == 3 && strings.HasSuffix(p[2], ".m3u8") {
			if track.Original && track.Asset.HLS != nil && track.Asset != v.video {
				sendPlaylist(w, []byte(s.proxyPlaylist(v, track.Asset, !track.Subtitle)))
			} else {
				sendPlaylist(w, []byte(s.generatedPlaylist(v, track)))
			}
			return
		}
		if len(p) != 4 {
			http.NotFound(w, r)
			return
		}
		n, err := strconv.Atoi(strings.Split(p[3], ".")[0])
		if err != nil || n < 0 || n >= len(v.boundaries)-1 {
			http.NotFound(w, r)
			return
		}
		start := v.boundaries[n]
		duration := v.boundaries[n+1] - start
		if !track.Subtitle {
			v.audioPosition(start)
		}
		offset := 0.0
		if !track.Original {
			offset = s.playbackOffset(v, start)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		clockBase, err := s.sectionClock(ctx, v, n)
		if err != nil {
			v.note(err)
			failure(w, 502, err)
			return
		}
		key := fmt.Sprintf("track:%s:%s:%d:%.6f:%.6f", v.Key, track.ID, n, offset, clockBase)
		data, err := s.Engine.Cache.Get(ctx, key, func() ([]byte, error) {
			if track.Subtitle {
				return s.subtitles(ctx, v, *track, start, duration, offset, clockBase)
			}
			return s.Engine.Audio(ctx, *track, start, duration, offset, clockBase)
		})
		if err != nil {
			v.note(err)
			failure(w, 502, err)
			return
		}
		kind := "video/mp2t"
		if track.Subtitle {
			kind = "text/vtt"
		} else {
			s.prefetchAudio(v, *track, n)
		}
		serveBytes(w, r, kind, data)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) prefetchFileVideo(v *Session, n int) {
	if s.videoPrefetch == nil || v.video == nil || v.video.File == nil || n >= len(v.boundaries)-1 {
		return
	}
	ctx := context.WithValue(v.ctx, videoBytesKey{}, &v.videoDownload)
	v.videoPrefetch.schedule(ctx, n, s.lookaheadEnd(v, n), func(ctx context.Context, index int) error {
		select {
		case s.videoPrefetch <- struct{}{}:
			defer func() { <-s.videoPrefetch }()
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := s.fileVideo(ctx, v, index)
		return err
	})
}

func (s *Server) prefetchAudio(v *Session, track Track, n int) {
	if n+1 >= len(v.boundaries)-1 {
		return
	}
	v.mu.Lock()
	// Players probe segment zero of every rendition. Only warm the default
	// Italian track until subsequent requests identify a different selection.
	if n == 0 && (track.Original || track.Lang != "it" || v.audioPrefetchID != "") {
		v.mu.Unlock()
		return
	}
	if v.audioPrefetchID != track.ID {
		v.audioPrefetch.stop()
		v.audioPrefetchID = track.ID
	}
	v.audioPrefetch.schedule(v.ctx, n+1, s.lookaheadEnd(v, n+1), func(ctx context.Context, index int) error {
		start := v.boundaries[index]
		offset := 0.0
		if !track.Original {
			offset = s.playbackOffset(v, start)
		}
		clockBase, err := s.sectionClock(ctx, v, index)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("track:%s:%s:%d:%.6f:%.6f", v.Key, track.ID, index, offset, clockBase)
		_, err = s.Engine.Cache.Get(ctx, key, func() ([]byte, error) {
			return s.Engine.Audio(ctx, track, start, v.boundaries[index+1]-start, offset, clockBase)
		})
		return err
	})
	v.mu.Unlock()
}

func (s *Server) playbackOffset(v *Session, at float64) float64 {
	a := s.Alignments.Get(v.Key)
	v.mu.Lock()
	zero := v.startupZero
	v.mu.Unlock()
	if zero && !a.AutoComplete && a.Manual == nil {
		return 0
	}
	return a.At(at, s.Config.Get().MinConfidence)
}
func serveBytes(w http.ResponseWriter, r *http.Request, kind string, data []byte) {
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "segment", time.Time{}, bytes.NewReader(data))
}
func (s *Server) master(v *Session) ([]byte, error) {
	delivery := s.deliveryFor(v)
	v.mu.Lock()
	v.delivery = &delivery
	v.mu.Unlock()
	m := playlist.Multivariant{Version: 7}
	variant := playlist.MultivariantVariant{Bandwidth: 20000000}
	if v.variant != nil {
		variant = *v.variant
		variant.Codecs = append([]string{}, v.variant.Codecs...)
	}
	variant.URI = "video.m3u8"
	if delivery.VideoDirect {
		variant.URI = v.directURI(v.video)
	}
	variant.Audio = "audio"
	variant.Subtitles = ""
	variant.Video = ""
	variant.ClosedCaptions = ""
	defaultID := ""
	for _, t := range v.tracks {
		if !t.Subtitle && !t.Original && t.Lang == "it" {
			defaultID = t.ID
			break
		}
	}
	if defaultID == "" {
		for _, t := range v.tracks {
			if !t.Subtitle {
				defaultID = t.ID
				break
			}
		}
	}
	names := map[string]int{}
	for _, t := range v.tracks {
		uri := "track/" + t.ID + ".m3u8"
		if delivery.Tracks[t.ID] {
			uri = v.directURI(t.Asset)
		}
		uriPointer := &uri
		if t.Original && !t.Subtitle && t.Asset == v.video && v.video.HLS != nil {
			uriPointer = nil
		}
		typ := playlist.MultivariantRenditionTypeAudio
		group := "audio"
		if t.Subtitle {
			typ = playlist.MultivariantRenditionTypeSubtitles
			group = "subs"
			variant.Subtitles = "subs"
		}
		name := safeHLSName(t.Name)
		names[group+name]++
		if n := names[group+name]; n > 1 {
			name += fmt.Sprintf(" (%d)", n)
		}
		m.Renditions = append(m.Renditions, &playlist.MultivariantRendition{Type: typ, GroupID: group, Name: name, Language: t.Lang, Autoselect: true, Default: t.ID == defaultID, URI: uriPointer})
	}
	if defaultID == "" {
		variant.Audio = ""
	}
	if len(variant.Codecs) > 0 && defaultID != "" {
		found := false
		for _, c := range variant.Codecs {
			if c == "mp4a.40.2" {
				found = true
			}
		}
		if !found {
			variant.Codecs = append(variant.Codecs, "mp4a.40.2")
		}
	}
	m.Variants = []*playlist.MultivariantVariant{&variant}
	b, err := m.Marshal()
	// CODECS is optional in HLS. An empty value is misleading to ExoPlayer;
	// codec discovery is delegated to the init segment when unknown.
	return bytes.ReplaceAll(b, []byte(`,CODECS=""`), nil), err
}
func safeHLSName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}
func (s *Server) generatedPlaylist(v *Session, t *Track) string {
	var b strings.Builder
	target := 1
	for i := 0; i < len(v.boundaries)-1; i++ {
		target = max(target, int(math.Ceil(v.boundaries[i+1]-v.boundaries[i])))
	}
	sequence := 0
	if v.video.HLS != nil && len(v.video.HLS.Segments) > 0 {
		sequence = v.video.HLS.Segments[0].Sequence
	}
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-PLAYLIST-TYPE:VOD\n", target, sequence)
	if t == nil {
		// File tracks have one container configuration. Repeating an identical
		// init map with a different URI at every segment makes VLC reopen the
		// fMP4 demuxer and can reset its playback clock at each boundary.
		b.WriteString("#EXT-X-MAP:URI=\"video/0/init.mp4\"\n")
	}
	for i := 0; i < len(v.boundaries)-1; i++ {
		duration := v.boundaries[i+1] - v.boundaries[i]
		if v.video.HLS != nil && v.video.HLS.Segments[i].Discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if t == nil {
			fmt.Fprintf(&b, "#EXTINF:%.6f,\nvideo/%d/segment.m4s\n", duration, i)
		} else {
			ext := "ts"
			if t.Subtitle {
				ext = "vtt"
			}
			fmt.Fprintf(&b, "#EXTINF:%.6f,\n%s/%d.%s\n", duration, t.ID, i, ext)
		}
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}
func (s *Server) proxyPlaylist(v *Session, a *Asset, track bool) string {
	var b strings.Builder
	idx := 0
	rewrite := func(raw string) string {
		return s.baseResource(v, Origin{raw, a.Origin.Headers}, 0, false, false, false)
	}
	for _, line := range strings.Split(a.HLS.Raw, "\n") {
		l := strings.TrimSpace(line)
		if l != "" && !strings.HasPrefix(l, "#") {
			position := 0.0
			force := false
			if idx < len(a.HLS.Segments) {
				position = a.HLS.Segments[idx].Start
				force = a.HLS.Segments[idx].Range != ""
			}
			u, err := resolveURL(a.HLS.Origin.URL, l)
			if err != nil {
				continue
			}
			b.WriteString(s.baseResource(v, Origin{u, a.Origin.Headers}, position, track, force, a == v.video))
			idx++
		} else {
			b.WriteString(rewriteURI(line, a.HLS.Origin.URL, rewrite))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (s *Server) sectionClock(ctx context.Context, v *Session, n int) (float64, error) {
	if v.video.HLS == nil {
		return v.clockBase, nil
	}
	section := 0
	for i := n; i > 0; i-- {
		if v.video.HLS.Segments[i].Discontinuity {
			section = i
			break
		}
	}
	if section == 0 {
		return v.clockBase, nil
	}
	at := v.video.HLS.Segments[section].Start
	b, err := s.Engine.Cache.Get(ctx, fmt.Sprintf("clock:%s:%d", v.video.ID, section), func() ([]byte, error) {
		p, e := s.Engine.ProbeAt(ctx, v.video, at)
		if e != nil {
			return nil, e
		}
		base, e := strconv.ParseFloat(p.Format.StartTime, 64)
		if e != nil {
			return nil, errors.New("source discontinuity has no presentation clock")
		}
		return []byte(decimal(base - at)), nil
	})
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(string(b), 64)
}
func (s *Server) baseResource(v *Session, o Origin, at float64, track, force, video bool) string {
	return v.addResource(o, at, track, force, video)
}
func (s *Server) resource(w http.ResponseWriter, r *http.Request, v *Session, id string) {
	v.mu.Lock()
	res, ok := v.resources[id]
	v.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if res.Track && res.Video {
		v.position(res.Position)
	} else if res.Track {
		v.audioPosition(res.Position)
	}
	force := res.Force || s.Config.Get().PreferProxy || len(res.Origin.Headers) > 0
	if u, err := httpURL(res.Origin.URL); err == nil && s.Net.Client.Jar != nil && len(s.Net.Client.Jar.Cookies(u)) > 0 {
		force = true
	}
	if res.Video {
		v.videoRedirected.Store(!force)
	}
	if !force {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, res.Origin.URL, http.StatusTemporaryRedirect)
		return
	}
	if res.Video {
		defer beginVideoDownload(context.WithValue(r.Context(), videoBytesKey{}, &v.videoDownload))()
	}
	resp, err := s.Net.request(r.Context(), res.Origin, r.Method, r.Header.Get("Range"))
	if err != nil {
		v.note(err)
		failure(w, 502, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 416 {
		w.Header().Set("Content-Range", resp.Header.Get("Content-Range"))
		w.WriteHeader(416)
		return
	}
	if err = validateMediaResponse(resp, r.Header.Get("Range")); err != nil {
		v.note(err)
		failure(w, 502, err)
		return
	}
	if resp.ContentLength > segmentLimit {
		failure(w, 502, errors.New("media segment exceeds the 96 MiB limit"))
		return
	}
	copyMediaHeaders(w.Header(), resp.Header)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if r.Method == "HEAD" {
		return
	}
	body := io.Reader(resp.Body)
	if res.Video {
		body = io.TeeReader(body, videoByteWriter{context.WithValue(r.Context(), videoBytesKey{}, &v.videoDownload)})
	}
	n, _ := io.Copy(w, io.LimitReader(body, segmentLimit))
	s.Net.Bytes.Add(n)
}
