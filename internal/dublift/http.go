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
	Client *http.Client
	Bytes  atomic.Int64
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
		return nil
	}}}
}
func (n *Network) request(ctx context.Context, o Origin, method, byteRange string) (*http.Response, error) {
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
	resp, e := n.Client.Do(r)
	if e != nil {
		return nil, fmt.Errorf("origin request failed: %s", networkError(e))
	}
	return resp, nil
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
	Net    *Network
	Origin Origin
	Size   int64
	ETag   string
}

func (n *Network) OpenFile(ctx context.Context, o Origin) (*RemoteFile, error) {
	resp, e := n.request(ctx, o, "GET", "bytes=0-0")
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 {
		return nil, errors.New("file rejected: origin must honor Range with 206 Partial Content")
	}
	a, b, size, e := parseContentRange(resp.Header.Get("Content-Range"))
	if e != nil || a != 0 || b != 0 {
		return nil, errors.New("file rejected: invalid range probe response")
	}
	if resp.ContentLength > 1 {
		return nil, errors.New("file rejected: invalid range probe length")
	}
	data, e := readBounded(resp.Body, 1)
	if e != nil || len(data) != 1 {
		return nil, errors.New("file rejected: invalid range probe body")
	}
	n.Bytes.Add(1)
	return &RemoteFile{n, Origin{resp.Request.URL.String(), o.Headers}, size, resp.Header.Get("ETag")}, nil
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
	resp, e := f.Net.request(ctx, f.Origin, "GET", fmt.Sprintf("bytes=%d-%d", off, off+want-1))
	if e != nil {
		return 0, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 {
		return 0, errors.New("origin stopped honoring Range; full download prevented")
	}
	a, b, total, e := parseContentRange(resp.Header.Get("Content-Range"))
	if e != nil || a != off || b != off+want-1 || total != f.Size {
		return 0, errors.New("origin returned an inconsistent Content-Range")
	}
	if f.ETag != "" && resp.Header.Get("ETag") != "" && resp.Header.Get("ETag") != f.ETag {
		return 0, errors.New("source file changed during playback")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != want {
		return 0, errors.New("origin returned an inconsistent range length")
	}
	n, e := io.ReadFull(resp.Body, p[:want])
	f.Net.Bytes.Add(int64(n))
	if e == nil && int64(len(p)) > want {
		e = io.EOF
	}
	return n, e
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
