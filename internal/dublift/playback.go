package dublift

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
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
func (s *Server) awaitPreparedAlignment(ctx context.Context, v *Session) error {
	v.mu.Lock()
	done := v.alignDone
	running := v.Aligning
	v.mu.Unlock()
	if !running || done == nil {
		return nil
	}
	// Only the dashboard's Prepare action waits for alignment. Player requests
	// always return as soon as essential media preparation finishes.
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-v.ctx.Done():
		return v.ctx.Err()
	}
}
func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/media/"), "/")
	if len(p) < 2 {
		http.NotFound(w, r)
		return
	}
	v := s.getSession(p[0])
	// Diagnostics redirects resolve the original from the live session or its
	// authenticated resume ticket without probing or preparing any media.
	if s.redirectOriginal.Load() {
		original := ""
		if v != nil {
			original = v.stream.URL
		} else if len(p) == 2 && p[1] == "master.m3u8" {
			if ticket, err := s.readPlayback(p[0], r.URL.Query().Get("resume")); err == nil {
				original = ticket.Stream.URL
			}
		}
		if _, err := httpURL(original); err == nil {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, original, http.StatusTemporaryRedirect)
			return
		}
	}
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
		s.servePreparationError(w, r, v, p, err)
		return
	}
	if v.native != nil {
		if p[1] != "master.m3u8" {
			http.NotFound(w, r)
			return
		}
		b, err := s.nativePlaylist(v, v.nativeMaster, v.native.VariantURL, true, 0)
		if err != nil {
			v.note(err)
			failure(w, 502, err)
			return
		}
		sendPlaylist(w, b)
		return
	}
	switch {
	case p[1] == "master.m3u8":
		b, err := s.master(v)
		if err != nil {
			v.note(err)
			failure(w, 500, err)
			return
		}
		sendPlaylist(w, b)
		v.mu.Lock()
		first := !v.masterLoaded
		v.masterLoaded = true
		v.mu.Unlock()
		if first {
			s.ensurePlaybackAlignment(v)
		}
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
			if len(init) == 0 {
				ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
				defer cancel()
				ctx = context.WithValue(ctx, videoBytesKey{}, &v.videoDownload)
				var err error
				init, err = s.Engine.Cache.Get(ctx, "video-init:"+v.video.ID, func() ([]byte, error) {
					return s.Engine.VideoInit(ctx, v.video)
				})
				if err != nil {
					v.note(err)
					failure(w, 502, err)
					return
				}
				v.mu.Lock()
				if len(v.videoInit) == 0 {
					v.videoInit = init
				}
				init = v.videoInit
				v.mu.Unlock()
			}
			serveBytes(w, r, "video/mp4", init)
			return
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
		data = media
		v.videoSegmentCompleted(n)
		s.prefetchFileVideo(v, n+1)
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
			v.fileAudioSegmentRequested(n)
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
			if r.Method == http.MethodGet && r.Header.Get("Range") == "" {
				data = v.continuousAudioTS(track.ID, n, data)
			}
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
	return a.At(at, s.Config.Get().MinConfidence)
}
func serveBytes(w http.ResponseWriter, r *http.Request, kind string, data []byte) {
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "segment", time.Time{}, bytes.NewReader(data))
}
func (s *Server) master(v *Session) ([]byte, error) {
	m := playlist.Multivariant{Version: 7}
	variant := playlist.MultivariantVariant{Bandwidth: 20000000}
	if v.variant != nil {
		variant = *v.variant
		variant.Codecs = append([]string{}, v.variant.Codecs...)
	}
	variant.URI = "video.m3u8"
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
	// VLC 3 chooses the last audio rendition even when another is marked
	// DEFAULT. Keep the selected Italian rendition last for VLC while retaining
	// the HLS default flag for players that honor it.
	ordered := make([]Track, 0, len(v.tracks))
	for _, t := range v.tracks {
		if t.ID != defaultID {
			ordered = append(ordered, t)
		}
	}
	for _, t := range v.tracks {
		if t.ID == defaultID {
			ordered = append(ordered, t)
		}
	}
	names := map[string]int{}
	for _, t := range ordered {
		uri := "track/" + t.ID + ".m3u8"
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
		m.Renditions = append(m.Renditions, &playlist.MultivariantRendition{Type: typ, GroupID: group, Name: name, Language: t.Lang, Autoselect: t.Subtitle || t.ID == defaultID, Default: t.ID == defaultID, URI: uriPointer})
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
		return v.addResource(Origin{raw, a.Origin.Headers}, 0, false, false)
	}
	for _, line := range strings.Split(a.HLS.Raw, "\n") {
		l := strings.TrimSpace(line)
		if l != "" && !strings.HasPrefix(l, "#") {
			position := 0.0
			if idx < len(a.HLS.Segments) {
				position = a.HLS.Segments[idx].Start
			}
			u, err := resolveURL(a.HLS.Origin.URL, l)
			if err != nil {
				continue
			}
			b.WriteString(v.addResource(Origin{u, a.Origin.Headers}, position, track, a == v.video))
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
func (s *Server) resource(w http.ResponseWriter, r *http.Request, v *Session, id string) {
	v.mu.Lock()
	res, ok := v.resources[id]
	v.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if res.PlaylistDepth > 0 {
		if res.PlaylistDepth > 3 {
			failure(w, 502, errors.New("too many nested HLS playlists"))
			return
		}
		h, err := s.Net.LoadHLS(r.Context(), res.Origin)
		if err != nil {
			v.note(err)
			failure(w, 502, err)
			return
		}
		b, err := s.nativePlaylist(v, h, "", res.Video, res.PlaylistDepth)
		if err != nil {
			v.note(err)
			failure(w, 502, err)
			return
		}
		sendPlaylist(w, b)
		return
	}
	if res.Track && res.Video && r.Method == "GET" {
		v.position(res.Position)
		if v.video != nil && v.video.HLS != nil {
			v.videoSegmentRequested(sort.SearchFloat64s(v.boundaries, res.Position))
		}
	} else if res.Track {
		v.audioPosition(res.Position)
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
		err := errors.New("media segment exceeds the 96 MiB limit")
		v.note(err)
		failure(w, 502, err)
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
	n, err := io.Copy(w, io.LimitReader(body, segmentLimit))
	s.Net.Bytes.Add(n)
	if r.Context().Err() == nil {
		v.note(err)
	}
}
