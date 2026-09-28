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

type Addon struct {
	Name        string `json:"name"`
	ManifestURL string `json:"manifestURL"`
}

type Settings struct {
	Listen         string `json:"listen"`
	PublicURL      string `json:"publicURL"`
	PreferProxy    bool   `json:"preferProxy"`
	DirectPlayback bool   `json:"directPlayback"`
	// nil is infinite: prefer direct audio regardless of measured offset.
	DirectTolerance           *float64 `json:"directTolerance"`
	MinConfidence             float64  `json:"minConfidence"`
	SearchRadius              float64  `json:"searchRadius"`
	AlignmentSamples          int      `json:"alignmentSamples"`
	MaxItalianResults         int      `json:"maxItalianResults"`
	SourceCheckTimeoutSeconds int      `json:"sourceCheckTimeoutSeconds"`
	SourceCheckParallelism    int      `json:"sourceCheckParallelism"`
	BypassSourceChecks        bool     `json:"bypassSourceChecks"`
	StartImmediately          bool     `json:"startImmediately"`
	CacheMB                   int      `json:"cacheMB"`
	FFmpeg                    string   `json:"ffmpeg"`
	FFprobe                   string   `json:"ffprobe"`
	VixBaseURL                string   `json:"vixBaseURL"`
	TMDBToken                 string   `json:"tmdbToken"`
	Addons                    []Addon  `json:"addons"`
	SetupCompleted            bool     `json:"setupCompleted"`
}

func DefaultSettings() Settings {
	tolerance := .125
	return Settings{Listen: "0.0.0.0:7000", PublicURL: defaultPublicURL(), DirectPlayback: true, DirectTolerance: &tolerance, MinConfidence: .68, SearchRadius: 8, AlignmentSamples: 1, MaxItalianResults: 3, SourceCheckTimeoutSeconds: 20, SourceCheckParallelism: 3, StartImmediately: true, CacheMB: 256, FFmpeg: "ffmpeg", FFprobe: "ffprobe", VixBaseURL: "https://vixsrc.to", Addons: []Addon{}}
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
	if c.AlignmentSamples < 1 || c.AlignmentSamples > 12 {
		return errors.New("alignment samples must be between 1 and 12")
	}
	if c.MaxItalianResults < 1 || c.MaxItalianResults > 100 {
		return errors.New("maximum Italian results must be between 1 and 100")
	}
	if c.SourceCheckTimeoutSeconds < 1 || c.SourceCheckTimeoutSeconds > 120 {
		return errors.New("source check timeout must be between 1 and 120 seconds")
	}
	if c.SourceCheckParallelism < 1 || c.SourceCheckParallelism > 16 {
		return errors.New("parallel source checks must be between 1 and 16")
	}
	if c.DirectTolerance != nil && (math.IsNaN(*c.DirectTolerance) || math.IsInf(*c.DirectTolerance, 0) || *c.DirectTolerance < 0) {
		return errors.New("direct tolerance must be nonnegative, or null for infinite")
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
	if _, e := httpURL(c.VixBaseURL); e != nil {
		return e
	}
	if c.PublicURL != "" {
		if _, e := httpURL(c.PublicURL); e != nil {
			return e
		}
	}
	if len(c.Addons) > 20 {
		return errors.New("at most 20 upstream addons")
	}
	for _, a := range c.Addons {
		if _, e := httpURL(a.ManifestURL); e != nil {
			return e
		}
		if !strings.HasSuffix(strings.Split(a.ManifestURL, "?")[0], "/manifest.json") {
			return errors.New("upstream URL must end with /manifest.json")
		}
	}
	return nil
}

type Config struct {
	mu    sync.RWMutex
	path  string
	value Settings
}

func OpenConfig(path string) (*Config, error) {
	c := &Config{path: path, value: DefaultSettings()}
	b, e := os.ReadFile(path)
	if errors.Is(e, os.ErrNotExist) {
		return c, c.Save(c.value)
	}
	if e != nil {
		return nil, e
	}
	if e = json.Unmarshal(b, &c.value); e != nil {
		return nil, e
	}
	if e = c.value.Validate(); e != nil {
		return nil, e
	}
	if e = os.Chmod(path, 0600); e != nil {
		return nil, e
	}
	return c, nil
}
func (c *Config) Get() Settings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.value
	if s.DirectTolerance != nil {
		x := *s.DirectTolerance
		s.DirectTolerance = &x
	}
	s.Addons = append([]Addon{}, s.Addons...)
	return s
}
func (c *Config) Save(s Settings) error {
	if e := s.Validate(); e != nil {
		return e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := atomicJSON(c.path, s); e != nil {
		return e
	}
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
