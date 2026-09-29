package dublift

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
)

type Asset struct {
	ID     string
	Origin Origin
	HLS    *HLS
	File   *RemoteFile
	Index  FileIndex
}

func (a *Asset) Duration() float64 {
	if a.HLS != nil {
		return a.HLS.Duration
	}
	return a.Index.Duration
}

type Track struct {
	ID       string
	Name     string
	Lang     string
	Asset    *Asset
	Selector string
	Original bool
	Subtitle bool
}
type ProbeStream struct {
	Index     int    `json:"index"`
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	StartTime string `json:"start_time"`
	Tags      struct {
		Language string `json:"language"`
		Title    string `json:"title"`
	} `json:"tags"`
}
type Probe struct {
	Streams []ProbeStream `json:"streams"`
	Format  struct {
		Duration  string `json:"duration"`
		StartTime string `json:"start_time"`
	} `json:"format"`
}
type Engine struct {
	Config          *Config
	Net             *Network
	Cache           *ByteCache
	mu              sync.Mutex
	jobs            map[string]*mediaJob
	base            string
	srv             *http.Server
	slots           chan struct{}
	backgroundSlots chan struct{}
	foregroundFiles map[string]*fileActivity
}
type fileActivity struct {
	count int
	idle  chan struct{}
}

func (e *Engine) foregroundFile(a *Asset) func() {
	if a.File == nil {
		return func() {}
	}
	e.mu.Lock()
	gate := e.foregroundFiles[a.ID]
	if gate == nil {
		gate = &fileActivity{idle: make(chan struct{})}
		e.foregroundFiles[a.ID] = gate
	}
	gate.count++
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		gate.count--
		if gate.count == 0 {
			delete(e.foregroundFiles, a.ID)
			close(gate.idle)
		}
	}
}
func (e *Engine) waitForFileRead(ctx context.Context, id string) error {
	if background, _ := ctx.Value(backgroundWorkKey{}).(bool); !background {
		return nil
	}
	for {
		e.mu.Lock()
		gate := e.foregroundFiles[id]
		e.mu.Unlock()
		if gate == nil {
			return ctx.Err()
		}
		select {
		case <-gate.idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type backgroundWorkKey struct{}
type cacheOnlyFileKey struct{}
type cacheMissKey struct{}
type mediaJob struct {
	asset     *Asset
	playlist  string
	resources map[string]Origin
	keys      map[string]bool
	budget    atomic.Int64
	ctx       context.Context
}

func NewEngine(c *Config, n *Network) (*Engine, error) {
	e := &Engine{Config: c, Net: n, Cache: NewByteCache(int64(c.Get().CacheMB) << 20), jobs: map[string]*mediaJob{}, slots: make(chan struct{}, 3), backgroundSlots: make(chan struct{}, 1), foregroundFiles: map[string]*fileActivity{}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	e.base = "http://" + l.Addr().String()
	e.srv = &http.Server{Handler: http.HandlerFunc(e.serveJob), ReadHeaderTimeout: 5 * time.Second}
	go e.srv.Serve(l)
	return e, nil
}
func (e *Engine) Close() error { return e.srv.Close() }
func (e *Engine) OpenAsset(ctx context.Context, o Origin, hls bool) (*Asset, error) {
	a := &Asset{ID: identity(o.URL, fmt.Sprint(o.Headers)), Origin: o}
	if !hls {
		// Extensionless redirect links are common. Sniff at most 512 bytes; this
		// never turns an ignored range into a file download.
		resp, err := e.Net.request(ctx, o, "GET", "bytes=0-511")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 && resp.StatusCode != 206 {
			resp.Body.Close()
			return nil, &HTTPError{resp.StatusCode}
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		hls = strings.HasPrefix(strings.TrimSpace(string(b)), "#EXTM3U")
		if hls {
			o.URL = resp.Request.URL.String()
			a.Origin = o
		}
	}
	if hls {
		h, err := e.Net.LoadHLS(ctx, o)
		if err != nil {
			return nil, err
		}
		a.HLS = h
		for _, segment := range h.Segments {
			if segment.Duration > 300 {
				return nil, errors.New("media segments longer than five minutes are unsupported; refusing an unbounded video read")
			}
		}
		return a, nil
	}
	f, err := e.Net.OpenFile(ctx, o)
	if err != nil {
		return nil, err
	}
	a.File = f
	a.Index, err = BuildFileIndex(ctx, f)
	return a, err
}
func (e *Engine) job(ctx context.Context, a *Asset, start, duration float64) (string, float64, func(), error) {
	j := &mediaJob{asset: a, resources: map[string]Origin{}, keys: map[string]bool{}, ctx: ctx}
	j.budget.Store(256 << 20)
	id := token()
	root := e.base + "/" + id
	skip := start
	if a.HLS != nil {
		keyURLs := map[string]bool{}
		for _, seg := range a.HLS.Segments {
			if m := uriRE.FindStringSubmatch(seg.Key); m != nil {
				if raw, err := resolveURL(a.HLS.Origin.URL, m[1]); err == nil {
					keyURLs[raw] = true
				}
			}
		}
		var windowStart float64
		var err error
		j.playlist, windowStart, err = a.HLS.Window(start, duration, func(raw string) string {
			k := identity(raw)[:24]
			j.resources[k] = Origin{raw, a.Origin.Headers}
			j.keys[k] = keyURLs[raw]
			return root + "/r/" + k
		})
		if err != nil {
			return "", 0, nil, err
		}
		skip = start - windowStart
	}
	e.mu.Lock()
	e.jobs[id] = j
	e.mu.Unlock()
	cleanup := func() { e.mu.Lock(); delete(e.jobs, id); e.mu.Unlock() }
	ext := "/input"
	if a.HLS != nil {
		ext += ".m3u8"
	}
	return root + ext, skip, cleanup, nil
}
func (e *Engine) serveJob(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	e.mu.Lock()
	j := e.jobs[parts[0]]
	e.mu.Unlock()
	if j == nil {
		http.NotFound(w, r)
		return
	}
	if j.ctx.Err() != nil {
		http.Error(w, "expired extraction", 410)
		return
	}
	if parts[1] == "input.m3u8" {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, j.playlist)
		return
	}
	if parts[1] == "input" && j.asset.File != nil {
		e.serveFileJob(w, r, j)
		return
	}
	if len(parts) != 3 || parts[1] != "r" {
		http.NotFound(w, r)
		return
	}
	o, ok := j.resources[parts[2]]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if j.keys[parts[2]] {
		// libavformat requests bytes=0- even for a 16-byte AES key. Key
		// endpoints may legitimately ignore that range; fetch the bounded key
		// and provide correct local Range semantics ourselves.
		b, _, err := e.Net.Fetch(r.Context(), o, 16)
		if err != nil || len(b) != 16 {
			http.Error(w, "unable to read the 16-byte HLS key", 502)
			return
		}
		j.budget.Add(-int64(len(b)))
		serveBytes(w, r, "application/octet-stream", b)
		return
	}
	resp, err := e.Net.request(r.Context(), o, "GET", r.Header.Get("Range"))
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	if err = validateMediaResponse(resp, r.Header.Get("Range")); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	limit := int64(segmentLimit)
	if resp.ContentLength > limit {
		http.Error(w, "segment exceeds byte budget", 502)
		return
	}
	copyMediaHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	e.copyBudget(w, resp.Body, j, limit)
}
func (e *Engine) copyBudget(w io.Writer, r io.Reader, j *mediaJob, limit int64) {
	buf := make([]byte, 64<<10)
	for limit > 0 && j.ctx.Err() == nil {
		n, err := r.Read(buf[:min(int64(len(buf)), limit)])
		if n > 0 {
			if j.budget.Add(-int64(n)) < 0 {
				return
			}
			e.Net.Bytes.Add(int64(n))
			if _, we := w.Write(buf[:n]); we != nil {
				return
			}
			limit -= int64(n)
		}
		if err != nil {
			return
		}
	}
}
func (e *Engine) serveFileJob(w http.ResponseWriter, r *http.Request, j *mediaJob) {
	f := j.asset.File
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	if r.Method == "HEAD" {
		w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
		return
	}
	a, b, err := requestRange(r.Header.Get("Range"), f.Size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", f.Size))
		http.Error(w, "ranged input required", 416)
		return
	}
	// Every actual origin read is a separately validated finite byte range.
	// Returning the virtual length lets libavformat seek to indexes at EOF.
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, f.Size))
	w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
	w.WriteHeader(206)
	for off := a; off <= b; {
		if r.Context().Err() != nil || j.ctx.Err() != nil {
			return
		}
		size := min(int64(1<<20), b-off+1)
		if j.budget.Add(-size) < 0 {
			return
		}
		miss, cacheOnly := j.ctx.Value(cacheMissKey{}).(*atomic.Bool)
		// Cached analysis does not compete for origin bandwidth. Other
		// background file work yields to playback before making a range read.
		if !cacheOnly {
			if err := e.waitForFileRead(j.ctx, j.asset.ID); err != nil {
				return
			}
		}
		// FFmpeg repeatedly asks for the container header and cue table when
		// opening each short extraction. Share only requested finite ranges in
		// the bounded session cache; never read ahead or keep a whole file.
		var fetch func(int64, int64) ([]byte, error)
		if !cacheOnly {
			fetch = func(start, length int64) ([]byte, error) {
				data := make([]byte, length)
				n, err := f.ReadAtContext(j.ctx, data, start)
				return data[:n], err
			}
		}
		buf, err := e.Cache.ReadRange(j.ctx, j.asset.ID, off, size, fetch)
		if errors.Is(err, errRangeNotCached) {
			miss.Store(true)
		}
		if len(buf) > 0 {
			if _, we := w.Write(buf); we != nil {
				return
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			off += int64(len(buf))
		}
		if err != nil {
			return
		}
	}
}
func validateMediaResponse(resp *http.Response, requested string) error {
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return &HTTPError{resp.StatusCode}
	}
	if requested != "" {
		if resp.StatusCode != 206 {
			return errors.New("origin ignored media Range")
		}
		a, b, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return err
		}
		wantA, wantB, err := requestRange(requested, total)
		if err != nil || a != wantA || b != wantB {
			return errors.New("inconsistent media Content-Range")
		}
		if resp.ContentLength >= 0 && resp.ContentLength != b-a+1 {
			return errors.New("invalid media range length")
		}
	}
	return nil
}
func copyMediaHeaders(dst, src http.Header) {
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("FFmpeg output exceeds byte budget")
	}
	return b.Buffer.Write(p)
}
func (e *Engine) run(ctx context.Context, binary string, args []string, limit int, packetInfo ...io.Writer) ([]byte, error) {
	if background, _ := ctx.Value(backgroundWorkKey{}).(bool); background {
		select {
		case e.backgroundSlots <- struct{}{}:
			defer func() { <-e.backgroundSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 2 * time.Second
	out := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	var sideReader, sideWriter *os.File
	var sideDone chan error
	if len(packetInfo) != 0 {
		var err error
		sideReader, sideWriter, err = os.Pipe()
		if err != nil {
			return nil, err
		}
		defer sideReader.Close()
		cmd.ExtraFiles = []*os.File{sideWriter}
		sideDone = make(chan error, 1)
		go func() { _, err := io.Copy(packetInfo[0], sideReader); sideDone <- err }()
	}
	err := cmd.Start()
	if sideWriter != nil {
		sideWriter.Close()
	}
	if err == nil {
		err = cmd.Wait()
	}
	if sideDone != nil {
		if sideErr := <-sideDone; err == nil {
			err = sideErr
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s failed (%s)", programName(binary), safeFFmpegError(stderr.String()))
	}
	return out.Bytes(), nil
}
func programName(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}
func safeFFmpegError(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	redact := regexp.MustCompile(`(?:https?|tcp|crypto)://[^\s'\"]+`)
	if len(lines) > 4 {
		lines = lines[:4]
	}
	message := redact.ReplaceAllString(strings.Join(lines, "; "), "[media]")
	if len(message) > 1000 {
		message = message[:1000]
	}
	if message == "" {
		message = "media could not be decoded or read within the byte budget"
	}
	return message
}
func ffInput(url string) []string {
	args := []string{"-protocol_whitelist", "http,tcp,crypto", "-probesize", "4000000", "-analyzeduration", "4000000"}
	if strings.HasSuffix(url, ".m3u8") {
		args = append(args, hlsInputOptions()...)
	}
	return append(args, "-i", url)
}

func hlsInputOptions() []string {
	args := []string{"-allowed_extensions", "ALL"}
	if runtime.GOOS == "android" {
		// The bundled FFmpeg 7.1.2 checks segment extensions separately.
		// Our bounded HLS jobs serve approved resources at extensionless URLs.
		// FFmpeg 6.1 on desktop does not recognize this option.
		args = append(args, "-extension_picky", "0")
	}
	return args
}
func (e *Engine) Probe(ctx context.Context, a *Asset) (Probe, error) {
	return e.ProbeAt(ctx, a, 0)
}
func (e *Engine) ProbeAt(ctx context.Context, a *Asset, at float64) (Probe, error) {
	var p Probe
	u, _, cleanup, err := e.job(ctx, a, at, min(8, a.Duration()-at))
	if err != nil {
		return p, err
	}
	defer cleanup()
	args := []string{"-v", "error", "-protocol_whitelist", "http,tcp,crypto", "-probesize", "4000000", "-analyzeduration", "4000000", "-show_streams", "-show_format", "-of", "json"}
	if a.HLS != nil {
		args = append(args, hlsInputOptions()...)
	} else {
		// Indexed containers carry codec/track metadata in their headers.
		// Avoid scanning seconds of high-bitrate video merely to enumerate tracks.
		args = append(args, "-nofind_stream_info")
	}
	args = append(args, u)
	b, err := e.run(ctx, e.Config.Get().FFprobe, args, 1<<20)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(b, &p)
	return p, err
}
func (e *Engine) PCM(ctx context.Context, t Track, start, duration float64) ([]int16, error) {
	return e.pcm(ctx, t, start, duration, false)
}
func (e *Engine) PCMFromZero(ctx context.Context, t Track, start, duration float64) ([]int16, error) {
	return e.pcm(ctx, t, start, duration, true)
}
func (e *Engine) pcm(ctx context.Context, t Track, start, duration float64, fromZero bool) ([]int16, error) {
	if duration <= 0 || duration > 400 {
		return nil, errors.New("invalid analysis window")
	}
	key := fmt.Sprintf("pcm:%s:%s:%.3f:%.3f:%t", t.Asset.ID, t.Selector, start, duration, fromZero)
	b, err := e.Cache.Get(ctx, key, func() ([]byte, error) {
		fileCtx := ctx
		var miss *atomic.Bool
		if t.Asset.File != nil && ctx.Value(cacheOnlyFileKey{}) == true {
			miss = &atomic.Bool{}
			fileCtx = context.WithValue(ctx, cacheMissKey{}, miss)
		}
		inputStart, inputDuration := start, duration+1
		if fromZero {
			inputStart, inputDuration = 0, start+duration+1
		}
		u, skip, cleanup, err := e.job(fileCtx, t.Asset, inputStart, inputDuration)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		args := []string{"-nostdin", "-v", "error", "-threads", "1"}
		if t.Asset.File != nil && !fromZero {
			args = append(args, "-ss", decimal(skip))
		}
		args = append(args, "-discard:v", "all")
		args = append(args, ffInput(u)...)
		if t.Asset.HLS != nil || fromZero {
			seek := skip
			if fromZero {
				seek = start
			}
			args = append(args, "-ss", decimal(seek))
		}
		args = append(args, "-t", decimal(duration), "-map", t.Selector, "-vn", "-sn", "-ac", "1", "-ar", strconv.Itoa(pcmRate), "-f", "s16le", "pipe:1")
		out, err := e.run(fileCtx, e.Config.Get().FFmpeg, args, int(math.Ceil(duration*pcmRate*2))+65536)
		if miss != nil && miss.Load() {
			return nil, errRangeNotCached
		}
		return out, err
	})
	if err != nil {
		return nil, err
	}
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out, nil
}
func decimal(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

// Audio segments share the source presentation clock. Silence pads Vixsrc
// gaps at either end; only audio is encoded, always at normal speed.
func (e *Engine) Audio(ctx context.Context, t Track, start, duration, offset, clockBase float64) ([]byte, error) {
	finish := e.foregroundFile(t.Asset)
	defer finish()
	vixStart := start - offset
	leading := max(0, -vixStart)
	inputStart := max(0, vixStart)
	available := max(0, min(duration-leading, t.Asset.Duration()-inputStart))
	args := []string{"-nostdin", "-v", "error", "-threads", "1"}
	cleanup := func() {}
	if available > .001 {
		u, skip, done, err := e.job(ctx, t.Asset, inputStart, available+1)
		if err != nil {
			return nil, err
		}
		cleanup = done
		trimStart := 0.0
		if t.Asset.File != nil {
			args = append(args, "-ss", decimal(skip))
		} else {
			trimStart = skip
		}
		args = append(args, "-discard:v", "all")
		args = append(args, ffInput(u)...)
		args = append(args, "-map", t.Selector, "-af", fmt.Sprintf("asetpts=PTS-STARTPTS,atrim=start=%s:duration=%s,asetpts=PTS-STARTPTS,adelay=%d:all=1,apad", decimal(trimStart), decimal(available), int(math.Round(leading*1000))))
	} else {
		args = append(args, "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo")
	}
	defer cleanup()
	args = append(args, "-t", decimal(duration), "-vn", "-sn", "-c:a", "aac", "-b:a", "192k", "-ac", "2", "-ar", "48000", "-output_ts_offset", decimal(start+clockBase), "-mpegts_copyts", "1", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts", "pipe:1")
	return e.run(ctx, e.Config.Get().FFmpeg, args, 4<<20)
}
func (e *Engine) Video(ctx context.Context, a *Asset, start, duration float64) ([]byte, error) {
	finish := e.foregroundFile(a)
	defer finish()
	u, skip, cleanup, err := e.job(ctx, a, start, duration+1)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// Matroska cue times are rounded to the container's tick. Seek just after
	// the cue so libavformat cannot round down to the previous GOP.
	args := []string{"-nostdin", "-v", "error", "-threads", "1", "-copyts", "-ss", decimal(skip + .01), "-itsoffset", decimal(-start), "-discard:a", "all", "-discard:s", "all", "-discard:d", "all"}
	args = append(args, ffInput(u)...)
	// -dn does not disable the MP4 muxer's synthesized chapter text track.
	// Explicitly exclude chapters so each fragment has exactly the video track.
	// The MP4 muxer rebases timestamps. The framecrc side output records the
	// first compressed packet's real PTS before rebasing; it is not decoded or
	// re-encoded. This distinguishes actual preroll from the requested GOP.
	args = append(args, "-t", decimal(duration+.25), "-map", "0:v:0", "-map_chapters", "-1", "-an", "-sn", "-dn", "-c:v", "copy", "-avoid_negative_ts", "disabled", "-f", "tee", "[f=mp4:movflags=+frag_keyframe+empty_moov+default_base_moof:avoid_negative_ts=disabled]pipe:1|[f=framecrc]pipe:3")
	clock := &packetClock{}
	raw, err := e.run(ctx, e.Config.Get().FFmpeg, args, segmentLimit, clock)
	if err != nil {
		return nil, err
	}
	if !clock.known {
		return nil, errors.New("remux produced no video packet timestamps")
	}
	return placeFragments(raw, start+clock.first+1, start+1, start+duration+1)
}

// FFmpeg's standalone MP4 muxer resets each extraction's decode clock.
// Rebase fragment metadata, keep only complete GOPs before the next indexed
// boundary, and preserve every compressed video sample byte-for-byte.
func placeFragments(raw []byte, first, start, end float64) ([]byte, error) {
	init, media, err := splitFMP4(raw)
	if err != nil {
		return nil, err
	}
	mdhd := mp4Child(init, "moov", "trak", "mdia", "mdhd")
	if len(mdhd) < 24 {
		return nil, errors.New("fragment init lacks video timescale")
	}
	scale := binary.BigEndian.Uint32(mdhd[12:])
	if mdhd[0] == 1 {
		if len(mdhd) < 36 {
			return nil, errors.New("invalid fragment timescale")
		}
		scale = binary.BigEndian.Uint32(mdhd[20:])
	}
	if scale == 0 {
		return nil, errors.New("zero fragment timescale")
	}
	var parts fmp4.Parts
	if err = parts.Unmarshal(media); err != nil {
		return nil, err
	}
	if err = restoreFirstSampleFlags(parts, media); err != nil {
		return nil, err
	}
	if len(parts) == 0 || len(parts[0].Tracks) != 1 || len(parts[0].Tracks[0].Samples) == 0 {
		return nil, errors.New("empty video fragment")
	}
	t := parts[0].Tracks[0]
	delta := int64(math.Round(first*float64(scale))) - int64(t.BaseTime) - int64(t.Samples[0].PTSOffset)
	var out seekablebuffer.Buffer
	out.Write(init)
	kept := 0
	for _, p := range parts {
		if len(p.Tracks) != 1 || len(p.Tracks[0].Samples) == 0 {
			return nil, errors.New("unexpected remux tracks")
		}
		track := p.Tracks[0]
		base := int64(track.BaseTime) + delta
		pts := float64(base+int64(track.Samples[0].PTSOffset)) / float64(scale)
		if pts < start-.025 {
			continue
		}
		if pts >= end-.025 {
			break
		}
		if base < 0 {
			return nil, errors.New("negative video decode clock")
		}
		track.BaseTime = uint64(base)
		p.SequenceNumber = uint32(kept + 1)
		if err = p.Marshal(&out); err != nil {
			return nil, err
		}
		kept++
	}
	if kept == 0 {
		return nil, errors.New("no video samples at requested seek position")
	}
	return out.Bytes(), nil
}

// mediacommon's reader applies tfhd defaults/per-sample flags but does not
// apply trun.first_sample_flags. FFmpeg uses that override for HEVC keyframes;
// losing it marks every frame non-sync and can prevent players from seeking.
func restoreFirstSampleFlags(parts fmp4.Parts, media []byte) error {
	boxes, err := mp4Boxes(media)
	if err != nil {
		return err
	}
	partIndex := 0
	for _, moof := range boxes {
		if moof.kind != "moof" {
			continue
		}
		if partIndex >= len(parts) {
			return errors.New("fragment metadata mismatch")
		}
		p := parts[partIndex]
		partIndex++
		children, err := mp4Boxes(moof.data)
		if err != nil {
			return err
		}
		for _, traf := range children {
			if traf.kind != "traf" {
				continue
			}
			fields, err := mp4Boxes(traf.data)
			if err != nil {
				return err
			}
			trackID := 0
			for _, field := range fields {
				if field.kind == "tfhd" && len(field.data) >= 8 {
					trackID = int(binary.BigEndian.Uint32(field.data[4:]))
				}
			}
			var track *fmp4.PartTrack
			for _, candidate := range p.Tracks {
				if candidate.ID == trackID {
					track = candidate
					break
				}
			}
			if track == nil {
				return errors.New("fragment track metadata mismatch")
			}
			sample := 0
			for _, field := range fields {
				if field.kind != "trun" {
					continue
				}
				if len(field.data) < 8 {
					return errors.New("truncated fragment run")
				}
				flags := binary.BigEndian.Uint32(field.data[:4]) & 0xffffff
				count := int(binary.BigEndian.Uint32(field.data[4:8]))
				if sample+count > len(track.Samples) {
					return errors.New("invalid fragment sample count")
				}
				if count > 0 && flags&4 != 0 {
					offset := 8
					if flags&1 != 0 {
						offset += 4
					}
					if len(field.data) < offset+4 {
						return errors.New("truncated first sample flags")
					}
					track.Samples[sample].IsNonSyncSample = binary.BigEndian.Uint32(field.data[offset:])&0x10000 != 0
				}
				sample += count
			}
		}
	}
	return nil
}
func splitFMP4(b []byte) ([]byte, []byte, error) {
	for i := 0; i+8 <= len(b); {
		size := int64(binary.BigEndian.Uint32(b[i:]))
		head := 8
		if size == 1 {
			if i+16 > len(b) {
				break
			}
			v := binary.BigEndian.Uint64(b[i+8:])
			if v > math.MaxInt64 {
				break
			}
			size = int64(v)
			head = 16
		}
		if string(b[i+4:i+8]) == "moof" {
			return b[:i], b[i:], nil
		}
		if size < int64(head) || size > int64(len(b)-i) {
			break
		}
		i += int(size)
	}
	return nil, nil, errors.New("FFmpeg did not produce fragmented MP4")
}
