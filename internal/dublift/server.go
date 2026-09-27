package dublift

import (
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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var dashboard embed.FS

type Server struct {
	Config       *Config
	Net          *Network
	Engine       *Engine
	Alignments   *AlignmentStore
	mu           sync.Mutex
	sessions     map[string]*Session
	events       []string
	ctx          context.Context
	cancel       context.CancelFunc
	generation   uint64
	lookupCancel context.CancelFunc
	lookupStatus string
	playbackKey  cipher.AEAD
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
	s := &Server{Config: c, Net: n, Engine: e, Alignments: a, sessions: map[string]*Session{}, events: []string{}, ctx: ctx, cancel: cancel, playbackKey: key}
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
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
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
		if err := s.Config.Save(cfg); err != nil {
			failure(w, 400, err)
			return
		}
		s.Engine.Cache.Resize(int64(cfg.CacheMB) << 20)
		jsonResponse(w, 200, map[string]any{"ok": true, "note": "Listen address changes apply after restarting. Source changes apply to new sessions."})
	case "/api/status":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
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
			view := map[string]any{"id": v.ID, "content": v.Content.ID, "contentName": v.ContentName, "name": v.stream.Name, "title": v.stream.Title, "description": v.stream.Description, "filename": v.stream.BehaviorHints.Filename, "order": v.Order, "playing": v.Playing, "playbackAt": v.PlaybackAt, "requestAt": v.RequestAt, "requestMethod": v.RequestMethod, "lastActivity": v.LastUsed, "passthrough": v.Passthrough, "fallbackReason": v.FallbackReason, "sampleIndex": v.SampleIndex, "sampleTotal": v.SampleTotal, "samplePhase": v.SamplePhase, "startupZero": v.startupZero, "status": v.Status, "errors": append([]string{}, v.Errors...), "created": v.Created, "position": v.Position, "positionAt": v.PositionAt, "aligning": v.Aligning, "url": s.playbackURL(r, v)}
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
		ffmpegOK := false
		if _, err := exec.LookPath(s.Config.Get().FFmpeg); err == nil {
			ffmpegOK = true
		}
		ffprobeOK := false
		if _, err := exec.LookPath(s.Config.Get().FFprobe); err == nil {
			ffprobeOK = true
		}
		s.mu.Lock()
		lookupStatus := s.lookupStatus
		s.mu.Unlock()
		jsonResponse(w, 200, map[string]any{"sessions": snapshots, "lookupStatus": lookupStatus, "events": events, "cacheBytes": s.Engine.Cache.Used(), "originBytes": s.Net.Bytes.Load(), "ffmpeg": ffmpegOK, "ffprobe": ffprobeOK, "manifestURL": s.base(r) + "/manifest.json"})
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
