package dublift

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDashboardSetupAndAddonName(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/legacy/manifest.json" {
			io.WriteString(w, `{"name":"Legacy Addon","icon":"/legacy-icon.png"}`)
			return
		}
		io.WriteString(w, `{"name":"  Fixture Addon  ","logo":"/addon-logo.png","icon":"/old-icon.png"}`)
	}))
	defer upstream.Close()
	c, err := OpenConfig(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	local := httptest.NewServer(s)
	defer local.Close()

	post := func(path string, body any) *http.Response {
		t.Helper()
		data, _ := json.Marshal(body)
		response, err := http.Post(local.URL+path, "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 {
			message, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("%s: %d %s", path, response.StatusCode, message)
		}
		return response
	}
	empty, _ := json.Marshal(c.Get())
	rejected, err := http.Post(local.URL+"/api/settings", "application/json", bytes.NewReader(empty))
	if err != nil {
		t.Fatal(err)
	}
	rejected.Body.Close()
	if rejected.StatusCode != 400 {
		t.Fatalf("settings accepted zero upstream addons: %d", rejected.StatusCode)
	}
	url := upstream.URL + "/manifest.json"
	response := post("/api/addon-name", map[string]string{"manifestURL": url})
	var name struct {
		Name string `json:"name"`
		Icon string `json:"icon"`
	}
	if err := json.NewDecoder(response.Body).Decode(&name); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if name.Name != "Fixture Addon" {
		t.Fatalf("unexpected addon name: %q", name.Name)
	}
	if name.Icon != upstream.URL+"/addon-logo.png" {
		t.Fatalf("unexpected addon icon: %q", name.Icon)
	}
	response = post("/api/addon-name", map[string]string{"manifestURL": upstream.URL + "/legacy/manifest.json"})
	if err := json.NewDecoder(response.Body).Decode(&name); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if name.Icon != upstream.URL+"/legacy-icon.png" {
		t.Fatalf("legacy addon icon was lost: %q", name.Icon)
	}

	cfg := c.Get()
	cfg.Addons = []Addon{{Name: "User supplied name", ManifestURL: url}}
	cfg.AlignmentSampleSeconds = 24
	response = post("/api/settings", cfg)
	response.Body.Close()
	if c.Get().Addons[0].Name != "Fixture Addon" || c.Get().AlignmentSampleSeconds != 24 {
		t.Fatal("settings did not save the upstream name and sample length")
	}
	response = post("/api/setup-complete", struct{}{})
	response.Body.Close()
	fresh, err := OpenConfig(c.path)
	if err != nil || !fresh.Get().SetupCompleted {
		t.Fatalf("setup completion did not persist: %v", err)
	}
}

func TestStreamSourceFormatUsesCheckedMedia(t *testing.T) {
	stream := Stream{URL: "https://origin.test/source.m3u8?token=redacted"}
	stream.BehaviorHints.Filename = "movie.mkv"
	if got := streamSourceFormat(stream, nil); got != "hls" {
		t.Fatalf("URL format lost to filename: %s", got)
	}
	stream.URL = "https://origin.test/opaque"
	if got := streamSourceFormat(stream, nil); got != "mkv" {
		t.Fatalf("filename format not used: %s", got)
	}
	if got := streamSourceFormat(stream, &Asset{HLS: &HLS{}}); got != "hls" {
		t.Fatalf("checked HLS format not used: %s", got)
	}
	if got := streamSourceFormat(Stream{URL: "https://origin.test/opaque"}, &Asset{Index: FileIndex{Container: "matroska"}}); got != "mkv" {
		t.Fatalf("checked Matroska format not used: %s", got)
	}
	stream = Stream{URL: "https://origin.test/movie.MP4?token=redacted"}
	stream.BehaviorHints.Filename = "movie.mkv"
	if got := streamSourceFormat(stream, nil); got != "mp4" {
		t.Fatalf("MP4 URL format lost to filename: %s", got)
	}
	stream.URL = "https://origin.test/opaque"
	stream.BehaviorHints.Filename = "movie.m4v"
	if got := streamSourceFormat(stream, nil); got != "mp4" {
		t.Fatalf("MP4 filename format not used: %s", got)
	}
	if got := streamSourceFormat(Stream{URL: "https://origin.test/opaque"}, &Asset{Index: FileIndex{Container: "mp4"}}); got != "mp4" {
		t.Fatalf("checked MP4 format not used: %s", got)
	}
}

func TestStatusReportsPreparationBeforeMediaIsReady(t *testing.T) {
	s := lifecycleServer(t)
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: "https://origin.test/movie.m3u8"})
	v.listedReady = make(chan struct{})
	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	snapshot := func() map[string]any {
		t.Helper()
		return s.status(request)["sessions"].([]map[string]any)[0]
	}
	if snapshot()["preparationStarted"] != false {
		t.Fatal("unprepared stream was marked as preparing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.prepareSession(ctx, v); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled request, got %v", err)
	}
	prepared := snapshot()
	if prepared["preparationStarted"] != true || prepared["preparationDone"] != false {
		t.Fatalf("preparation state not exposed while source check is pending: %v", prepared)
	}
}

func TestStatusEventStreamReportsManifestRequest(t *testing.T) {
	s := lifecycleServer(t)
	local := httptest.NewServer(s)
	defer local.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", local.URL+"/api/events", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("missing event stream content type")
	}
	reader := bufio.NewReader(response.Body)
	readStatus := func() map[string]any {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var state map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &state); err != nil {
				t.Fatal(err)
			}
			return state
		}
	}
	first := readStatus()
	if first["cacheMaxBytes"] != float64(256<<20) || first["manifestRequests"] != float64(0) {
		t.Fatal(first)
	}
	manifest, err := http.Get(local.URL + "/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest.Body.Close()
	second := readStatus()
	if second["manifestRequests"] != float64(1) {
		t.Fatal(second)
	}
}
