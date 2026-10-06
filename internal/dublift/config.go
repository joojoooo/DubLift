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
// defaults; there are currently no config migrations.
const currentConfigVersion = 1

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
	SetupCompleted         bool     `json:"setupCompleted"`
}

func DefaultSettings() Settings {
	return Settings{ConfigVersion: currentConfigVersion, Listen: "0.0.0.0:7000", PublicURL: defaultPublicURL(), MinConfidence: .68, SearchRadius: 10, AlignmentSampleSeconds: 5, AlignmentSamples: 1, CacheMB: 256, FFmpeg: "ffmpeg", FFprobe: "ffprobe", Sources: defaultSources()}
}

func defaultPublicURL() string {
	// A UDP dial selects the interface used for the default route without sending a packet.
	conn, err := net.Dial("udp4", "192.0.2.1:80")
	if err == nil {
		ip := conn.LocalAddr().(*net.UDPAddr).IP
		conn.Close()
		if ip.IsPrivate() {
			return "http://" + ip.String() + ":7000"
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
				return "http://" + network.IP.String() + ":7000"
			}
		}
	}
	return ""
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
	mu      sync.RWMutex
	path    string
	value   Settings
	options ConfigOptions
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
	var header struct {
		ConfigVersion json.RawMessage `json:"configVersion"`
	}
	if e = json.Unmarshal(b, &header); e != nil {
		return nil, e
	}
	var version int
	if e = json.Unmarshal(header.ConfigVersion, &version); e != nil || version != currentConfigVersion {
		return c, c.Save(c.value)
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
	c.options.applyManaged(&s)
	s = s.normalizedSources()
	if e := s.Validate(); e != nil {
		return e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := atomicJSON(c.path, s); e != nil {
		return e
	}
	s.Sources = append([]Source{}, s.Sources...)
	c.value = s
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
