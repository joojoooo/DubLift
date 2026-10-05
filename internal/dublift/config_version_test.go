package dublift

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigVersionReset(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version any
	}{
		{"missing", nil},
		{"zero", 0},
		{"older", -1},
		{"future", currentConfigVersion + 1},
		{"string", "1"},
		{"null", json.RawMessage("null")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			// None of these old values may survive the reset, including the wizard
			// state. Unsupported versions must reset even with incompatible types.
			fields := map[string]any{
				"listen": "127.0.0.1:9000", "publicURL": "https://old.test",
				"minConfidence": .9, "searchRadius": 60,
				"alignmentSampleSeconds": 30, "alignmentSamples": "old-format",
				"cacheMB": 512, "ffmpeg": "/old/ffmpeg", "ffprobe": "/old/ffprobe",
				"tmdbToken": "old-token", "setupCompleted": true,
				"sources":    []Source{{Type: "addon", Name: "Old", ManifestURL: "https://old.test/manifest.json"}, {Type: "vixsrc", BaseURL: "https://old-vix.test", Disabled: true}},
				"addons":     []map[string]string{{"name": "Legacy", "manifestURL": "https://legacy.test/manifest.json"}},
				"vixBaseURL": "https://legacy-vix.test",
			}
			if tc.version != nil {
				fields["configVersion"] = tc.version
			}
			data, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := OpenConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			want := DefaultSettings()
			if !reflect.DeepEqual(cfg.Get(), want) {
				t.Fatal("unsupported config did not reset completely")
			}
			data, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var persisted Settings
			if err := json.Unmarshal(data, &persisted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persisted, want) {
				t.Fatal("reset defaults and current version were not saved")
			}
			if bytes.Contains(data, []byte(`"addons"`)) || bytes.Contains(data, []byte(`"vixBaseURL"`)) {
				t.Fatal("legacy config fields survived the reset")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("reset config is not private", err)
			}
			reopened, err := OpenConfig(path)
			if err != nil || !reflect.DeepEqual(reopened.Get(), want) {
				t.Fatal("reset config could not be reopened", err)
			}
		})
	}
}

func TestCurrentConfigValidationDoesNotReset(t *testing.T) {
	for name, sources := range map[string][]Source{
		"missing sources":  nil,
		"missing built-in": {},
		"missing Movy":     {{Type: "vixsrc", Name: "VixSrc", BaseURL: "https://vix.test"}},
		"invalid URL":      {{Type: "vixsrc", Name: "VixSrc", BaseURL: "file:///old/video"}},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := DefaultSettings()
			cfg.Sources = sources
			cfg.SetupCompleted = true
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenConfig(path); err == nil {
				t.Fatal("invalid current config was accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("invalid current config was overwritten", err)
			}
		})
	}
}
