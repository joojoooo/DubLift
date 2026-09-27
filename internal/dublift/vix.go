package dublift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var assignmentRE = regexp.MustCompile(`\bwindow\.masterPlaylist\s*=\s*\{`)
var paramsRE = regexp.MustCompile(`(?:\bparams|"params"|'params')\s*:\s*\{`)
var literalRE = regexp.MustCompile(`(?:"(\w+)"|'(\w+)'|\b(\w+))\s*:\s*("(?:\\[\s\S]|[^"\\])*"|'(?:\\[\s\S]|[^'\\])*'|\d+)`)
var fhdRE = regexp.MustCompile(`\bwindow\.canPlayFHD\s*=\s*true\b`)
var numericRE = regexp.MustCompile(`^[1-9]\d*$`)
var imdbRE = regexp.MustCompile(`^tt\d+$`)

func objectAt(s string, start int) (string, error) {
	depth := 0
	var quote byte
	for i := start; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if strings.HasPrefix(s[i:], "//") {
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				break
			}
			i += end
			continue
		}
		if strings.HasPrefix(s[i:], "/*") {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				break
			}
			i += end + 3
			continue
		}
		if c == '{' {
			depth++
		}
		if c == '}' {
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", errors.New("incomplete Vixsrc player configuration")
}
func decodeJS(raw string) (string, error) {
	var b strings.Builder
	s := raw[1 : len(raw)-1]
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			return "", errors.New("invalid string escape")
		}
		switch s[i] {
		case 'u', 'x':
			n := 4
			if s[i] == 'x' {
				n = 2
			}
			if i+n >= len(s) {
				return "", errors.New("invalid Unicode escape")
			}
			v, e := strconv.ParseUint(s[i+1:i+n+1], 16, 32)
			if e != nil {
				return "", e
			}
			b.WriteRune(rune(v))
			i += n
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), nil
}
func literals(s string) map[string]string {
	out := map[string]string{}
	for _, m := range literalRE.FindAllStringSubmatch(s, -1) {
		k := m[1] + m[2] + m[3]
		v := m[4]
		if v[0] == '"' || v[0] == '\'' {
			var e error
			v, e = decodeJS(v)
			if e != nil {
				continue
			}
		}
		out[k] = v
	}
	return out
}
func resolveURL(base, ref string) (string, error) {
	if strings.ContainsAny(ref, "\r\n\\") {
		return "", errors.New("invalid child URL")
	}
	b, e := httpURL(base)
	if e != nil {
		return "", e
	}
	r, e := url.Parse(ref)
	if e != nil {
		return "", errors.New("invalid child URL")
	}
	u := b.ResolveReference(r)
	if _, e = httpURL(u.String()); e != nil {
		return "", e
	}
	return u.String(), nil
}
func ExtractVixPlaylist(html, base string) (string, error) {
	m := assignmentRE.FindStringIndex(html)
	if m == nil {
		return "", errors.New("Vixsrc player has no master playlist")
	}
	master, e := objectAt(html, m[1]-1)
	if e != nil {
		return "", e
	}
	p := paramsRE.FindStringIndex(master)
	if p == nil {
		return "", errors.New("Vixsrc player has no playlist parameters")
	}
	block, e := objectAt(master, p[1]-1)
	if e != nil {
		return "", e
	}
	params := literals(block)
	fields := literals(strings.Replace(master, block, "{}", 1))
	if fields["url"] == "" || params["token"] == "" {
		return "", errors.New("Vixsrc playlist credentials missing")
	}
	if _, e = strconv.ParseUint(params["expires"], 10, 64); e != nil {
		return "", errors.New("Vixsrc expiry invalid")
	}
	raw, e := resolveURL(base+"/", fields["url"])
	if e != nil {
		return "", e
	}
	u, _ := url.Parse(raw)
	u.Fragment = ""
	if !strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") {
		u.Path = strings.TrimRight(u.Path, "/") + ".m3u8"
	}
	if fhdRE.MatchString(html) {
		params["h"] = "1"
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
func (n *Network) ResolveVix(ctx context.Context, base, kind, id string, season, episode int) (Origin, error) {
	path := "/movie/" + id
	if kind == "series" {
		path = fmt.Sprintf("/tv/%s/%d/%d", id, season, episode)
	}
	headers := http.Header{"User-Agent": {userAgent}, "Referer": {base + "/"}, "Origin": {base}, "Cache-Control": {"no-cache"}}
	for attempt := 0; attempt < 2; attempt++ {
		b, _, e := n.Fetch(ctx, Origin{fmt.Sprintf("%s/api%s?_=%d-%d", strings.TrimRight(base, "/"), path, time.Now().UnixMilli(), attempt), headers}, manifestLimit)
		if e != nil {
			return Origin{}, fmt.Errorf("Vixsrc API: %w", e)
		}
		var data struct {
			Src string `json:"src"`
		}
		if json.Unmarshal(b, &data) != nil || data.Src == "" {
			return Origin{}, errors.New("Vixsrc API returned no embed URL")
		}
		embed, e := resolveURL(base+"/", data.Src)
		if e != nil {
			return Origin{}, e
		}
		b, _, e = n.Fetch(ctx, Origin{embed, headers}, manifestLimit)
		if e != nil {
			var he *HTTPError
			if errors.As(e, &he) && he.Status == 410 && attempt == 0 {
				continue
			}
			return Origin{}, fmt.Errorf("Vixsrc embed: %w", e)
		}
		raw, e := ExtractVixPlaylist(string(b), base)
		return Origin{raw, headers}, e
	}
	return Origin{}, errors.New("Vixsrc embed expired")
}

type Content struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	BaseID  string `json:"-"`
	TMDB    string `json:"tmdb"`
	Season  int    `json:"season,omitempty"`
	Episode int    `json:"episode,omitempty"`
}

func ParseContent(kind, id string) (Content, error) {
	c := Content{Type: kind, ID: id}
	if kind == "tv" {
		c.Type = "series"
	}
	if c.Type != "movie" && c.Type != "series" {
		return c, errors.New("expected movie or series")
	}
	parts := strings.Split(strings.TrimPrefix(id, "tmdb:"), ":")
	c.BaseID = parts[0]
	if numericRE.MatchString(c.BaseID) {
		c.TMDB = c.BaseID
	} else if !imdbRE.MatchString(c.BaseID) {
		return c, errors.New("expected TMDB or IMDb ID")
	}
	if c.Type == "series" {
		if len(parts) != 3 {
			return c, errors.New("episodes require id:season:episode")
		}
		var e error
		c.Season, e = strconv.Atoi(parts[1])
		if e != nil || c.Season < 0 {
			return c, errors.New("invalid season")
		}
		c.Episode, e = strconv.Atoi(parts[2])
		if e != nil || c.Episode <= 0 {
			return c, errors.New("invalid episode")
		}
	} else if len(parts) != 1 {
		return c, errors.New("invalid movie ID")
	}
	return c, nil
}

// Cinemeta provides IMDb -> TMDB metadata without a key. A personal TMDB API
// bearer token supports explicit /find and reverse lookups when needed.
func (n *Network) ResolveTMDB(ctx context.Context, c Content, token string) (string, error) {
	if c.TMDB != "" {
		return c.TMDB, nil
	}
	if token != "" {
		b, _, e := n.Fetch(ctx, Origin{"https://api.themoviedb.org/3/find/" + c.BaseID + "?external_source=imdb_id", http.Header{"Authorization": {"Bearer " + token}}}, manifestLimit)
		if e != nil {
			return "", e
		}
		var v struct {
			Movie []struct {
				ID int `json:"id"`
			} `json:"movie_results"`
			TV []struct {
				ID int `json:"id"`
			} `json:"tv_results"`
		}
		if json.Unmarshal(b, &v) == nil {
			if c.Type == "movie" && len(v.Movie) > 0 {
				return strconv.Itoa(v.Movie[0].ID), nil
			}
			if c.Type == "series" && len(v.TV) > 0 {
				return strconv.Itoa(v.TV[0].ID), nil
			}
		}
	}
	b, _, e := n.Fetch(ctx, Origin{"https://v3-cinemeta.strem.io/meta/" + c.Type + "/" + c.BaseID + ".json", nil}, manifestLimit)
	if e != nil {
		return "", e
	}
	var v struct {
		Meta struct {
			MoviedbID json.RawMessage `json:"moviedb_id"`
			TMDBID    json.RawMessage `json:"tmdb_id"`
		} `json:"meta"`
	}
	if json.Unmarshal(b, &v) == nil {
		for _, r := range []json.RawMessage{v.Meta.MoviedbID, v.Meta.TMDBID} {
			id := strings.Trim(string(r), "\"")
			if numericRE.MatchString(id) {
				return id, nil
			}
		}
	}
	return "", errors.New("IMDb→TMDB lookup unavailable; set a TMDB API read token or use a tmdb: ID")
}
