package dublift

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist/primitives"
)

// FFmpeg can report MPEG-TS clocks near the 33-bit rollover as negative.
// Normalize once per section so crossing zero does not reset generated audio.
// The signed source clock remains unchanged for subtitles.
func (e *Engine) audioClock(ctx context.Context, a *Asset, clockBase float64, segment int) (float64, error) {
	if a.HLS == nil {
		return clockBase, nil
	}
	section := a.HLS.section(segment)
	seg := a.HLS.Segments[section]
	clock := clockBase + seg.Start
	if clock >= 0 {
		return clockBase, nil
	}
	if seg.Map != "" {
		// EXT-X-MAP can contain TS PAT/PMT packets as well as an MP4 init.
		// Inspect only a bounded prefix, and share it across section requests.
		prefix, err := e.Cache.Get(ctx, fmt.Sprintf("clock-init:%s:%d", a.ID, section), func() ([]byte, error) {
			return e.hlsInitPrefix(ctx, a, seg)
		})
		if err != nil {
			return 0, err
		}
		if len(prefix) < 189 || prefix[0] != 0x47 || prefix[188] != 0x47 {
			return clockBase, nil
		}
	}
	const period = float64(uint64(1)<<33) / 90000
	return clockBase + math.Ceil(-clock/period)*period, nil
}

func (e *Engine) hlsInitPrefix(ctx context.Context, a *Asset, seg HLSSegment) ([]byte, error) {
	var attrs primitives.Attributes
	if err := attrs.Unmarshal(strings.TrimPrefix(seg.Map, "#EXT-X-MAP:")); err != nil {
		return nil, err
	}
	o, err := a.HLS.Child(attrs["URI"])
	if err != nil {
		return nil, err
	}
	// Two TS packet starts identify PAT/PMT initialization. Round the limit
	// up to an AES block boundary to support encrypted initialization maps.
	const prefixLimit = 384
	start, length := int64(0), int64(prefixLimit)
	if raw := attrs["BYTERANGE"]; raw != "" {
		parts := strings.Split(raw, "@")
		size, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || size <= 0 || len(parts) > 2 {
			return nil, errors.New("invalid HLS initialization byte range")
		}
		length = min(length, size)
		if len(parts) == 2 {
			start, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil || start < 0 || start > math.MaxInt64-length {
				return nil, errors.New("invalid HLS initialization byte offset")
			}
		}
	}
	byteRange := fmt.Sprintf("bytes=%d-%d", start, start+length-1)
	resp, err := e.Net.request(ctx, o, "GET", byteRange)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	validationRange := byteRange
	if resp.StatusCode == 200 && start == 0 {
		// Some init endpoints ignore Range. Reading their bounded prefix is
		// safe only when no byte offset needs to be skipped.
		validationRange = ""
	}
	if err := validateMediaResponse(resp, validationRange); err != nil {
		return nil, err
	}
	prefix, err := io.ReadAll(io.LimitReader(resp.Body, length))
	e.Net.Bytes.Add(int64(len(prefix)))
	if err != nil {
		return nil, err
	}
	if seg.Key != "" {
		var keyAttrs primitives.Attributes
		if err := keyAttrs.Unmarshal(strings.TrimPrefix(seg.Key, "#EXT-X-KEY:")); err != nil {
			return nil, err
		}
		if keyAttrs["METHOD"] == "AES-128" {
			// AES-128 maps require an explicit IV; segment sequence numbers
			// only supply implicit IVs for media segments.
			ivText := strings.TrimPrefix(strings.TrimPrefix(keyAttrs["IV"], "0x"), "0X")
			if ivText == "" || len(ivText) > 32 || len(prefix)%aes.BlockSize != 0 {
				return nil, errors.New("invalid encrypted HLS initialization")
			}
			iv, err := hex.DecodeString(strings.Repeat("0", 32-len(ivText)) + ivText)
			if err != nil {
				return nil, errors.New("invalid HLS initialization IV")
			}
			keyOrigin, err := a.HLS.Child(keyAttrs["URI"])
			if err != nil {
				return nil, err
			}
			key, _, err := e.Net.Fetch(ctx, keyOrigin, aes.BlockSize)
			if err != nil {
				return nil, err
			}
			if len(key) != aes.BlockSize {
				return nil, errors.New("invalid HLS initialization key")
			}
			block, err := aes.NewCipher(key)
			if err != nil {
				return nil, err
			}
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(prefix, prefix)
		}
	}
	return prefix, nil
}
