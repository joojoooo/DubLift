package dublift

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const manifestLimit = 4 << 20
const segmentLimit = 96 << 20

type Origin struct {
	URL     string
	Headers http.Header
}
type Network struct {
	Client             *http.Client
	Bytes              atomic.Int64
	rangeHeaderTimeout time.Duration
}

// Video bytes are counted on each origin read, including while a range
// request is still in progress. The context selects the playback session.
type videoBytesKey struct{}

type videoDownload struct {
	bytes  atomic.Int64
	active atomic.Int64
}

func beginVideoDownload(ctx context.Context) func() {
	if meter, ok := ctx.Value(videoBytesKey{}).(*videoDownload); ok {
		meter.active.Add(1)
		return func() { meter.active.Add(-1) }
	}
	return func() {}
}

func recordVideoBytes(ctx context.Context, size int) {
	if meter, ok := ctx.Value(videoBytesKey{}).(*videoDownload); ok {
		meter.bytes.Add(int64(size))
	}
}

type videoByteWriter struct{ ctx context.Context }

func (w videoByteWriter) Write(p []byte) (int, error) {
	recordVideoBytes(w.ctx, len(p))
	return len(p), nil
}

func NewNetwork() *Network {
	jar, _ := cookiejar.New(nil)
	return &Network{Client: &http.Client{Jar: jar, Timeout: 45 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 12 * time.Second, KeepAlive: 30 * time.Second, Resolver: platformResolver()}).DialContext, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 64, MaxConnsPerHost: 12, DisableCompression: true}, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many origin redirects")
		}
		if _, e := httpURL(r.URL.String()); e != nil {
			return e
		}
		// Go synthesizes a Referer for every redirect, including signed media
		// URLs. Some CDNs reject that header. Preserve only a Referer that the
		// upstream addon explicitly supplied with the original request.
		if via[0].Header.Get("Referer") == "" {
			r.Header.Del("Referer")
		}
		return nil
	}}}
}
func (n *Network) request(ctx context.Context, o Origin, method, byteRange string) (*http.Response, error) {
	return n.requestWithClient(ctx, o, method, byteRange, n.Client)
}

func (n *Network) requestWithClient(ctx context.Context, o Origin, method, byteRange string, client *http.Client) (*http.Response, error) {
	if _, e := httpURL(o.URL); e != nil {
		return nil, e
	}
	r, e := http.NewRequestWithContext(ctx, method, o.URL, nil)
	if e != nil {
		return nil, e
	}
	r.Header = o.Headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	for _, k := range []string{"Host", "Connection", "Content-Length", "Transfer-Encoding", "Range", "Accept-Encoding"} {
		r.Header.Del(k)
	}
	r.Header.Set("Accept-Encoding", "identity")
	if byteRange != "" {
		r.Header.Set("Range", byteRange)
	}
	resp, e := client.Do(r)
	if e != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("origin request failed: %s: %w", networkError(ctx.Err()), ctx.Err())
		}
		return nil, fmt.Errorf("origin request failed: %s", networkError(e))
	}
	return resp, nil
}

func (n *Network) requestFileRange(ctx context.Context, o Origin, byteRange string) (*http.Response, error) {
	// A finite origin range can stay open while the demuxer consumes many
	// cached chunks. The general HTTP client's whole-response timeout must
	// not cut off an otherwise progressing read. The header and per-chunk
	// deadlines, plus the caller's job context, still bound this request.
	client := *n.Client
	client.Timeout = 0
	for attempt := 0; ; attempt++ {
		requestCtx, cancel := context.WithCancel(ctx)
		var timer *time.Timer
		if attempt == 0 {
			timeout := n.rangeHeaderTimeout
			if timeout <= 0 {
				timeout = 3 * time.Second
			}
			// A redirector can select a stalled worker. Retry before it drains
			// the player's buffer; the second attempt retains the normal limit.
			// Stop this timer at the headers, not at the end of a large body.
			timer = time.AfterFunc(timeout, cancel)
		}
		resp, err := n.requestWithClient(requestCtx, o, "GET", byteRange, &client)
		if timer != nil && !timer.Stop() && err == nil {
			resp.Body.Close()
			err = context.DeadlineExceeded
		}
		if err != nil {
			cancel()
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
			return nil, err
		}
		resp.Body = &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
		// A redirector can send consecutive ranges to different workers. A
		// single bad worker must not fail a seek; close its response without
		// reading a whole-file body before trying the entry URL once more.
		if attempt > 0 || !retryableRangeStatus(resp.StatusCode) {
			return resp, nil
		}
		resp.Body.Close()
	}
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelReadCloser) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}

func retryableRangeStatus(status int) bool {
	return status == 200 || status == 403 || status == 429 || status >= 500
}

// Do not expose signed URLs or headers in logs / dashboard errors.
func networkError(e error) string {
	if errors.Is(e, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(e, context.Canceled) {
		return "cancelled"
	}
	return "network or TLS error"
}
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("response exceeds byte budget")
	}
	return b, e
}
func (n *Network) Fetch(ctx context.Context, o Origin, limit int64) ([]byte, string, error) {
	resp, e := n.request(ctx, o, "GET", "")
	if e != nil {
		return nil, "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", &HTTPError{resp.StatusCode}
	}
	if resp.ContentLength > limit {
		return nil, "", errors.New("response exceeds byte budget")
	}
	b, e := readBounded(resp.Body, limit)
	n.Bytes.Add(int64(len(b)))
	return b, resp.Request.URL.String(), e
}

type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("origin HTTP %d", e.Status) }
func parseContentRange(s string) (start, end, total int64, err error) {
	var tail string
	if !strings.HasPrefix(s, "bytes ") {
		return 0, 0, 0, errors.New("missing Content-Range")
	}
	p := strings.Split(strings.TrimPrefix(s, "bytes "), "/")
	if len(p) != 2 {
		return 0, 0, 0, errors.New("invalid Content-Range")
	}
	tail = p[1]
	r := strings.Split(p[0], "-")
	if len(r) != 2 {
		return 0, 0, 0, errors.New("invalid Content-Range")
	}
	start, e1 := strconv.ParseInt(r[0], 10, 64)
	end, e2 := strconv.ParseInt(r[1], 10, 64)
	total, e3 := strconv.ParseInt(tail, 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || start < 0 || end < start || total <= end {
		err = errors.New("invalid Content-Range")
	}
	return
}

type RemoteFile struct {
	Net     *Network
	Origin  Origin
	Size    int64
	ETag    string
	prefix  []byte
	tailOff int64
	tail    []byte
}

// Matroska seek tables commonly live at EOF. Retain that small region after
// indexing so each short remux can seek even if a later CDN worker fails.
func (f *RemoteFile) pinTail(ctx context.Context) error {
	const limit = 2 << 20
	n := min(f.Size, int64(limit))
	data := make([]byte, n)
	if _, err := f.ReadAtContext(ctx, data, f.Size-n); err != nil {
		return err
	}
	f.tailOff, f.tail = f.Size-n, data
	return nil
}

// Leave time for the source check to complete. Tail retention only avoids
// later reads, so it should be skipped when discovery is near its
// deadline or an optional origin range stalls.
func optionalPinContext(parent context.Context) (context.Context, context.CancelFunc, bool) {
	const timeout = 5 * time.Second
	if parent.Err() != nil {
		return nil, nil, false
	}
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= timeout+time.Second {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return ctx, cancel, true
}

// Return pinned bytes starting at off, or the size of the gap before them.
func (f *RemoteFile) pinnedAt(off, size int64) ([]byte, int64) {
	if off < int64(len(f.prefix)) {
		n := min(size, int64(len(f.prefix))-off)
		return f.prefix[off : off+n], n
	}
	if len(f.tail) != 0 {
		if off >= f.tailOff {
			n := min(size, f.Size-off)
			return f.tail[off-f.tailOff : off-f.tailOff+n], n
		}
		if off+size > f.tailOff {
			return nil, f.tailOff - off
		}
	}
	return nil, size
}

func (n *Network) OpenFile(ctx context.Context, o Origin) (*RemoteFile, error) {
	resp, e := n.requestFileRange(ctx, o, "bytes=0-0")
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 {
		return n.fileFromProbe(o, resp, nil, 1)
	}
	data, e := readBounded(resp.Body, 1)
	if e != nil {
		return nil, errors.New("file rejected: invalid range probe body")
	}
	return n.fileFromProbe(o, resp, data, 1)
}

func (n *Network) fileFromProbe(o Origin, resp *http.Response, data []byte, requested int64) (*RemoteFile, error) {
	if resp.StatusCode != 206 {
		if resp.StatusCode != 200 {
			return nil, &HTTPError{resp.StatusCode}
		}
		return nil, errors.New("file rejected: origin must honor Range with 206 Partial Content")
	}
	a, b, size, e := parseContentRange(resp.Header.Get("Content-Range"))
	if e != nil || a != 0 || b != min(requested, size)-1 {
		return nil, errors.New("file rejected: invalid range probe response")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != b+1 {
		return nil, errors.New("file rejected: invalid range probe length")
	}
	if int64(len(data)) != b+1 {
		return nil, errors.New("file rejected: invalid range probe body")
	}
	n.Bytes.Add(int64(len(data)))
	// Keep the entry URL: a redirector may select a different healthy CDN
	// worker for each range. Pinning its first target breaks later seeks.
	return &RemoteFile{Net: n, Origin: o, Size: size, ETag: resp.Header.Get("ETag"), prefix: data}, nil
}
func (f *RemoteFile) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 || off >= f.Size {
		return 0, io.EOF
	}
	want := int64(len(p))
	if off+want > f.Size {
		want = f.Size - off
	}
	if want == 0 {
		return 0, nil
	}
	defer beginVideoDownload(ctx)()
	resp, e := f.openRange(ctx, off, want)
	if e != nil {
		return 0, e
	}
	defer resp.Body.Close()
	n, e := io.ReadFull(io.TeeReader(resp.Body, videoByteWriter{ctx}), p[:want])
	f.Net.Bytes.Add(int64(n))
	if e == nil && int64(len(p)) > want {
		e = io.EOF
	}
	return n, e
}

func (f *RemoteFile) openRange(ctx context.Context, off, want int64) (*http.Response, error) {
	if off < 0 || want <= 0 || off > f.Size-want {
		return nil, errors.New("invalid file range")
	}
	resp, e := f.Net.requestFileRange(ctx, f.Origin, fmt.Sprintf("bytes=%d-%d", off, off+want-1))
	if e != nil {
		return nil, e
	}
	if resp.StatusCode != 206 {
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, &HTTPError{resp.StatusCode}
		}
		return nil, errors.New("origin stopped honoring Range; full download prevented")
	}
	a, b, total, e := parseContentRange(resp.Header.Get("Content-Range"))
	if e != nil || a != off || b != off+want-1 || total != f.Size {
		resp.Body.Close()
		return nil, errors.New("origin returned an inconsistent Content-Range")
	}
	if f.ETag != "" && resp.Header.Get("ETag") != "" && resp.Header.Get("ETag") != f.ETag {
		resp.Body.Close()
		return nil, errors.New("source file changed during playback")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != want {
		resp.Body.Close()
		return nil, errors.New("origin returned an inconsistent range length")
	}
	return resp, nil
}
func requestRange(raw string, size int64) (int64, int64, error) {
	if !strings.HasPrefix(raw, "bytes=") || strings.Contains(raw, ",") {
		return 0, 0, errors.New("one byte range required")
	}
	p := strings.Split(strings.TrimPrefix(raw, "bytes="), "-")
	if len(p) != 2 {
		return 0, 0, errors.New("invalid Range")
	}
	if p[0] == "" {
		n, e := strconv.ParseInt(p[1], 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, errors.New("invalid suffix range")
		}
		return max(0, size-n), size - 1, nil
	}
	a, e := strconv.ParseInt(p[0], 10, 64)
	if e != nil || a < 0 || a >= size {
		return 0, 0, errors.New("unsatisfiable Range")
	}
	b := size - 1
	if p[1] != "" {
		b, e = strconv.ParseInt(p[1], 10, 64)
		if e != nil || b < a {
			return 0, 0, errors.New("invalid Range")
		}
		b = min(b, size-1)
	}
	return a, b, nil
}
