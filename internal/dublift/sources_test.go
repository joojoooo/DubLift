package dublift

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSourceConfigPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	config, err := OpenConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Get()
	cfg.Sources = withMovyDisabled([]Source{{Type: "addon", Name: "First", ManifestURL: "https://first.test/manifest.json"}, {Type: "addon", Name: "Second", ManifestURL: "https://second.test/manifest.json"}, {Type: "vixsrc", Name: "VixSrc", BaseURL: "https://vix.test"}})
	cfg.Sources[0], cfg.Sources[2] = cfg.Sources[2], cfg.Sources[0]
	cfg.Sources[0].Disabled = true
	cfg.SetupCompleted = true
	cfg.CacheMB = 512
	cfg.TMDBToken = "test-token"
	if config.Get().Sources[0].Disabled {
		t.Fatal("Get returned mutable config storage")
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	fresh, err := OpenConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Get(), cfg) {
		t.Fatal("current config did not persist")
	}
	cfg.Sources[0].BaseURL = "https://mutated.test"
	if config.Get().Sources[0].BaseURL != "https://vix.test" {
		t.Fatal("Save retained mutable input storage")
	}
	got := config.Get()
	got.Sources[0].BaseURL = "https://mutated.test"
	if config.Get().Sources[0].BaseURL != "https://vix.test" {
		t.Fatal("Get returned mutable config storage")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"addons"`)) || bytes.Contains(data, []byte(`"vixBaseURL"`)) {
		t.Fatal("legacy fields were saved")
	}
}

func TestSourceValidation(t *testing.T) {
	for _, sources := range [][]Source{
		nil,
		{},
		{{Type: "vixsrc", BaseURL: "https://vix.test"}, {Type: "vixsrc", BaseURL: "https://vix.test"}},
		{{Type: "unknown", BaseURL: "https://vix.test"}},
		{{Type: "vixsrc", BaseURL: "file:///tmp/video"}},
		{{Type: "vixsrc", BaseURL: "https://vix.test"}, {Type: "addon", ManifestURL: "https://addon.test/no-manifest"}},
	} {
		cfg := DefaultSettings()
		cfg.Sources = sources
		if cfg.Validate() == nil {
			t.Fatalf("accepted invalid sources: %+v", sources)
		}
	}
	cfg := DefaultSettings()
	for i := range cfg.Sources {
		cfg.Sources[i].Disabled = true
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal("all sources may be hidden", err)
	}
}

func TestSourceSettingsAPIAndFixedIcon(t *testing.T) {
	s := lifecycleServer(t)
	cfg := s.Config.Get()
	cfg.Sources[0].Disabled = true
	cfg.Sources[0].BaseURL = "https://configured.test"
	cfg.Sources[1].BaseURL = "https://configured-movy.test"
	data, _ := json.Marshal(cfg)
	req := httptest.NewRequest("POST", "/api/settings", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !s.Config.Get().Sources[0].Disabled || s.Config.Get().Source("vixsrc").BaseURL != "https://configured.test" || s.Config.Get().Source("movy").BaseURL != "https://configured-movy.test" {
		t.Fatal("settings did not persist")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/manifest.json", nil))
	if !strings.Contains(w.Body.String(), `"configurationRequired":true`) {
		t.Fatal("hidden sources should require configuration")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/source-types", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"urlLabel":"Vixsrc base URL"`) || !strings.Contains(w.Body.String(), `"italianAudio":true`) || !strings.Contains(w.Body.String(), `"type":"movy","name":"Movy","icon":"/movy.png","urlLabel":"Movy base URL"`) || !strings.Contains(w.Body.String(), `"italianAudio":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/vixsrc.ico", nil))
	if w.Code != 200 || w.Body.Len() == 0 {
		t.Fatal("missing embedded VixSrc icon")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/movy.png", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.Len() == 0 {
		t.Fatal("missing embedded Movy icon")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/settings", nil))
	var saved Settings
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || saved.ConfigVersion != currentConfigVersion {
		t.Fatal("settings API omitted the config version", err)
	}
	validData, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(validData, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "configVersion")
	unversioned, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	fields["configVersion"], err = json.Marshal(currentConfigVersion + 1)
	if err != nil {
		t.Fatal(err)
	}
	unsupported, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"legacy fields":       []byte(`{"addons":[],"vixBaseURL":"https://legacy.test"}`),
		"missing version":     unversioned,
		"unsupported version": unsupported,
		"removed built-in":    []byte(`{"configVersion":1,"sources":[]}`),
	} {
		t.Run(name, func(t *testing.T) {
			req = httptest.NewRequest("POST", "/api/settings", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w = httptest.NewRecorder()
			s.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatal("invalid config was accepted", w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(s.Config.Get(), saved) {
				t.Fatal("invalid API config changed saved settings")
			}
		})
	}
}
