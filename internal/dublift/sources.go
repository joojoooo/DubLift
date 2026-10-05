package dublift

import (
	"errors"
	"fmt"
	"strings"
)

// Built-in definitions drive both config validation and dashboard cards. New
// scrapers can describe their URL and audio role here without changing the UI.
type sourceDefinition struct {
	Type         string `json:"type"`
	Name         string `json:"name"`
	Icon         string `json:"icon"`
	URLLabel     string `json:"urlLabel"`
	DefaultURL   string `json:"defaultURL"`
	ItalianAudio bool   `json:"italianAudio"`
}

var builtinSources = []sourceDefinition{
	{Type: "vixsrc", Name: "VixSrc", Icon: "/vixsrc.ico", URLLabel: "Vixsrc base URL", DefaultURL: "https://vixsrc.to", ItalianAudio: true},
}

type dashboardSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (source Source) dashboardSource() dashboardSource {
	name := source.Name
	if name == "" {
		name = "Addon source"
	}
	// Reordering and renaming keep the same filter. Never expose addon URLs,
	// which may contain credentials, as filter IDs or browser storage keys.
	return dashboardSource{ID: identity("source", source.Type, source.ManifestURL), Name: name}
}

func defaultSources() []Source {
	sources := make([]Source, 0, len(builtinSources))
	for _, def := range builtinSources {
		sources = append(sources, Source{Type: def.Type, Name: def.Name, BaseURL: def.DefaultURL})
	}
	return sources
}

func (c Settings) normalizedSources() Settings {
	c.Sources = append([]Source{}, c.Sources...)
	for i := range c.Sources {
		for _, def := range builtinSources {
			if c.Sources[i].Type == def.Type {
				c.Sources[i].Name = def.Name
			}
		}
	}
	return c
}

func validateSources(sources []Source) error {
	seen := map[string]bool{}
	addons := 0
	for _, source := range sources {
		if source.Type == "addon" {
			addons++
			if _, err := httpURL(source.ManifestURL); err != nil {
				return err
			}
			if !strings.HasSuffix(strings.Split(source.ManifestURL, "?")[0], "/manifest.json") {
				return errors.New("upstream URL must end with /manifest.json")
			}
			if source.BaseURL != "" {
				return errors.New("addon sources use a manifest URL")
			}
			continue
		}
		known := false
		for _, def := range builtinSources {
			if def.Type == source.Type {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("unknown source type %q", source.Type)
		}
		if seen[source.Type] {
			return fmt.Errorf("duplicate built-in source %q", source.Type)
		}
		seen[source.Type] = true
		if _, err := httpURL(source.BaseURL); err != nil {
			return err
		}
		if source.ManifestURL != "" {
			return errors.New("built-in sources use a base URL")
		}
	}
	if addons > 20 {
		return errors.New("at most 20 upstream addons")
	}
	for _, def := range builtinSources {
		if !seen[def.Type] {
			return fmt.Errorf("built-in source %s cannot be removed", def.Name)
		}
	}
	return nil
}

func (c Settings) Source(kind string) Source {
	for _, source := range c.Sources {
		if source.Type == kind {
			return source
		}
	}
	return Source{}
}

func (c Settings) hasEnabledSource() bool {
	for _, source := range c.Sources {
		if !source.Disabled {
			return true
		}
	}
	return false
}
