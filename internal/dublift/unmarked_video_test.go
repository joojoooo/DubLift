package dublift

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Keep the Matroska cues but remove the key flags from its SimpleBlocks.
// Some AV1 releases have exactly this mismatch, which makes FFmpeg's normal
// stream-copy path wait forever for a marked key packet.
func clearMatroskaKeyFlags(t *testing.T, data []byte) int {
	t.Helper()
	header, err := ebmlHeader(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	segment, err := ebmlHeader(data[header.end:], header.end)
	if err != nil || segment.id != 0x18538067 {
		t.Fatalf("bad Matroska segment: %v", err)
	}
	count := 0
	for off := segment.data; off < int64(len(data)); {
		element, err := ebmlHeader(data[off:], off)
		if err != nil || element.end < 0 || element.end > int64(len(data)) {
			t.Fatalf("bad Matroska element at %d: %v", off, err)
		}
		if element.id == 0x1f43b675 {
			for pos := element.data; pos < element.end; {
				block, err := ebmlHeader(data[pos:], pos)
				if err != nil || block.end < 0 || block.end > element.end {
					t.Fatalf("bad Matroska block at %d: %v", pos, err)
				}
				if block.id == 0xa3 {
					track, n, err := vint(data[block.data:block.end], false)
					if err != nil || block.data+int64(n)+2 >= block.end {
						t.Fatalf("bad SimpleBlock at %d: %v", pos, err)
					}
					flag := block.data + int64(n) + 2
					if track == 1 && data[flag]&0x80 != 0 {
						data[flag] &^= 0x80
						count++
					}
				}
				pos = block.end
			}
		}
		off = element.end
	}
	return count
}

func TestUnmarkedAV1VideoRemux(t *testing.T) {
	ffmpegAvailable(t)
	if !bytes.Contains(command(t, "ffmpeg", "-hide_banner", "-encoders"), []byte("libsvtav1")) {
		t.Skip("libsvtav1 encoder unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "unmarked.mkv")
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "20", "-c:v", "libsvtav1", "-preset", "12", "-g", "48", "-pix_fmt", "yuv420p", "-c:a", "libopus", path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := clearMatroskaKeyFlags(t, data); n < 2 {
		t.Fatalf("only %d key flags cleared", n)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer origin.Close()
	engine := testEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	asset, err := engine.OpenAsset(ctx, Origin{URL: origin.URL + "/unmarked.mkv"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(asset.Index.Boundaries) < 3 {
		t.Fatalf("too few indexed boundaries: %v", asset.Index.Boundaries)
	}
	bounds := asset.Index.Boundaries[:3]
	// FFmpeg may recover the key flag from a synthetic AV1 key OBU even when
	// the container omits it. Force the fallback to exercise its exact packet
	// splitting and clock path without depending on encoder internals.
	asset.unmarkedVideo.Store(true)
	var segments [][]byte
	if err := engine.VideoWindow(ctx, asset, bounds, func(_ int, data []byte) error {
		segments = append(segments, data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || !asset.unmarkedVideo.Load() {
		t.Fatalf("fallback published %d segments, active=%t", len(segments), asset.unmarkedVideo.Load())
	}
	for n, segment := range segments {
		out := filepath.Join(dir, fmt.Sprintf("segment-%d.mp4", n))
		if err := os.WriteFile(out, segment, 0600); err != nil {
			t.Fatal(err)
		}
		lo, hi := packetTimes(t, out, "v:0")
		if lo < bounds[n]+.9 || lo > bounds[n]+1.1 || hi < bounds[n+1]+.94 || hi > bounds[n+1]+1.1 {
			t.Fatalf("segment %d covers %.3f..%.3f, expected %.3f..%.3f", n, lo, hi, bounds[n]+1, bounds[n+1]+1)
		}
		command(t, "ffmpeg", "-v", "error", "-i", out, "-map", "0:v:0", "-frames:v", "2", "-f", "null", "-")
	}
	fresh, err := engine.OpenAsset(ctx, Origin{URL: origin.URL + "/unmarked.mkv"}, false)
	if err != nil {
		t.Fatal(err)
	}
	fresh.unmarkedVideo.Store(true)
	seeked, err := engine.Video(ctx, fresh, bounds[1], bounds[2]-bounds[1])
	if err != nil || len(seeked) == 0 || !fresh.unmarkedVideo.Load() {
		t.Fatalf("seek fallback: bytes=%d active=%t err=%v", len(seeked), fresh.unmarkedVideo.Load(), err)
	}
	out := filepath.Join(dir, "seeked.mp4")
	if err := os.WriteFile(out, seeked, 0600); err != nil {
		t.Fatal(err)
	}
	command(t, "ffmpeg", "-v", "error", "-i", out, "-map", "0:v:0", "-frames:v", "2", "-f", "null", "-")
}
