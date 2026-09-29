package dublift

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

// Container headers alone do not establish HEVC's frame reorder delay. A
// remux can cover the right overall interval and decode successfully while
// assigning many B-frames the same presentation time. Compare every copied
// packet with its source, including windows that restart the demuxer.
func TestHEVCPacketTiming(t *testing.T) {
	ffmpegAvailable(t)
	if !bytes.Contains(command(t, "ffmpeg", "-hide_banner", "-encoders"), []byte("libx265")) {
		t.Skip("libx265 required for HEVC timing fixture")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	command(t, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24000/1001", "-t", "32", "-pix_fmt", "yuv420p10le", "-c:v", "libx265", "-preset", "fast", "-x265-params", "pools=1:frame-threads=1:bframes=4:b-adapt=0:keyint=48:min-keyint=48:scenecut=0:open-gop=1", source)
	type packet struct {
		PTS  string `json:"pts_time"`
		Hash string `json:"data_hash"`
	}
	packets := func(path string) []packet {
		t.Helper()
		b := command(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_packets", "-show_data_hash", "sha256", "-show_entries", "packet=pts_time,data_hash", "-of", "json", path)
		var result struct{ Packets []packet }
		if err := json.Unmarshal(b, &result); err != nil {
			t.Fatal(err)
		}
		return result.Packets
	}
	original := map[string]float64{}
	for _, p := range packets(source) {
		original[p.Hash], _ = strconv.ParseFloat(p.PTS, 64)
	}
	origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer origin.Close()
	e := testEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	a, err := e.OpenAsset(ctx, Origin{URL: origin.URL + "/source.mkv"}, false)
	if err != nil {
		t.Fatal(err)
	}
	check := func(data []byte) {
		t.Helper()
		path := filepath.Join(dir, "remux.mp4")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		copied := packets(path)
		if len(copied) < 30 {
			t.Fatalf("only %d copied video packets", len(copied))
		}
		for i, p := range copied {
			want, ok := original[p.Hash]
			if !ok {
				t.Fatalf("packet %d was changed by remuxing", i)
			}
			got, _ := strconv.ParseFloat(p.PTS, 64)
			if math.Abs(got-want-1) > .002 {
				t.Fatalf("packet %d presentation time %.6f, want %.6f; frame reorder timing was lost", i, got, want+1)
			}
		}
	}
	for _, n := range []int{0, 1, 3} {
		bounds := a.Index.Boundaries[n : n+3]
		var segments [][]byte
		if err := e.VideoWindow(ctx, a, bounds, func(_ int, data []byte) error {
			segments = append(segments, data)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(segments) != len(bounds)-1 {
			t.Fatal("remux omitted a video segment")
		}
		for i, data := range segments {
			check(data)
			_, media, err := splitFMP4(data)
			if err != nil {
				t.Fatal(err)
			}
			var parts fmp4.Parts
			if err := parts.Unmarshal(media); err != nil {
				t.Fatal(err)
			}
			if len(parts) != 1 || parts[0].SequenceNumber != uint32(n+i+1) {
				t.Fatalf("segment %d lost its absolute fragment sequence", n+i)
			}
		}
	}
}
