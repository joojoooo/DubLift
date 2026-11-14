package dublift

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	standaloneInputLimit  = 4 << 20
	standaloneOutputLimit = 8 << 20
	standaloneCueLimit    = 20000
	standaloneCueBytes    = 64 << 10
	standaloneLineBytes   = 8 << 10
)

func subtitleLanguage(raw string) string {
	lang := language(strings.TrimSpace(strings.ReplaceAll(raw, "_", "-")))
	// Keep every language, including regional tags, while normalizing common
	// ISO 639-2 aliases supplied by subtitle providers.
	parts := strings.SplitN(lang, "-", 2)
	aliases := map[string]string{
		"fre": "fr", "fra": "fr", "french": "fr", "ger": "de", "deu": "de", "german": "de",
		"spa": "es", "spanish": "es", "por": "pt", "portuguese": "pt", "dut": "nl", "nld": "nl",
		"rus": "ru", "jpn": "ja", "chi": "zh", "zho": "zh", "kor": "ko", "ara": "ar",
		"pol": "pl", "tur": "tr", "ukr": "uk", "swe": "sv", "dan": "da", "nor": "no",
		"fin": "fi", "cze": "cs", "ces": "cs", "gre": "el", "ell": "el", "rum": "ro", "ron": "ro",
		"hun": "hu", "heb": "he", "hin": "hi", "ind": "id", "tha": "th", "vie": "vi",
	}
	parts[0] = language(parts[0])
	if alias := aliases[parts[0]]; alias != "" {
		parts[0] = alias
	}
	lang = strings.Join(parts, "-")
	if len(lang) == 0 || len(lang) > 63 {
		return "und"
	}
	for _, part := range strings.Split(lang, "-") {
		if len(part) == 0 || len(part) > 8 {
			return "und"
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				return "und"
			}
		}
	}
	return lang
}

func subtitleLanguageName(lang string) string {
	names := map[string]string{"en": "English", "it": "Italian", "fr": "French", "de": "German", "es": "Spanish", "pt": "Portuguese", "nl": "Dutch", "ru": "Russian", "ja": "Japanese", "zh": "Chinese", "ko": "Korean", "ar": "Arabic", "und": "Unknown language"}
	base, region, _ := strings.Cut(lang, "-")
	if name := names[base]; name != "" {
		if region != "" {
			name += " (" + region + ")"
		}
		return name
	}
	return lang
}

func sameSubtitleOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func subtitlePublicHeaders(h http.Header) http.Header {
	out := make(http.Header)
	for _, key := range []string{"Referer", "Origin", "User-Agent", "Accept", "Accept-Language"} {
		if values := h.Values(key); len(values) != 0 {
			out[key] = append([]string(nil), values...)
		}
	}
	return out
}

// Registration only inspects metadata. A standalone file must never enter
// OpenAsset, ffprobe, or the original-HLS subtitle playlist loader.
func (v *Session) registerStandaloneSubtitles() {
	seen, names := map[string]bool{}, map[string]int{}
	source := v.SourceName
	if source == "" {
		source = "Original"
	}
	video := v.stream.Origin()
	videoURL, _ := httpURL(video.URL)
	for _, sub := range v.stream.Subtitles[:min(200, len(v.stream.Subtitles))] {
		if len(sub.URL) > 8<<10 {
			continue
		}
		u, err := httpURL(sub.URL)
		if err != nil {
			continue
		}
		lang := subtitleLanguage(sub.Lang)
		headers := video.Headers.Clone()
		if !sameSubtitleOrigin(videoURL, u) {
			headers = subtitlePublicHeaders(headers)
		}
		// Explicit attachment headers belong to the subtitle origin, including
		// its own authentication. Video credentials only survive same-origin.
		for key, value := range sub.Headers {
			if !strings.ContainsAny(key+value, "\r\n") {
				headers.Set(key, value)
			}
		}
		// A shared URL can select different subtitle files through headers.
		id := "standalone-" + identity(sub.URL, lang, fmt.Sprint(headers))[:32]
		if seen[id] {
			continue
		}
		seen[id] = true
		name := subtitleLanguageName(lang) + " · " + source
		names[lang]++
		if names[lang] > 1 {
			name += fmt.Sprintf(" (%d)", names[lang])
		}
		v.tracks = append(v.tracks, Track{ID: id, Name: name, Lang: lang, Original: true, Subtitle: true, Standalone: &Origin{URL: sub.URL, Headers: headers}})
	}
}

func (n *Network) fetchStandaloneSubtitle(ctx context.Context, o Origin) ([]byte, error) {
	client := *n.Client
	redirect := client.CheckRedirect
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many subtitle redirects")
		}
		if _, err := httpURL(r.URL.String()); err != nil {
			return err
		}
		if redirect != nil {
			if err := redirect(r, via); err != nil {
				return err
			}
		}
		// Go's default redirect policy also permits subdomains and other ports.
		// Limit explicit credentials to the exact subtitle origin. The cookie
		// jar still supplies the target host's own cookies normally.
		if !sameSubtitleOrigin(via[0].URL, r.URL) {
			r.Header = subtitlePublicHeaders(r.Header)
		}
		return nil
	}
	resp, err := n.requestWithRetry(ctx, o, http.MethodGet, "", &client, false)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("subtitle download: %s", networkError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{resp.StatusCode}
	}
	if resp.ContentLength > standaloneInputLimit {
		return nil, errors.New("subtitle exceeds 4 MiB input limit")
	}
	data, err := readBounded(resp.Body, standaloneInputLimit)
	n.Bytes.Add(int64(len(data)))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, errors.New("subtitle read failed or exceeds 4 MiB input limit")
	}
	return data, nil
}

// The cached representation is a bounded series of binary cue records:
// start/end milliseconds (uint64), text/settings lengths (uint32), then UTF-8.
// Segment requests scan it without reparsing or allocating all the cues.
func normalizeStandaloneSubtitle(ctx context.Context, data []byte) ([]byte, error) {
	if len(data) > standaloneInputLimit {
		return nil, errors.New("subtitle exceeds 4 MiB input limit")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("subtitle encoding must be UTF-8 (optional UTF-8 BOM)")
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, r := range text {
		if r < 32 && r != '\n' && r != '\t' || r == 127 || r == '\ufeff' {
			return nil, errors.New("subtitle contains unsupported control characters or encoding")
		}
	}
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 1024), standaloneLineBytes+1)
	if !scanner.Scan() {
		return nil, errors.New("empty or unsupported subtitle file")
	}
	first := scanner.Text()
	vtt := first == "WEBVTT" || strings.HasPrefix(first, "WEBVTT ") || strings.HasPrefix(first, "WEBVTT\t")
	if vtt && strings.Contains(first, "-->") {
		return nil, errors.New("malformed WebVTT header")
	}
	header := vtt
	var block []string
	blockBytes, count := 0, 0
	// A non-nil empty result represents a valid empty WebVTT file.
	out := make([]byte, 0)
	flush := func() error {
		if len(block) == 0 {
			return nil
		}
		defer func() { block, blockBytes = block[:0], 0 }()
		if vtt && (block[0] == "NOTE" || strings.HasPrefix(block[0], "NOTE ") || strings.HasPrefix(block[0], "NOTE\t")) {
			return nil
		}
		if vtt && (block[0] == "STYLE" || block[0] == "REGION") {
			return errors.New("standalone WebVTT STYLE and REGION blocks are unsupported")
		}
		line := 0
		if !strings.Contains(block[0], "-->") {
			line++ // WebVTT cue identifier or SRT sequence number.
			if !vtt {
				if _, err := strconv.ParseUint(strings.TrimSpace(block[0]), 10, 32); err != nil {
					return errors.New("unsupported or malformed subtitle file (expected WebVTT or SRT)")
				}
			}
		}
		if len(block) <= line+1 {
			return errors.New("malformed subtitle cue")
		}
		left, right, ok := strings.Cut(block[line], "-->")
		fields := strings.Fields(right)
		if !ok || len(fields) == 0 {
			return errors.New("malformed subtitle timing")
		}
		start, e1 := standaloneTimestamp(strings.TrimSpace(left), vtt)
		end, e2 := standaloneTimestamp(fields[0], vtt)
		if e1 != nil || e2 != nil || end <= start {
			return errors.New("invalid subtitle timestamps")
		}
		settings := ""
		if len(fields) > 1 {
			if !vtt {
				return errors.New("SRT cue settings are unsupported")
			}
			for _, field := range fields[1:] {
				key, _, found := strings.Cut(field, ":")
				if !found || !strings.Contains("|vertical|line|position|size|align|region|", "|"+key+"|") {
					return errors.New("unsupported WebVTT cue setting")
				}
				for _, c := range field {
					if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(":%.,-", c)) {
						return errors.New("invalid WebVTT cue setting")
					}
				}
			}
			settings = " " + strings.Join(fields[1:], " ")
		}
		payload := strings.Join(block[line+1:], "\n")
		if err := standaloneCueText(payload); err != nil {
			return err
		}
		count++
		if count > standaloneCueLimit || len(out)+24+len(payload)+len(settings) > standaloneOutputLimit {
			return errors.New("subtitle exceeds normalized size or 20000-cue limit")
		}
		out = binary.LittleEndian.AppendUint64(out, uint64(start))
		out = binary.LittleEndian.AppendUint64(out, uint64(end))
		out = binary.LittleEndian.AppendUint32(out, uint32(len(payload)))
		out = binary.LittleEndian.AppendUint32(out, uint32(len(settings)))
		out = append(out, payload...)
		out = append(out, settings...)
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := scanner.Text()
		if len(line) > standaloneLineBytes {
			return nil, errors.New("subtitle line exceeds 8 KiB limit")
		}
		if header {
			// Header metadata, including X-TIMESTAMP-MAP, does not change the
			// standalone file's VOD timeline. Only the video section maps it.
			if line == "" {
				header = false
			} else if strings.Contains(line, "-->") {
				return nil, errors.New("WebVTT header must end with a blank line")
			} else if strings.ContainsAny(line, "<>") {
				return nil, errors.New("invalid WebVTT header metadata")
			}
		} else if strings.TrimSpace(line) == "" {
			if err := flush(); err != nil {
				return nil, err
			}
		} else {
			blockBytes += len(line) + 1
			if blockBytes > standaloneCueBytes || len(block) >= 1024 {
				return nil, errors.New("subtitle cue exceeds 64 KiB or 1024-line limit")
			}
			block = append(block, line)
		}
		if !scanner.Scan() {
			break
		}
	}
	if scanner.Err() != nil {
		return nil, errors.New("subtitle line exceeds 8 KiB limit")
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if count == 0 && !vtt {
		return nil, errors.New("unsupported or empty SRT subtitle file")
	}
	return out, nil
}

func standaloneTimestamp(s string, vtt bool) (int64, error) {
	invalid := errors.New("invalid subtitle timestamp")
	if len(s) > 16 {
		return 0, invalid
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 && !(vtt && len(parts) == 2) {
		return 0, invalid
	}
	var total int64
	for i, part := range parts {
		if i == len(parts)-1 {
			if len(part) != 6 || part[2] != '.' && (vtt || part[2] != ',') {
				return 0, invalid
			}
			part = part[:2] + part[3:]
		} else if len(part) < 2 || (i > 0 || len(parts) == 2) && len(part) != 2 {
			return 0, invalid
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, invalid
			}
		}
		n, err := strconv.ParseInt(part, 10, 32)
		if err != nil || i == len(parts)-1 && n >= 60000 || i < len(parts)-1 && (i > 0 || len(parts) == 2) && n >= 60 {
			return 0, invalid
		}
		if i == len(parts)-1 {
			total = total*60000 + n
		} else {
			total = total*60 + n
		}
	}
	if total > 7*24*60*60*1000 {
		return 0, invalid
	}
	return total, nil
}

func standaloneCueText(text string) error {
	if strings.Contains(text, "-->") {
		return errors.New("invalid subtitle cue text")
	}
	// Preserve ordinary emphasis without interpreting arbitrary HTML. WebVTT
	// voice/class/ruby annotations also remain text in a WebVTT response.
	for rest := text; ; {
		_, tail, found := strings.Cut(rest, "<")
		if !found {
			return nil
		}
		tag, next, closed := strings.Cut(tail, ">")
		if !closed || len(tag) > 256 {
			return errors.New("unsupported subtitle markup")
		}
		name := strings.TrimPrefix(tag, "/")
		base := strings.Fields(name)
		if len(base) == 0 {
			return errors.New("unsupported subtitle markup")
		}
		kind, _, _ := strings.Cut(base[0], ".")
		if kind != "b" && kind != "i" && kind != "u" && kind != "c" && kind != "v" && kind != "lang" && kind != "ruby" && kind != "rt" {
			return errors.New("unsupported subtitle markup (expected WebVTT text or basic emphasis)")
		}
		rest = next
	}
}

func (s *Server) standaloneSubtitles(ctx context.Context, t Track, start, duration, clockBase float64) ([]byte, error) {
	data, err := s.Engine.Cache.Get(ctx, "standalone-subtitle:"+t.ID, func() ([]byte, error) {
		body, err := s.Net.fetchStandaloneSubtitle(ctx, *t.Standalone)
		if err != nil {
			return nil, err
		}
		return normalizeStandaloneSubtitle(ctx, body)
	})
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "WEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:%d\n\n", int64(math.Round(clockBase*90000))&((1<<33)-1))
	for len(data) >= 24 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a := float64(binary.LittleEndian.Uint64(data)) / 1000
		z := float64(binary.LittleEndian.Uint64(data[8:])) / 1000
		n := int(binary.LittleEndian.Uint32(data[16:]))
		m := int(binary.LittleEndian.Uint32(data[20:]))
		payload, settings := data[24:24+n], data[24+n:24+n+m]
		data = data[24+n+m:]
		if z <= start || a >= start+duration {
			continue
		}
		if out.Len()+len(payload)+len(settings)+64 > standaloneOutputLimit {
			return nil, errors.New("subtitle segment exceeds 8 MiB output limit")
		}
		// HLS requires each overlapping cue's complete display time, even
		// when the cue starts or ends outside this segment (RFC 8216 §3.5).
		fmt.Fprintf(&out, "%s --> %s%s\n%s\n\n", vttTime(a), vttTime(z), settings, payload)
	}
	return []byte(out.String()), nil
}
