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

func TestConfigV1ToV3Migration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	customSources := []Source{
		{Type: "vixsrc", Name: "VixSrc", BaseURL: "https://vix.custom", Disabled: false},
		{Type: "movy", Name: "Movy", BaseURL: "https://movy.custom", Disabled: true},
		{Type: "addon", Name: "Custom Addon", ManifestURL: "https://addon.custom/manifest.json", Disabled: false},
	}
	v1Fields := map[string]any{
		"configVersion":          1,
		"listen":                 "127.0.0.1:9500",
		"publicURL":              "http://192.168.1.55:7000",
		"minConfidence":          0.85,
		"searchRadius":           35.0,
		"alignmentSampleSeconds": 15,
		"alignmentSamples":       4,
		"cacheMB":                512,
		"ffmpeg":                 "/custom/ffmpeg",
		"ffprobe":                "/custom/ffprobe",
		"tmdbToken":              "custom-tmdb-token",
		"setupCompleted":         true,
		"sources":                customSources,
		"legacyField":            "should-be-cleaned",
	}
	data, err := json.Marshal(v1Fields)
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
	got := cfg.Get()
	if !got.NextEpisodePrefetch || got.NextEpisodeLeadMinutes != 5 {
		t.Fatal("v1 → v2 → v3 did not apply prefetch defaults")
	}
	if got.ConfigVersion != currentConfigVersion {
		t.Fatalf("expected current configVersion, got %d", got.ConfigVersion)
	}
	if got.PublicURL != "" {
		t.Fatalf("expected publicURL to be reset to empty string, got %q", got.PublicURL)
	}
	if got.Listen != "127.0.0.1:9500" {
		t.Fatalf("expected listen preserved, got %q", got.Listen)
	}
	if got.MinConfidence != 0.85 {
		t.Fatalf("expected minConfidence preserved, got %v", got.MinConfidence)
	}
	if got.SearchRadius != 35.0 {
		t.Fatalf("expected searchRadius preserved, got %v", got.SearchRadius)
	}
	if got.AlignmentSampleSeconds != 15 {
		t.Fatalf("expected alignmentSampleSeconds preserved, got %d", got.AlignmentSampleSeconds)
	}
	if got.AlignmentSamples != 4 {
		t.Fatalf("expected alignmentSamples preserved, got %d", got.AlignmentSamples)
	}
	if got.CacheMB != 512 {
		t.Fatalf("expected cacheMB preserved, got %d", got.CacheMB)
	}
	if got.FFmpeg != "/custom/ffmpeg" {
		t.Fatalf("expected ffmpeg preserved, got %q", got.FFmpeg)
	}
	if got.FFprobe != "/custom/ffprobe" {
		t.Fatalf("expected ffprobe preserved, got %q", got.FFprobe)
	}
	if got.TMDBToken != "custom-tmdb-token" {
		t.Fatalf("expected tmdbToken preserved, got %q", got.TMDBToken)
	}
	if !got.SetupCompleted {
		t.Fatal("expected setupCompleted preserved")
	}
	if !reflect.DeepEqual(got.Sources, customSources) {
		t.Fatalf("expected sources preserved: got %v, want %v", got.Sources, customSources)
	}

	// Verify the file was persisted to disk with updated version and permissions.
	persistedBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Settings
	if err := json.Unmarshal(persistedBytes, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.ConfigVersion != currentConfigVersion || persisted.PublicURL != "" {
		t.Fatalf("persisted file not properly migrated: %+v", persisted)
	}
	if bytes.Contains(persistedBytes, []byte(`"legacyField"`)) {
		t.Fatal("legacy fields survived migration save")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("migrated file does not have 0600 permissions: %v", err)
	}

	reopened, err := OpenConfig(path)
	if err != nil || !reflect.DeepEqual(reopened.Get(), got) {
		t.Fatalf("reopened migrated config mismatch: %v", err)
	}
}

func TestConfigMigrationChaining(t *testing.T) {
	// Test sequential execution of multiple migration steps.
	origMigrations := configMigrations
	defer func() { configMigrations = origMigrations }()

	configMigrations = map[int]configMigration{
		1: func(data map[string]any) error {
			data["step1"] = true
			data["publicURL"] = ""
			data["configVersion"] = 2
			return nil
		},
		2: func(data map[string]any) error {
			if data["step1"] != true {
				t.Fatal("step 2 executed before step 1")
			}
			data["step2"] = true
			data["configVersion"] = 3
			return nil
		},
	}

	data := map[string]any{"configVersion": 1, "publicURL": "http://old.test"}
	migrated, ok := migrateConfig(data, 1, 3)
	if !ok {
		t.Fatal("chained migration failed")
	}
	if migrated["step1"] != true || migrated["step2"] != true || migrated["configVersion"] != 3 || migrated["publicURL"] != "" {
		t.Fatalf("unexpected chained migration result: %v", migrated)
	}

	// Test missing intermediate step returns false.
	configMigrations = map[int]configMigration{
		1: func(data map[string]any) error {
			data["configVersion"] = 2
			return nil
		},
		// Missing step 2 -> 3
	}
	_, ok = migrateConfig(map[string]any{"configVersion": 1}, 1, 3)
	if ok {
		t.Fatal("expected failure when intermediate migration step is missing")
	}
}

func TestConfigV1CorruptReset(t *testing.T) {
	for name, fields := range map[string]map[string]any{
		"unparseable type": {
			"configVersion":    1,
			"alignmentSamples": "not-an-integer",
			"sources":          defaultSources(),
		},
		"invalid validation value": {
			"configVersion": 1,
			"cacheMB":       5, // invalid cacheMB (< 32)
			"sources":       defaultSources(),
		},
		"missing sources": {
			"configVersion": 1,
			"sources":       []Source{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
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
				t.Fatalf("corrupt v1 config did not reset to defaults: got %+v, want %+v", cfg.Get(), want)
			}
		})
	}

	t.Run("corrupt JSON syntax", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{not valid json`), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := OpenConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		want := DefaultSettings()
		if !reflect.DeepEqual(cfg.Get(), want) {
			t.Fatal("corrupt JSON syntax did not reset to defaults")
		}
	})
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

func TestConfigV2ToV3PrefetchMigrationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := DefaultSettings()
	original.ConfigVersion = 2
	original.PublicURL = "https://preserved.test"
	original.TMDBToken = "private-token"
	original.SetupCompleted = true
	original.CacheMB = 512
	original.Sources = append(original.Sources, Source{Type: "addon", Name: "Private", ManifestURL: "https://addon.test/key/manifest.json"})
	data, _ := json.Marshal(original)
	var fields map[string]any
	json.Unmarshal(data, &fields)
	delete(fields, "nextEpisodePrefetch")
	delete(fields, "nextEpisodeLeadMinutes")
	data, _ = json.Marshal(fields)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := OpenConfigWithOptions(path, ConfigOptions{AppFFmpeg: "/managed/ffmpeg"})
	if err != nil {
		t.Fatal(err)
	}
	original.ConfigVersion = 3
	original.NextEpisodePrefetch = true
	original.NextEpisodeLeadMinutes = 5
	original.FFmpeg = "/managed/ffmpeg"
	if !reflect.DeepEqual(cfg.Get(), original) {
		t.Fatalf("migration lost settings: %+v", cfg.Get())
	}
	disabled := cfg.Get()
	disabled.NextEpisodePrefetch = false
	disabled.NextEpisodeLeadMinutes = 30
	if err := cfg.Save(disabled); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenConfig(path)
	if err != nil || !reflect.DeepEqual(reopened.Get(), disabled) {
		t.Fatalf("explicitly disabled value lost: %v", err)
	}
	for _, lead := range []int{-1, 0, 31} {
		invalid := disabled
		invalid.NextEpisodeLeadMinutes = lead
		if invalid.Validate() == nil {
			t.Fatalf("accepted lead %d", lead)
		}
	}
	for _, lead := range []int{1, 5, 30} {
		valid := disabled
		valid.NextEpisodeLeadMinutes = lead
		if err := valid.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
