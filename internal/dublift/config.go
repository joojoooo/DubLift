package dublift

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Bump this when the config format changes. Unsupported versions reset to
// defaults; older supported versions execute sequential migrations.
const currentConfigVersion = 3

type Source struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	ManifestURL string `json:"manifestURL,omitempty"`
	BaseURL     string `json:"baseURL,omitempty"`
	Disabled    bool   `json:"disabled"`
}

type Settings struct {
	ConfigVersion          int      `json:"configVersion"`
	Listen                 string   `json:"listen"`
	PublicURL              string   `json:"publicURL"`
	MinConfidence          float64  `json:"minConfidence"`
	SearchRadius           float64  `json:"searchRadius"`
	AlignmentSampleSeconds int      `json:"alignmentSampleSeconds"`
	AlignmentSamples       int      `json:"alignmentSamples"`
	CacheMB                int      `json:"cacheMB"`
	FFmpeg                 string   `json:"ffmpeg"`
	FFprobe                string   `json:"ffprobe"`
	TMDBToken              string   `json:"tmdbToken"`
	Sources                []Source `json:"sources"`
	NextEpisodePrefetch    bool     `json:"nextEpisodePrefetch"`
	NextEpisodeLeadMinutes int      `json:"nextEpisodeLeadMinutes"`
	SetupCompleted         bool     `json:"setupCompleted"`
}

func DefaultSettings() Settings {
	return Settings{
		ConfigVersion:          currentConfigVersion,
		Listen:                 "0.0.0.0:7000",
		PublicURL:              "",
		MinConfidence:          .68,
		SearchRadius:           10,
		AlignmentSampleSeconds: 5,
		AlignmentSamples:       1,
		CacheMB:                256,
		NextEpisodePrefetch:    true,
		NextEpisodeLeadMinutes: 5,
		FFmpeg:                 "ffmpeg",
		FFprobe:                "ffprobe",
		Sources:                defaultSources(),
	}
}

func detectedLANURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return ""
	}
	if strings.EqualFold(host, "localhost") {
		return ""
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if ip.IsLoopback() {
			return ""
		}
		if !ip.IsUnspecified() {
			return "http://" + net.JoinHostPort(ip.String(), port)
		}
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return "http://" + net.JoinHostPort(host, port)
	}
	ip := detectedLANIP()
	if ip == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(ip, port)
}

func localManifestURLForListen(listen, port string) string {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		host = ip.String()
	}
	return "http://" + net.JoinHostPort(host, port) + "/manifest.json"
}

func listenPort(listen string) string {
	if _, p, err := net.SplitHostPort(listen); err == nil && p != "" {
		return p
	}
	if listen != "" && !strings.Contains(listen, ":") {
		return listen
	}
	return "7000"
}

func detectedLANIP() string {
	// A UDP dial selects the interface used for the default route without sending a packet.
	conn, err := net.Dial("udp4", "192.0.2.1:80")
	if err == nil {
		ip := conn.LocalAddr().(*net.UDPAddr).IP
		conn.Close()
		if ip.IsPrivate() {
			return ip.String()
		}
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if network, ok := addr.(*net.IPNet); ok && network.IP.To4() != nil && network.IP.IsPrivate() {
				return network.IP.String()
			}
		}
	}
	return ""
}

type configMigration func(data map[string]any) error

var configMigrations = map[int]configMigration{
	1: migrateV1ToV2,
	2: migrateV2ToV3,
}

func migrateV1ToV2(data map[string]any) error {
	data["publicURL"] = ""
	data["configVersion"] = 2
	return nil
}

func migrateV2ToV3(data map[string]any) error {
	data["nextEpisodePrefetch"] = true
	data["nextEpisodeLeadMinutes"] = 5
	data["configVersion"] = 3
	return nil
}

func migrateConfig(data map[string]any, fromVersion, toVersion int) (map[string]any, bool) {
	if fromVersion >= toVersion {
		return data, false
	}
	current := fromVersion
	for current < toVersion {
		migration, ok := configMigrations[current]
		if !ok {
			return nil, false
		}
		if err := migration(data); err != nil {
			return nil, false
		}
		current++
	}
	return data, true
}

func httpURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || strings.ContainsAny(raw, "\r\n") {
		return nil, errors.New("expected an HTTP(S) URL without embedded user credentials")
	}
	return u, nil
}

func (c Settings) Validate() error {
	if c.ConfigVersion != currentConfigVersion {
		return errors.New("unsupported config version; reload the dashboard")
	}
	if c.NextEpisodeLeadMinutes < 1 || c.NextEpisodeLeadMinutes > 30 {
		return errors.New("next episode lead time must be 1–30 minutes")
	}
	if c.AlignmentSampleSeconds < 5 || c.AlignmentSampleSeconds > 40 {
		return errors.New("alignment sample length must be 5–40 seconds")
	}
	if c.AlignmentSamples < 1 || c.AlignmentSamples > 12 {
		return errors.New("alignment samples must be between 1 and 12")
	}
	if math.IsNaN(c.MinConfidence) || c.MinConfidence < 0 || c.MinConfidence > 1 {
		return errors.New("minimum confidence must be between 0 and 1")
	}
	if math.IsNaN(c.SearchRadius) || c.SearchRadius < 5 || c.SearchRadius > 180 {
		return errors.New("search radius must be 5–180 seconds")
	}
	if c.CacheMB < 32 || c.CacheMB > 2048 {
		return errors.New("cache must be 32–2048 MiB")
	}
	if c.FFmpeg == "" || c.FFprobe == "" {
		return errors.New("FFmpeg and ffprobe paths are required")
	}
	if c.PublicURL != "" {
		if _, e := httpURL(c.PublicURL); e != nil {
			return e
		}
	}
	return validateSources(c.Sources)
}

type Config struct {
	saveMu    sync.Mutex
	mu        sync.RWMutex
	path      string
	value     Settings
	options   ConfigOptions
	listeners map[string]func(Settings, Settings)
}

// ConfigOptions carries process-supplied settings and defaults.
type ConfigOptions struct {
	AppListen      string
	AppFFmpeg      string
	AppFFprobe     string
	DefaultCacheMB int
}

func OpenConfig(path string) (*Config, error) {
	return OpenConfigWithOptions(path, ConfigOptions{})
}

func OpenConfigWithOptions(path string, options ConfigOptions) (*Config, error) {
	initial := DefaultSettings()
	options.applyDefaults(&initial)
	c := &Config{path: path, value: initial, options: options}
	b, e := os.ReadFile(path)
	if errors.Is(e, os.ErrNotExist) {
		return c, c.Save(c.value)
	}
	if e != nil {
		return nil, e
	}
	// Read the version before decoding settings: an unsupported format may use
	// incompatible field types and must be replaced as a whole.
	var raw map[string]any
	if e = json.Unmarshal(b, &raw); e != nil || raw == nil {
		return c, c.Save(c.value)
	}
	verVal, ok := raw["configVersion"]
	if !ok || verVal == nil {
		return c, c.Save(c.value)
	}
	verFloat, ok := verVal.(float64)
	if !ok || verFloat != math.Trunc(verFloat) || math.IsNaN(verFloat) || math.IsInf(verFloat, 0) {
		return c, c.Save(c.value)
	}
	version := int(verFloat)
	if version > currentConfigVersion || version <= 0 {
		return c, c.Save(c.value)
	}
	if version < currentConfigVersion {
		migrated, ok := migrateConfig(raw, version, currentConfigVersion)
		if !ok {
			return c, c.Save(c.value)
		}
		migratedBytes, err := json.Marshal(migrated)
		if err != nil {
			return c, c.Save(c.value)
		}
		var candidate Settings
		candidate.Sources = nil
		if err := json.Unmarshal(migratedBytes, &candidate); err != nil {
			return c, c.Save(c.value)
		}
		candidate = candidate.normalizedSources()
		options.applyManaged(&candidate)
		if err := candidate.Validate(); err != nil {
			return c, c.Save(c.value)
		}
		if err := c.Save(candidate); err != nil {
			return nil, err
		}
		return c, nil
	}
	c.value.Sources = nil
	if e = json.Unmarshal(b, &c.value); e != nil {
		return nil, e
	}
	c.value = c.value.normalizedSources()
	if options.applyManaged(&c.value) {
		if e = c.Save(c.value); e != nil {
			return nil, e
		}
		return c, nil
	}
	if e = c.value.Validate(); e != nil {
		return nil, e
	}
	if e = os.Chmod(path, 0600); e != nil {
		return nil, e
	}
	return c, nil
}

// ManagedOptions captures which settings are locked by the host process.
type ManagedOptions struct {
	AppListenManaged  bool `json:"appListenManaged"`
	AppFFmpegManaged  bool `json:"appFFmpegManaged"`
	AppFFprobeManaged bool `json:"appFFprobeManaged"`
}

func (o ConfigOptions) Managed() ManagedOptions {
	return ManagedOptions{
		AppListenManaged:  o.AppListen != "",
		AppFFmpegManaged:  o.AppFFmpeg != "",
		AppFFprobeManaged: o.AppFFprobe != "",
	}
}

func (o ConfigOptions) applyDefaults(settings *Settings) {
	o.applyManaged(settings)
	if o.DefaultCacheMB != 0 {
		settings.CacheMB = o.DefaultCacheMB
	}
}

func (o ConfigOptions) applyManaged(settings *Settings) bool {
	c1 := setManagedValue(&settings.Listen, o.AppListen)
	c2 := setManagedValue(&settings.FFmpeg, o.AppFFmpeg)
	c3 := setManagedValue(&settings.FFprobe, o.AppFFprobe)
	return c1 || c2 || c3
}

func setManagedValue(setting *string, managed string) bool {
	if managed == "" || *setting == managed {
		return false
	}
	*setting = managed
	return true
}

func (c *Config) Managed() ManagedOptions { return c.options.Managed() }

// PrepareCandidate prepares settings submitted by the dashboard before validation:
// normalizes sources, preserves setup completion, and applies host-managed overrides.
func (c *Config) PrepareCandidate(s *Settings) {
	*s = s.normalizedSources()
	s.SetupCompleted = c.Get().SetupCompleted
	c.options.applyManaged(s)
}

func (c *Config) Get() Settings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.value
	s.Sources = append([]Source{}, s.Sources...)
	return s
}
func (c *Config) Save(s Settings) error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.options.applyManaged(&s)
	s = s.normalizedSources()
	if e := s.Validate(); e != nil {
		return e
	}
	c.mu.Lock()
	if e := atomicJSON(c.path, s); e != nil {
		c.mu.Unlock()
		return e
	}
	s.Sources = append([]Source{}, s.Sources...)
	old := c.value
	c.value = s
	listeners := make([]func(Settings, Settings), 0, len(c.listeners))
	for _, changed := range c.listeners {
		listeners = append(listeners, changed)
	}
	c.mu.Unlock()
	for _, changed := range listeners {
		changed(old, s)
	}
	return nil
}
func atomicJSON(path string, v any) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".dublift-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(b, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), path)
}
func token() string {
	b := make([]byte, 18)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}

func (c *Config) subscribeSettings(changed func(Settings, Settings)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listeners == nil {
		c.listeners = map[string]func(Settings, Settings){}
	}
	id := token()
	c.listeners[id] = changed
	changed(c.value, c.value)
	return func() { c.mu.Lock(); delete(c.listeners, id); c.mu.Unlock() }
}
