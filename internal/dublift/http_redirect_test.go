package dublift

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOriginRedirectDoesNotInventReferer(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := ""
		if r.URL.Query().Get("explicit") == "1" {
			expected = "https://required.test/player"
		}
		if got := r.Header.Get("Referer"); got != expected {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.Header.Get("Range") != "bytes=0-3" {
			t.Errorf("redirect lost Range: %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes 0-3/8")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "abcd")
	}))
	defer media.Close()
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, media.URL+"/resource?explicit="+r.URL.Query().Get("explicit"), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()
	net := NewNetwork()
	for _, ref := range []string{"", "https://required.test/player"} {
		h := http.Header{}
		explicit := "0"
		if ref != "" {
			h.Set("Referer", ref)
			explicit = "1"
		}
		resp, err := net.request(context.Background(), Origin{URL: fmt.Sprintf("%s/video.mp4?private=signed&explicit=%s", entry.URL, explicit), Headers: h}, http.MethodGet, "bytes=0-3")
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusPartialContent || string(body) != "abcd" {
			t.Fatalf("referrer %q: status %d, body %q, read %v", ref, resp.StatusCode, body, readErr)
		}
	}
}
