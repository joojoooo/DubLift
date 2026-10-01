package dublift

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRangeContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		cr     string
		ok     bool
	}{{"valid", 206, "bytes 0-0/999", true}, {"ignored", 200, "", false}, {"wrong start", 206, "bytes 1-1/999", false}, {"wrong end", 206, "bytes 0-3/999", false}, {"unknown size", 206, "bytes 0-0/*", false}, {"missing", 206, "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=0-0" {
					t.Error("missing probe Range")
				}
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Content-Range", tc.cr)
				w.WriteHeader(tc.status)
				w.Write([]byte{0})
			}))
			defer srv.Close()
			_, err := NewNetwork().OpenFile(context.Background(), Origin{URL: srv.URL})
			if (err == nil) != tc.ok {
				t.Fatalf("got %v", err)
			}
		})
	}
}
func TestRangeEveryReadAndHeaders(t *testing.T) {
	data := bytes.Repeat([]byte{12}, 4096)
	calls := 0
	ignore := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Referer") != "https://required.example/" || r.Header.Get("Cookie") != "session=secret" {
			t.Error("lost headers")
		}
		if ignore {
			w.WriteHeader(200)
			return
		}
		http.ServeContent(w, r, "movie.mkv", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	n := NewNetwork()
	f, err := n.OpenFile(context.Background(), Origin{srv.URL, http.Header{"Referer": {"https://required.example/"}, "Cookie": {"session=secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 31)
	if _, err = f.ReadAtContext(context.Background(), buf, 3021); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || n.Bytes.Load() != 32 {
		t.Fatalf("unexpected reads: %d / %d", calls, n.Bytes.Load())
	}
	ignore = true
	if _, err = f.ReadAtContext(context.Background(), buf, 32); err == nil {
		t.Fatal("allowed a full-body response after the initial probe")
	}
}

func TestRedirectedFileUsesEntryURLForLaterRanges(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 100)
	entryRequests := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/entry":
			entryRequests++
			if entryRequests == 1 {
				http.Redirect(w, r, "/first-worker", http.StatusTemporaryRedirect)
			} else if entryRequests == 2 {
				http.Redirect(w, r, "/bad-worker", http.StatusTemporaryRedirect)
			} else {
				http.Redirect(w, r, "/next-worker", http.StatusTemporaryRedirect)
			}
		case "/first-worker":
			if entryRequests != 1 {
				t.Error("later range was pinned to the first worker")
			}
			http.ServeContent(w, r, "movie.mkv", time.Time{}, bytes.NewReader(data))
		case "/bad-worker":
			w.WriteHeader(http.StatusOK)
			w.Write(data)
		case "/next-worker":
			http.ServeContent(w, r, "movie.mkv", time.Time{}, bytes.NewReader(data))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f, err := NewNetwork().OpenFile(context.Background(), Origin{URL: srv.URL + "/entry"})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := f.ReadAtContext(context.Background(), buf, 500); err != nil || !bytes.Equal(buf, data[500:510]) {
		t.Fatalf("later range: %q, %v", buf, err)
	}
	if entryRequests != 3 {
		t.Fatalf("entry requests: %d, want 3", entryRequests)
	}
}

func TestFileRangeRetriesStalledHeadersWithoutTimingOutBody(t *testing.T) {
	var calls atomic.Int32
	stalledCanceled := make(chan struct{})
	const headerWait = 50 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done()
			close(stalledCanceled)
			return
		}
		if r.Header.Get("Range") != "bytes=0-1" {
			t.Error("retry lost the bounded range")
		}
		w.Header().Set("Content-Range", "bytes 0-1/2")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(206)
		w.Write([]byte{1})
		w.(http.Flusher).Flush()
		time.Sleep(3 * headerWait)
		w.Write([]byte{2})
	}))
	defer srv.Close()
	n := NewNetwork()
	n.rangeHeaderTimeout = headerWait
	f := &RemoteFile{Net: n, Origin: Origin{URL: srv.URL}, Size: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The first read retries a stalled worker. The next one's body outlives
	// the header timer even on its first attempt.
	for range 2 {
		data := make([]byte, 2)
		if _, err := f.ReadAtContext(ctx, data, 0); err != nil || !bytes.Equal(data, []byte{1, 2}) {
			t.Fatalf("file body was interrupted: %v, %v", data, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("origin requests = %d, want one retry plus two successful reads", calls.Load())
	}
	select {
	case <-stalledCanceled:
	default:
		t.Fatal("stalled worker was left running")
	}
}

func TestFileRangeCanOutliveGeneralHTTPClientTimeout(t *testing.T) {
	const clientTimeout = 40 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-1" {
			t.Errorf("range = %q", got)
		}
		w.Header().Set("Content-Range", "bytes 0-1/2")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(206)
		w.Write([]byte{1})
		w.(http.Flusher).Flush()
		time.Sleep(3 * clientTimeout)
		w.Write([]byte{2})
	}))
	defer srv.Close()
	n := NewNetwork()
	n.Client.Timeout = clientTimeout
	f := &RemoteFile{Net: n, Origin: Origin{URL: srv.URL}, Size: 2}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data := make([]byte, 2)
	if _, err := f.ReadAtContext(ctx, data, 0); err != nil || !bytes.Equal(data, []byte{1, 2}) {
		t.Fatalf("progressing file body was cut off: %v, %v", data, err)
	}
}

func TestOpenAssetUsesOneBoundedInitialRequest(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 128<<10)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Range"); got != "bytes=0-65535" {
			t.Errorf("initial range: %q", got)
		}
		http.ServeContent(w, r, "movie.mkv", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	engine := testEngine(t)
	if _, err := engine.OpenAsset(context.Background(), Origin{URL: srv.URL + "/movie.mkv"}, false); err == nil {
		t.Fatal("accepted an invalid container")
	}
	if requests != 1 {
		t.Fatalf("initial requests: %d, want 1", requests)
	}
}
func TestVixPort(t *testing.T) {
	html := `window.masterPlaylist = {url: '/playlist/test?b=1&token=old', params: {token:'ab\x2bcd', expires:12345, empty:'', nested:{}}}; window.canPlayFHD=true;`
	got, err := ExtractVixPlaylist(html, "https://vix.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"/playlist/test.m3u8?", "b=1", "token=ab%2Bcd", "expires=12345", "h=1"} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %s in %s", s, got)
		}
	}
	if strings.Contains(got, "empty=") {
		t.Error("kept empty optional value")
	}
	if _, err = ExtractVixPlaylist(`window.masterPlaylist={url:fetch('/steal'),params:{token:process.env.SECRET,expires:42}}`, "https://vix.example"); err == nil {
		t.Error("accepted nonliteral config")
	}
}
func TestVixRetryAndEpisode(t *testing.T) {
	attempts := 0
	embedCalls := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != srv.URL+"/" || r.Header.Get("Origin") != srv.URL {
			t.Error("Vixsrc headers lost")
		}
		switch r.URL.Path {
		case "/api/tv/81349/1/2":
			attempts++
			fmt.Fprintf(w, `{"src":"/embed/%d"}`, attempts)
		case "/embed/1":
			embedCalls++
			w.WriteHeader(410)
		case "/embed/2":
			embedCalls++
			io.WriteString(w, `window.masterPlaylist={url:'/playlist/episode',params:{token:'signed',expires:12345}}`)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	o, e := NewNetwork().ResolveVix(context.Background(), srv.URL, "series", "81349", 1, 2)
	if e != nil {
		t.Fatal(e)
	}
	if attempts != 2 || embedCalls != 2 || !strings.Contains(o.URL, "episode.m3u8") {
		t.Fatal(attempts, embedCalls, o)
	}
}
func TestHLSWindowState(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:42
#EXT-X-KEY:METHOD=AES-128,URI="../key"
#EXT-X-MAP:URI="init.mp4",BYTERANGE="64@0"
#EXTINF:6,
#EXT-X-BYTERANGE:100@64
blob.mp4
#EXTINF:6,
#EXT-X-BYTERANGE:110
blob.mp4
#EXTINF:6,
#EXT-X-BYTERANGE:120
blob.mp4
#EXT-X-ENDLIST
`
	h, e := ParseHLS([]byte(raw), Origin{URL: "https://example.test/path/media.m3u8"})
	if e != nil {
		t.Fatal(e)
	}
	window, start, e := h.Window(7, 3, func(s string) string { return s })
	if e != nil {
		t.Fatal(e)
	}
	if start != 6 {
		t.Fatal(start)
	}
	for _, s := range []string{"#EXT-X-MEDIA-SEQUENCE:43", "#EXT-X-BYTERANGE:110@164", `URI="https://example.test/key"`, `URI="https://example.test/path/init.mp4"`} {
		if !strings.Contains(window, s) {
			t.Fatalf("missing %s\n%s", s, window)
		}
	}
	if strings.Count(window, "#EXTINF") != 1 {
		t.Fatal("window included unrequested segments")
	}
}
func TestAlignmentTimelinePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alignment.json")
	store, e := OpenAlignments(path)
	if e != nil {
		t.Fatal(e)
	}
	e = store.Update("edition-a", func(a *Alignment) {
		a.Anchors = []Anchor{{VixTime: 40, SourceTime: 42, Offset: 2, Confidence: .9}, {VixTime: 500, SourceTime: 507, Offset: 7, Confidence: .95}}
		a.Choose(.68)
		a.Boundaries = []Boundary{{SourceTime: 600, Offset: 7, Confidence: .8}}
	})
	if e != nil {
		t.Fatal(e)
	}
	fresh, e := OpenAlignments(path)
	if e != nil {
		t.Fatal(e)
	}
	a := fresh.Get("edition-a")
	if !a.DifferentEdit || a.At(10, .68) != 2 || a.At(700, .68) != 7 || a.At(700, .99) != 0 {
		t.Fatal(a)
	}
	manual := -3.25
	a.Manual = &manual
	if a.At(700, .68) != manual {
		t.Fatal("manual override ignored")
	}
	if fresh.Get("edition-b").Confidence != 0 {
		t.Fatal("source identities mixed")
	}
}
func signalPCM(seconds int) []int16 {
	n := seconds * pcmRate
	out := make([]int16, n)
	rng := rand.New(rand.NewPCG(123, 456))
	phase := [4]float64{}
	freq := [4]float64{}
	amp := [4]float64{}
	for i := range out {
		if i%(pcmRate/3) == 0 {
			for j := range freq {
				freq[j] = 100 + rng.Float64()*float64(800+j*500)
				amp[j] = .05 + rng.Float64()*.18
			}
		}
		x := 0.0
		for j := range phase {
			phase[j] += 2 * math.Pi * freq[j] / pcmRate
			x += amp[j] * math.Sin(phase[j])
		}
		out[i] = int16(x * 25000)
	}
	return out
}
func TestChromaprintSpectralAlignment(t *testing.T) {
	hay := signalPCM(60)
	start := int(math.Round(11.375 * pcmRate))
	for _, seconds := range []int{5, 24} {
		needle := append([]int16{}, hay[start:start+seconds*pcmRate]...)
		for i := range needle {
			needle[i] = int16(float64(needle[i]) * .67)
		}
		lag, confidence, e := MatchPCM(context.Background(), needle, hay)
		if e != nil {
			t.Fatal(seconds, e)
		}
		if math.Abs(lag-11.375) > .035 || confidence < .68 {
			t.Fatalf("%ds: lag=%.6f confidence=%.3f", seconds, lag, confidence)
		}
	}
	if _, _, e := MatchPCM(context.Background(), make([]int16, 5*pcmRate), hay); e == nil {
		t.Fatal("silence matched")
	}
}
func TestFindAnchorSearchesLongVixClipAgainstShortSourceClip(t *testing.T) {
	vixPCM := signalPCM(60)
	shift := int(2.4 * pcmRate)
	for _, offset := range []float64{2.4, -2.4} {
		sourcePCM := make([]int16, len(vixPCM))
		if offset > 0 {
			copy(sourcePCM[shift:], vixPCM)
		} else {
			copy(sourcePCM, vixPCM[shift:])
		}
		var sourceAt, sourceLength, vixAt, vixLength float64
		extract := func(pcm []int16, at, length float64) []int16 {
			start, end := int(math.Round(at*pcmRate)), int(math.Round((at+length)*pcmRate))
			return pcm[start:end]
		}
		source := func(_ context.Context, at, length float64) ([]int16, error) {
			sourceAt, sourceLength = at, length
			return extract(sourcePCM, at, length), nil
		}
		vix := func(_ context.Context, at, length float64) ([]int16, error) {
			vixAt, vixLength = at, length
			return extract(vixPCM, at, length), nil
		}
		anchor, err := FindAnchor(context.Background(), source, vix, 10, 0, 10, 60, 5)
		if err != nil || math.Abs(anchor.Offset-offset) > .05 || anchor.Confidence < .68 {
			t.Fatalf("offset %.1f: anchor %+v, error %v", offset, anchor, err)
		}
		if sourceAt != 10 || sourceLength != 5 || vixAt != 0 || vixLength != 25 {
			t.Fatalf("wrong extraction windows: source %.1f+%.1f, Vixsrc %.1f+%.1f", sourceAt, sourceLength, vixAt, vixLength)
		}
	}
}
func TestCacheBoundAndSingleFlight(t *testing.T) {
	c := NewByteCache(10)
	var calls atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, e := c.Get(context.Background(), "one", func() ([]byte, error) { calls.Add(1); time.Sleep(10 * time.Millisecond); return make([]byte, 8), nil })
			if e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	c.Get(context.Background(), "two", func() ([]byte, error) { return make([]byte, 7), nil })
	if c.Used() > 10 {
		t.Fatal("cache grew without bound")
	}
}
func TestPreparationTimeoutSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c, err := OpenConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	settings := c.Get()
	if settings.PreparationTimeoutSeconds != 120 {
		t.Fatal("unexpected preparation timeout", settings.PreparationTimeoutSeconds)
	}
	settings.PreparationTimeoutSeconds = 240
	if err := c.Save(settings); err != nil {
		t.Fatal(err)
	}
	c, err = OpenConfig(path)
	if err != nil || c.Get().PreparationTimeoutSeconds != 240 {
		t.Fatalf("timeout did not persist: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(b, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "preparationTimeoutSeconds")
	b, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	c, err = OpenConfig(path)
	if err != nil || c.Get().PreparationTimeoutSeconds != 120 {
		t.Fatalf("existing config did not receive timeout default: %v", err)
	}
	for _, timeout := range []int{0, 601} {
		settings.PreparationTimeoutSeconds = timeout
		if settings.Validate() == nil {
			t.Fatalf("accepted timeout %d", timeout)
		}
	}
}

func TestPrivateConfigAndIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "config.json")
	c, e := OpenConfig(path)
	if e != nil {
		t.Fatal(e)
	}
	st, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	if st.Mode().Perm() != 0600 || c.Get().Listen != "0.0.0.0:7000" {
		t.Fatal("incorrect defaults or permissions")
	}
	for _, id := range []string{"tmdb:81349:1:2", "81349:1:2", "tt8134186:1:2"} {
		v, e := ParseContent("series", id)
		if e != nil || v.Season != 1 || v.Episode != 2 {
			t.Fatal(v, e)
		}
	}
	if _, e := ParseContent("series", "tmdb:81349"); e == nil {
		t.Fatal("episode accepted without season / episode")
	}
}
