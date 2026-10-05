package dublift

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCachedPlaybackLinksSurviveCleanupAndRestart(t *testing.T) {
	s := lifecycleServer(t)
	c, _ := ParseContent("series", "tmdb:81349:1:2")
	var stream Stream
	json.Unmarshal([]byte(`{"name":"Original name","title":"Devs S01E02","url":"https://origin.test/video.m3u8?private=secret-value","behaviorHints":{"proxyHeaders":{"request":{"Cookie":"session=secret-cookie"}}},"providerData":true}`), &stream)
	v := s.newSession(c, stream)
	v.Order = 7
	v.SourceID, v.SourceName = "fixture-source-id", "Fixture addon"
	v.ticket = s.sealPlayback(v)
	url := s.playbackURL(httptest.NewRequest("GET", "http://local.test/", nil), v)
	if strings.Contains(url, "secret") || v.ticket == "" {
		t.Fatal("playback ticket exposed origin credentials")
	}
	s.beginLookup()
	if s.getSession(v.ID) != nil {
		t.Fatal("cleanup retained old session")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("HEAD", url, nil))
	if w.Code != 200 || len(s.sessions) != 0 {
		t.Fatal("HEAD revived a discarded session", w.Code)
	}
	restored, err := s.restorePlayback(v.ID, v.ticket)
	if err != nil || restored.ID != v.ID || restored.Order != 7 || restored.Content.BaseID != "81349" || restored.Content.Episode != 2 {
		t.Fatal(restored, err)
	}
	a, _ := json.Marshal(stream)
	b, _ := json.Marshal(restored.stream)
	if !reflect.DeepEqual(jsonValue(t, a), jsonValue(t, b)) {
		t.Fatal("restored stream lost metadata or request headers")
	}
	if restored.ctx.Err() != nil {
		t.Fatal("restored session inherited canceled work")
	}
	if restored.SourceID != v.SourceID || restored.SourceName != v.SourceName {
		t.Fatal("restored session lost its source filter identity")
	}
	s2, err := NewServer(s.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	restarted, err := s2.restorePlayback(v.ID, v.ticket)
	if err != nil {
		t.Fatal("restart invalidated cached stream links", err)
	}
	if restarted.SourceID != v.SourceID || restarted.SourceName != v.SourceName {
		t.Fatal("restart lost the source of a cached playback link")
	}
}

func TestPlaybackTicketsRejectTamperingAndExpiry(t *testing.T) {
	s := lifecycleServer(t)
	c, _ := ParseContent("movie", "tmdb:603")
	v := s.newSession(c, Stream{URL: "https://origin.test/video.m3u8"})
	encoded := s.sealPlayback(v)
	b, _ := base64.RawURLEncoding.DecodeString(encoded)
	b[len(b)-1] ^= 1
	if _, err := s.readPlayback(v.ID, base64.RawURLEncoding.EncodeToString(b)); err == nil {
		t.Fatal("accepted tampered source")
	}
	if _, err := s.readPlayback("another-session", encoded); err == nil {
		t.Fatal("ticket usable with a different session")
	}
	ticket, err := s.readPlayback(v.ID, encoded)
	if err != nil {
		t.Fatal(err)
	}
	ticket.Expires = time.Now().Add(-time.Second).Unix()
	plain, _ := json.Marshal(ticket)
	nonce := make([]byte, s.playbackKey.NonceSize())
	expired := base64.RawURLEncoding.EncodeToString(s.playbackKey.Seal(nonce, nonce, plain, []byte("DubLift playback v1")))
	if _, err := s.restorePlayback(v.ID, expired); err == nil {
		t.Fatal("expired ticket revived session")
	}
}

func TestVideoResourceCheckRejectsAudioOnlyFalseSuccess(t *testing.T) {
	s := lifecycleServer(t)
	for _, status := range []int{200, 403, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=0-511" || r.Header.Get("Referer") != "https://required.test/" {
					t.Error("missing bounded range or required origin headers")
				}
				w.Header().Set("Content-Type", "video/mp2t")
				w.WriteHeader(status)
				w.Write(make([]byte, 512))
			}))
			defer origin.Close()
			h, err := ParseHLS([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n"), Origin{URL: origin.URL + "/video.m3u8"})
			if err != nil {
				t.Fatal(err)
			}
			a := &Asset{HLS: h, Origin: Origin{Headers: http.Header{"Referer": {"https://required.test/"}}}}
			err = s.checkVideoResources(context.Background(), a)
			if (err == nil) != (status == 200) {
				t.Fatalf("video status %d: %v", status, err)
			}
		})
	}
}

func TestFailedVariantCannotBecomePreparedSource(t *testing.T) {
	s := lifecycleServer(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/master.m3u8" {
			w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nunavailable.m3u8\n"))
		} else {
			w.WriteHeader(502)
		}
	}))
	defer origin.Close()
	asset, _, _, err := s.inspectSource(context.Background(), Stream{URL: origin.URL + "/master.m3u8"})
	if err == nil || asset != nil {
		t.Fatal("failed child playlist did not propagate its error")
	}
}
