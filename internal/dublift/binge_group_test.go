package dublift

import (
	"context"
	"fmt"
	"testing"
)

func TestVixBingeGroups(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, group string
	}{
		{"1080p", "BANDWIDTH=4000000,RESOLUTION=1920x1080", "dublift:vixsrc:1080p"},
		{"1080p different bitrate", "BANDWIDTH=6500000,RESOLUTION=1920x1080", "dublift:vixsrc:1080p"},
		{"720p", "BANDWIDTH=2000000,RESOLUTION=1280x720", "dublift:vixsrc:720p"},
		{"bitrate only", "BANDWIDTH=1250000", "dublift:vixsrc:1.25-mbps"},
		{"low bitrate only", "BANDWIDTH=500000", "dublift:vixsrc:500-kbps"},
		{"auto", "", "dublift:vixsrc:auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for episode := 1; episode <= 2; episode++ {
				raw := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n#EXT-X-ENDLIST\n"
				if tc.metadata != "" {
					raw = fmt.Sprintf("#EXTM3U\n#EXT-X-STREAM-INF:%s\nepisode-%d.m3u8?token=private-%d\n", tc.metadata, episode, episode)
				}
				h, err := ParseHLS([]byte(raw), Origin{URL: fmt.Sprintf("https://origin-%d.test/episode-%d/master.m3u8?token=private-%d", episode, episode, episode)})
				if err != nil {
					t.Fatal(err)
				}
				qualities := vixQualityStreams(h, nil)
				if len(qualities) != 1 || qualities[0].stream.BehaviorHints.BingeGroup != tc.group {
					t.Fatalf("episode %d: expected group %q, got %+v", episode, tc.group, qualities)
				}
			}
		})
	}
}

func TestMovyBingeGroups(t *testing.T) {
	for _, tc := range []struct {
		quality, server, group string
	}{
		{"4K HDR", "seattle", "dublift:movy:2160p"},
		{"UHD HLS", "seattle", "dublift:movy:2160p"},
		{"2160p", "seattle", "dublift:movy:2160p"},
		{"FullHD", "seattle", "dublift:movy:1080p"},
		{" 1080P HLS ", "seattle", "dublift:movy:1080p"},
		{"1080", "seattle", "dublift:movy:1080p"},
		{"1080p", "seattle", "dublift:movy:1080p"},
		{"1080p", "denver", "dublift:movy:1080p"},
		{"Auto HLS", "seattle", "dublift:movy:auto"},
		{"Auto HLS", "denver", "dublift:movy:auto"},
		{"", "seattle", "dublift:movy:auto"},
		{"Episode 2 private-title https://signed.test/?session=secret", "seattle", "dublift:movy:auto"},
	} {
		t.Run(tc.quality+"/"+tc.server, func(t *testing.T) {
			for episode := 1; episode <= 2; episode++ {
				choices := NewNetwork().normalizeMovySources(context.Background(), movyPayload{Sources: []movySource{{
					URL: fmt.Sprintf("https://origin-%d.test/episode-%d.mp4?token=private-%d", episode, episode, episode), Quality: tc.quality,
				}}}, tc.server, "https://movy.test", fmt.Sprintf("https://movy.test/tv/123/1/%d?play=true", episode))
				if len(choices) != 1 || choices[0].stream.BehaviorHints.BingeGroup != tc.group {
					t.Fatalf("episode %d: expected group %q, got %+v", episode, tc.group, choices)
				}
			}
		})
	}
}
