package dublift

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeVideoCodec(t *testing.T) {
	const avcc = "\n00000000: 0142 c00a ffe1 0004 6742 c00a 0100 0268 00  .B......gB.....h.\n"
	const annexb = "\n00000000: 0000 0001 6742 c00a 0000 0168 00            ....gB.....h.\n"
	for _, tc := range []struct {
		name, codec, data, want string
	}{
		{"avcC", "h264", avcc, "avc1.42c00a"},
		{"Annex-B", "h264", annexb, "avc1.42c00a"},
		{"missing data", "h264", "", ""},
		{"invalid hex", "h264", "00000000: xyz  text", ""},
		{"truncated avcC", "h264", "00000000: 0142 c00a ff  .B...", ""},
		{"missing SPS", "h264", "00000000: 0000 0168 00  ...h.", ""},
		{"unsupported codec", "unknown", avcc, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Probe{Streams: []ProbeStream{{CodecType: "audio", CodecName: "aac"}, {CodecType: "video", CodecName: tc.codec, Extradata: tc.data}}}
			if got := probeVideoCodec(p); got != tc.want {
				t.Fatalf("codec = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProbeHLSVideoCodecMetadata(t *testing.T) {
	ffmpegAvailable(t)
	encoders := command(t, "ffmpeg", "-hide_banner", "-encoders")
	for _, encoder := range []string{"libx264", "libx265"} {
		t.Run(encoder, func(t *testing.T) {
			if !strings.Contains(string(encoders), encoder) {
				t.Skip(encoder + " encoder unavailable")
			}
			dir := t.TempDir()
			var previous string
			for _, format := range []string{"mpegts", "fmp4"} {
				args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x72:rate=10", "-t", "3", "-c:v", encoder, "-preset", "ultrafast", "-g", "10"}
				if encoder == "libx265" {
					args = append(args, "-x265-params", "pools=1:frame-threads=1:log-level=error")
				}
				args = append(args, "-hls_time", "1", "-hls_playlist_type", "vod", "-hls_segment_type", format, filepath.Join(dir, format+".m3u8"))
				command(t, "ffmpeg", args...)
				origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
				defer origin.Close()
				engine := testEngine(t)
				a, err := engine.OpenAsset(context.Background(), Origin{URL: origin.URL + "/" + format + ".m3u8"}, true)
				if err != nil {
					t.Fatal(err)
				}
				p, err := engine.Probe(context.Background(), a)
				if err != nil {
					t.Fatal(err)
				}
				codec := probeVideoCodec(p)
				if encoder == "libx264" && codec != "avc1.42c00a" {
					t.Fatalf("%s codec = %q", format, codec)
				}
				if encoder == "libx265" && !strings.HasPrefix(codec, "hvc1.1.") && !strings.HasPrefix(codec, "hev1.1.") {
					t.Fatalf("%s HEVC codec = %q", format, codec)
				}
				if previous != "" && strings.TrimPrefix(codec, "hev1.") != strings.TrimPrefix(previous, "hvc1.") {
					t.Fatalf("TS and fMP4 profiles differ: %s / %s", previous, codec)
				}
				previous = codec
			}
		})
	}
}
