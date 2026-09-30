package dublift

import (
	"bytes"
	"context"
	"crypto/cipher"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web/*
var dashboard embed.FS

type Server struct {
	Config           *Config
	Net              *Network
	Engine           *Engine
	videoPrefetch    chan struct{}
	Alignments       *AlignmentStore
	mu               sync.Mutex
	sessions         map[string]*Session
	events           []string
	ctx              context.Context
	cancel           context.CancelFunc
	generation       uint64
	lookupCancel     context.CancelFunc
	lookupStatus     string
	playbackKey      cipher.AEAD
	manifestRequests atomic.Uint64
}

func NewServer(c *Config) (*Server, error) {
	key, err := openPlaybackKey(c.path)
	if err != nil {
		return nil, err
	}
	n := NewNetwork()
	e, err := NewEngine(c, n)
	if err != nil {
		return nil, err
	}
	a, err := OpenAlignments(filepath.Join(filepath.Dir(c.path), "alignment.json"))
	if err != nil {
		e.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{Config: c, Net: n, Engine: e, Alignments: a, sessions: map[string]*Session{}, events: []string{}, ctx: ctx, cancel: cancel, playbackKey: key, videoPrefetch: make(chan struct{}, 1)}
	s.collectExpired(time.Now())
	go s.collectLoop()
	return s, nil
}
func (s *Server) Close() error { s.cancel(); return s.Engine.Close() }
func (s *Server) event(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, time.Now().Format("15:04:05")+" "+msg)
	if len(s.events) > 30 {
		s.events = s.events[len(s.events)-30:]
	}
}
func (s *Server) base(r *http.Request) string {
	if base := s.Config.Get().PublicURL; base != "" {
		return strings.TrimRight(base, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
func jsonResponse(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, code int, err error) {
	jsonResponse(w, code, map[string]string{"error": err.Error()})
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.api(w, r)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range, Content-Type")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Range, Accept-Ranges, Content-Length")
	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}
	switch {
	case r.URL.Path == "/manifest.json":
		s.manifestRequests.Add(1)
		jsonResponse(w, 200, map[string]any{"id": "local.dublift", "version": "0.1.0", "name": "DubLift", "description": "Your high-quality streams with synchronized Italian audio. Runs on your local network.", "resources": []string{"stream"}, "types": []string{"movie", "series"}, "catalogs": []any{}, "behaviorHints": map[string]any{"configurable": true, "configurationRequired": len(s.Config.Get().Addons) == 0}})
	case strings.HasPrefix(r.URL.Path, "/stream/"):
		s.streams(w, r)
	case strings.HasPrefix(r.URL.Path, "/media/"):
		s.media(w, r)
	case r.URL.Path == "/healthz":
		jsonResponse(w, 200, map[string]string{"status": "ok"})
	default:
		if r.URL.Path == "/" || r.URL.Path == "/configure" {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/index.html"
			b, _ := dashboard.ReadFile("web/index.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data: http: https:; frame-ancestors 'none'")
			w.Write(b)
			return
		}
		sub, _ := fs.Sub(dashboard, "web")
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
	}
}
func (s *Server) getSession(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	// Addon/media CORS is public. Private settings and mutation endpoints are
	// same-origin only; a random website cannot read addon credentials or POST.
	w.Header().Set("Cache-Control", "no-store")
	if origin := r.Header.Get("Origin"); origin != "" {
		u, e := url.Parse(origin)
		if e != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
			failure(w, 403, errors.New("dashboard API requires the same origin"))
			return
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		failure(w, 403, errors.New("cross-site dashboard request rejected"))
		return
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(405)
		return
	}
	if r.Method == "POST" && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		failure(w, 415, errors.New("application/json required"))
		return
	}
	switch r.URL.Path {
	case "/api/settings":
		if r.Method == "GET" {
			jsonResponse(w, 200, s.Config.Get())
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var cfg Settings
		if err := decodeRequest(w, r, &cfg); err != nil {
			failure(w, 400, err)
			return
		}
		if len(cfg.Addons) == 0 {
			failure(w, 400, errors.New("at least one upstream addon is required"))
			return
		}
		cfg.SetupCompleted = s.Config.Get().SetupCompleted
		if err := cfg.Validate(); err != nil {
			failure(w, 400, err)
			return
		}
		if err := s.populateAddonNames(r.Context(), &cfg); err != nil {
			failure(w, 400, err)
			return
		}
		if err := s.Config.Save(cfg); err != nil {
			failure(w, 400, err)
			return
		}
		s.Engine.Cache.Resize(int64(cfg.CacheMB) << 20)
		jsonResponse(w, 200, cfg)
	case "/api/addon-name":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var req struct {
			ManifestURL string `json:"manifestURL"`
		}
		if err := decodeRequest(w, r, &req); err != nil {
			failure(w, 400, err)
			return
		}
		details, err := s.addonDetails(r.Context(), req.ManifestURL)
		if err != nil {
			failure(w, 400, err)
			return
		}
		jsonResponse(w, 200, details)
	case "/api/setup-complete":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		cfg := s.Config.Get()
		cfg.SetupCompleted = true
		if err := s.Config.Save(cfg); err != nil {
			failure(w, 500, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"ok": true})
	case "/api/status":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		jsonResponse(w, 200, s.status(r))
	case "/api/events":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		s.statusEvents(w, r)
	case "/api/resolve":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var req struct{ Type, ID string }
		if err := decodeRequest(w, r, &req); err != nil {
			failure(w, 400, err)
			return
		}
		if _, err := ParseContent(req.Type, req.ID); err != nil {
			failure(w, 400, err)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/stream/" + req.Type + "/" + req.ID + ".json"
		s.streams(w, r2)
	default:
		s.sessionAction(w, r)
	}
}

func (s *Server) status(r *http.Request) map[string]any {
	s.mu.Lock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, v := range s.sessions {
		sessions = append(sessions, v)
	}
	events := append([]string{}, s.events...)
	s.mu.Unlock()
	snapshots := []map[string]any{}
	for _, v := range sessions {
		v.mu.Lock()
		view := map[string]any{"id": v.ID, "content": v.Content.ID, "contentName": v.ContentName, "name": v.stream.Name, "title": v.stream.Title, "description": v.stream.Description, "filename": v.stream.BehaviorHints.Filename, "sourceFormat": streamSourceFormat(v.stream, v.listedAsset), "order": v.Order, "playing": v.Playing, "preparationStarted": v.preparationStarted, "preparationDone": false, "playbackAt": v.PlaybackAt, "requestAt": v.RequestAt, "requestMethod": v.RequestMethod, "lastActivity": v.LastUsed, "passthrough": v.Passthrough, "sourceCheckDeferred": v.SourceCheckDeferred, "fallbackReason": v.FallbackReason, "sampleIndex": v.SampleIndex, "sampleTotal": v.SampleTotal, "samplePhase": v.SamplePhase, "startupZero": v.startupZero, "status": v.Status, "errors": append([]string{}, v.Errors...), "created": v.Created, "position": v.Position, "positionAt": v.PositionAt, "aligning": v.Aligning, "videoBytes": v.videoDownload.bytes.Load(), "videoActive": v.videoDownload.active.Load(), "videoRedirected": v.videoRedirected.Load(), "url": s.playbackURL(r, v), "originalUrl": v.stream.URL}
		if v.Passthrough {
			view["url"] = v.stream.URL
		}
		if v.listedReady != nil {
			select {
			case <-v.listedReady:
				view["checked"] = true
			default:
				view["checked"] = false
			}
		} else {
			view["checked"] = true
		}
		v.mu.Unlock()
		select {
		case <-v.ready:
			view["preparationDone"] = true
			view["sourceFormat"] = streamSourceFormat(v.stream, v.video)
			view["duration"] = v.Duration
			view["proxyReason"] = v.ProxyReason
			view["alignment"] = s.Alignments.Get(v.Key)
			view["ready"] = v.prepareErr == nil
			tracks := []map[string]string{}
			for _, t := range v.tracks {
				tracks = append(tracks, map[string]string{"name": t.Name, "language": t.Lang, "id": t.ID})
			}
			view["tracks"] = tracks
			if v.prepareErr == nil {
				view["directPlayback"] = s.deliveryFor(v)
				v.mu.Lock()
				view["loadedDelivery"] = v.delivery
				v.mu.Unlock()
			}
		default:
		}
		snapshots = append(snapshots, view)
	}
	sort.SliceStable(snapshots, func(i, j int) bool {
		a, b := snapshots[i], snapshots[j]
		if a["playing"].(bool) != b["playing"].(bool) {
			return a["playing"].(bool)
		}
		return a["order"].(int) < b["order"].(int)
	})
	s.mu.Lock()
	lookupStatus := s.lookupStatus
	s.mu.Unlock()
	cfg := s.Config.Get()
	return map[string]any{"sessions": snapshots, "lookupStatus": lookupStatus, "events": events, "cacheBytes": s.Engine.Cache.Used(), "cacheMaxBytes": int64(cfg.CacheMB) << 20, "originBytes": s.Net.Bytes.Load(), "manifestURL": s.base(r) + "/manifest.json", "manifestRequests": s.manifestRequests.Load()}
}

func streamSourceFormat(stream Stream, asset *Asset) string {
	if asset != nil {
		if asset.HLS != nil {
			return "hls"
		}
		if asset.Index.Container == "matroska" {
			return "mkv"
		}
		if asset.Index.Container == "mp4" {
			return "mp4"
		}
		return "other"
	}
	path := ""
	if u, err := url.Parse(stream.URL); err == nil {
		path = strings.ToLower(u.Path)
	}
	filename := strings.ToLower(stream.BehaviorHints.Filename)
	switch {
	case strings.HasSuffix(path, ".m3u8"):
		return "hls"
	case strings.HasSuffix(path, ".mkv"):
		return "mkv"
	case strings.HasSuffix(path, ".mp4"), strings.HasSuffix(path, ".m4v"):
		return "mp4"
	case strings.HasSuffix(filename, ".m3u8"):
		return "hls"
	case strings.HasSuffix(filename, ".mkv"):
		return "mkv"
	case strings.HasSuffix(filename, ".mp4"), strings.HasSuffix(filename, ".m4v"):
		return "mp4"
	default:
		return "other"
	}
}

func (s *Server) statusEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		failure(w, 500, errors.New("streaming unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	var previous []byte
	quietTicks := 0
	updates := time.NewTicker(time.Second)
	defer updates.Stop()
	for {
		state, err := json.Marshal(s.status(r))
		if err != nil {
			return
		}
		if !bytes.Equal(state, previous) {
			if _, err := w.Write(append(append([]byte("data: "), state...), '\n', '\n')); err != nil {
				return
			}
			flusher.Flush()
			previous = state
			quietTicks = 0
		} else {
			quietTicks++
			if quietTicks >= 15 {
				if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
					return
				}
				flusher.Flush()
				quietTicks = 0
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-updates.C:
		}
	}
}

type addonDetails struct {
	Name string `json:"name"`
	Icon string `json:"icon"`
}

func (s *Server) addonDetails(ctx context.Context, manifestURL string) (addonDetails, error) {
	if _, err := httpURL(manifestURL); err != nil {
		return addonDetails{}, err
	}
	if !strings.HasSuffix(strings.Split(manifestURL, "?")[0], "/manifest.json") {
		return addonDetails{}, errors.New("upstream URL must end with /manifest.json")
	}
	work, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	b, finalURL, err := s.Net.Fetch(work, Origin{URL: manifestURL}, manifestLimit)
	if err != nil {
		return addonDetails{}, err
	}
	var manifest struct {
		Name string `json:"name"`
		Logo string `json:"logo"`
		Icon string `json:"icon"`
	}
	if json.Unmarshal(b, &manifest) != nil || strings.TrimSpace(manifest.Name) == "" {
		return addonDetails{}, errors.New("upstream manifest has no addon name")
	}
	details := addonDetails{Name: strings.TrimSpace(manifest.Name)}
	iconValue := strings.TrimSpace(manifest.Logo)
	if iconValue == "" {
		iconValue = strings.TrimSpace(manifest.Icon)
	}
	if strings.HasPrefix(iconValue, "data:image/") && len(iconValue) <= 128<<10 {
		details.Icon = iconValue
	} else if icon, err := url.Parse(iconValue); err == nil && icon.String() != "" {
		base, _ := url.Parse(finalURL)
		resolved := base.ResolveReference(icon)
		if _, err := httpURL(resolved.String()); err == nil {
			details.Icon = resolved.String()
		}
	}
	return details, nil
}

func (s *Server) populateAddonNames(ctx context.Context, cfg *Settings) error {
	previous := s.Config.Get().Addons
	for i := range cfg.Addons {
		found := false
		for _, old := range previous {
			if cfg.Addons[i].ManifestURL == old.ManifestURL && old.Name != "" {
				cfg.Addons[i].Name = old.Name
				found = true
				break
			}
		}
		if found {
			continue
		}
		details, err := s.addonDetails(ctx, cfg.Addons[i].ManifestURL)
		if err != nil {
			return err
		}
		cfg.Addons[i].Name = details.Name
	}
	return nil
}
func decodeRequest(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid JSON request")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return nil
}
func (s *Server) sessionAction(w http.ResponseWriter, r *http.Request) {
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(p) != 4 || p[0] != "api" || p[1] != "sessions" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	v := s.getSession(p[2])
	if v == nil {
		http.NotFound(w, r)
		return
	}
	if p[3] == "close" {
		s.mu.Lock()
		s.discardLocked(v)
		s.mu.Unlock()
		jsonResponse(w, 200, map[string]bool{"ok": true})
		return
	}
	if err := s.prepareSession(r.Context(), v); err != nil {
		failure(w, 502, err)
		return
	}
	switch p[3] {
	case "prepare":
		jsonResponse(w, 200, map[string]bool{"ok": true})
	case "offset":
		var req struct {
			Offset *float64 `json:"offset"`
		}
		if err := decodeRequest(w, r, &req); err != nil {
			failure(w, 400, err)
			return
		}
		if req.Offset != nil && (math.IsNaN(*req.Offset) || math.IsInf(*req.Offset, 0) || math.Abs(*req.Offset) > 3600) {
			failure(w, 400, errors.New("offset must be within ±3600 seconds"))
			return
		}
		err := s.Alignments.UpdateContext(v.ctx, v.Key, func(a *Alignment) { a.Manual = req.Offset })
		if err != nil {
			failure(w, 500, err)
			return
		}
		v.mu.Lock()
		v.Revision++
		v.mu.Unlock()
		jsonResponse(w, 200, map[string]bool{"ok": true})
	case "realign":
		var req struct {
			Position *float64 `json:"position"`
		}
		if err := decodeRequest(w, r, &req); err != nil {
			failure(w, 400, err)
			return
		}
		v.mu.Lock()
		at := v.Position
		known := !v.PositionAt.IsZero()
		v.mu.Unlock()
		if req.Position != nil {
			if math.IsNaN(*req.Position) || math.IsInf(*req.Position, 0) || *req.Position < 0 || *req.Position >= v.Duration {
				failure(w, 400, errors.New("position must fall within the video runtime"))
				return
			}
			at = *req.Position
			known = true
		}
		if !known {
			failure(w, 409, errors.New("direct playback does not report its position; enter the player's current position, or play a local media segment first"))
			return
		}
		if !s.startAlignment(v, &at) {
			failure(w, 409, errors.New("alignment is already running or an English reference is unavailable"))
			return
		}
		jsonResponse(w, 202, map[string]any{"position": at, "ok": true})
	case "analyze":
		if !s.startAlignment(v, nil) {
			failure(w, 409, errors.New("alignment is already running or an English reference is unavailable"))
			return
		}
		jsonResponse(w, 202, map[string]bool{"ok": true})
	case "reset":
		err := s.Alignments.UpdateContext(v.ctx, v.Key, func(a *Alignment) { *a = Alignment{} })
		if err != nil {
			failure(w, 500, err)
			return
		}
		s.startAlignment(v, nil)
		jsonResponse(w, 200, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}
