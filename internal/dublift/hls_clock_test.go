package dublift

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAudioClockInspectsInitializationBytes(t *testing.T) {
	mp4, err := audioInit()
	if err != nil {
		t.Fatal(err)
	}
	// Only container identification matters here; real PAT/PMT packets are
	// exercised by TestGeneratedAudioWrappedHLSClock.
	ts := make([]byte, 376)
	ts[0], ts[188] = 0x47, 0x47
	for _, test := range []struct {
		name        string
		data        []byte
		transport   bool
		ranged      bool
		encrypted   bool
		ignoreRange bool
	}{
		{"ts", ts, true, false, false, false},
		{"ts-range", ts, true, true, false, false},
		{"ts-encrypted", ts, true, false, true, false},
		{"ts-ignored-range", ts, true, false, false, true},
		{"mp4", mp4, false, false, false, false},
		{"mp4-range", mp4, false, true, false, false},
		{"mp4-encrypted", mp4, false, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := bytes.Clone(test.data)
			key := []byte("0123456789abcdef")
			iv := make([]byte, aes.BlockSize)
			iv[15] = 1
			if test.encrypted {
				padding := aes.BlockSize - len(data)%aes.BlockSize
				data = append(data, bytes.Repeat([]byte{byte(padding)}, padding)...)
				block, err := aes.NewCipher(key)
				if err != nil {
					t.Fatal(err)
				}
				cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, data)
			}
			mapLine := `#EXT-X-MAP:URI="init.bin"`
			if test.ranged {
				mapLine += fmt.Sprintf(`,BYTERANGE="%d@37"`, len(data))
				data = append(bytes.Repeat([]byte{0xff}, 37), data...)
			}
			// A shared resource may be much larger than its initialization.
			data = append(data, make([]byte, 1<<20)...)
			var initRequests, keyRequests atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Required") != "header" || r.Header.Get("Cookie") != "session=fixture" {
					http.Error(w, "missing origin credentials", http.StatusForbidden)
					return
				}
				if r.URL.Path == "/key.bin" {
					keyRequests.Add(1)
					w.Write(key)
					return
				}
				initRequests.Add(1)
				if raw := r.Header.Get("Range"); raw == "" || strings.HasSuffix(raw, "-") {
					t.Error("initialization inspection needs a finite byte range", raw)
				}
				if test.ignoreRange {
					w.Write(data)
					return
				}
				http.ServeContent(w, r, "init.bin", time.Time{}, bytes.NewReader(data))
			}))
			defer origin.Close()
			e := testEngine(t)
			seg := HLSSegment{Start: 12, Map: mapLine, Discontinuity: true}
			if test.encrypted {
				seg.Key = `#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x1`
			}
			a := &Asset{ID: "source", HLS: &HLS{
				Origin:   Origin{URL: origin.URL + "/source.m3u8", Headers: http.Header{"X-Required": {"header"}, "Cookie": {"session=fixture"}}},
				Segments: []HLSSegment{{}, seg, {Start: 18}},
			}}
			const clockBase = -13.021333
			want := clockBase
			if test.transport {
				want += float64(uint64(1)<<33) / 90000
			}
			// Start after rollover, then retry the start of the same section.
			for _, n := range []int{2, 1, 2} {
				got, err := e.audioClock(t.Context(), a, clockBase, n)
				if err != nil || math.Abs(got-want) > .000001 {
					t.Fatalf("segment %d: clock %f, want %f: %v", n, got, want, err)
				}
			}
			wantKeys := int32(0)
			if test.encrypted {
				wantKeys = 1
			}
			if initRequests.Load() != 1 || keyRequests.Load() != wantKeys {
				t.Fatal("initialization inspection was not shared", initRequests.Load(), keyRequests.Load())
			}
			if got := e.Net.Bytes.Load(); got > 384+16 {
				t.Fatal("initialization inspection exceeded prefix budget", got)
			}
		})
	}
}
