package dublift

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
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

func TestAddonBingeGroupsUseSimplePrefix(t *testing.T) {
	source := Source{Type: "addon", Name: "Addon", ManifestURL: "https://addon.test/private-credential/manifest.json"}
	prefix := "dublift:"
	for _, upstream := range []string{"Addon:4K", "  External.Mixed/4K:v1 · group  ", "   ", "dublift:movy:1080p", "a:\tb\nc", "Unicode:日本語"} {
		got := source.playerBingeGroup(upstream)
		if got != prefix+upstream || got == upstream {
			t.Fatalf("group was not namespaced intact: %q", got)
		}
		if strings.Contains(got, "addon.test") || strings.Contains(got, "private-credential") {
			t.Fatal("namespace leaked addon connection", got)
		}
		renamed := source
		renamed.Name = "Renamed"
		if renamed.playerBingeGroup(upstream) != got {
			t.Fatal("renaming changed binge matching")
		}
		another := source
		another.ManifestURL = "https://other.test/manifest.json"
		if another.playerBingeGroup(upstream) != got {
			t.Fatal("addon connection affected the simple prefix")
		}
	}
	if source.playerBingeGroup("") != "" {
		t.Fatal("empty group was invented")
	}
	for _, provider := range []string{"vixsrc", "movy"} {
		builtin := Source{Type: provider}
		group := builtinBingeGroup(provider, "1080p")
		if builtin.playerBingeGroup(group) != group {
			t.Fatal("built-in group was double-prefixed")
		}
	}
}

func TestAddonBingeGroupRewritePreservesUnknownFallbackFields(t *testing.T) {
	source := Source{Type: "addon", ManifestURL: "https://addon.test/manifest.json"}
	for _, raw := range []string{
		`{"name":"Torrent","infoHash":"0123456789abcdef","fileIdx":3,"sources":["tracker:test"],"behaviorHints":{"bingeGroup":"  Exact.Mixed/v1  ","videoSize":1234567890123456789,"proxyHeaders":{"request":{"Referer":"https://required.test/"}},"customHint":{"keep":true}},"subtitles":[{"id":"it","lang":"ita","url":"https://subs.test/1"}],"customField":{"unknown":17}}`,
		`{"externalUrl":"https://player.test/","behaviorHints":{"bingeGroup":"External:Exact_CASE","unknownHint":[1,2,3]},"unknown":true}`,
		`{"url":"https://origin.test/video.mkv","behaviorHints":{"bingeGroup":"dublift:already-upstream","custom":"value"}}`,
		`{"url":"https://origin.test/video.mkv","behaviorHints":{"bingeGroup":"","custom":"value"}}`,
		`{"url":"https://origin.test/video.mkv","behaviorHints":{"bingeGroup":null,"custom":"value"}}`,
		`{"url":"https://origin.test/video.mkv","behaviorHints":{"custom":"value"}}`,
		`{"url":"https://origin.test/video.mkv","behaviorHints":null}`,
		`{"url":"https://origin.test/video.mkv","unknown":true}`,
	} {
		var stream Stream
		if err := json.Unmarshal([]byte(raw), &stream); err != nil {
			t.Fatal(err)
		}
		group := source.playerBingeGroup(stream.BehaviorHints.BingeGroup)
		out := stream.withBingeGroup(group)
		encoded, _ := json.Marshal(out)
		want := jsonValue(t, []byte(raw)).(map[string]any)
		if stream.BehaviorHints.BingeGroup != "" {
			want["behaviorHints"].(map[string]any)["bingeGroup"] = group
		}
		if !reflect.DeepEqual(jsonValue(t, encoded), want) {
			t.Fatalf("rewrite changed other upstream fields: %s", encoded)
		}
		original, _ := json.Marshal(stream)
		if string(original) != raw {
			t.Fatal("rendering changed original stream data")
		}
	}
}

func TestAddonBingeGroupResponsePrefetchAndTicketReuse(t *testing.T) {
	f, earlier := cachedEpisodeFixture(t)
	source := f.parent.source
	want := source.playerBingeGroup(exactNextGroup)
	if earlier.BehaviorHints.BingeGroup != want {
		t.Fatal("normal discovery forwarded the upstream group", earlier.BehaviorHints.BingeGroup)
	}
	oldURL, _ := url.Parse(earlier.URL)
	id := strings.Split(strings.TrimPrefix(oldURL.Path, "/media/"), "/")[0]
	ticket, err := f.s.readPlayback(id, oldURL.Query().Get("resume"))
	if err != nil || ticket.Stream.BehaviorHints.BingeGroup != exactNextGroup {
		t.Fatal("ticket lost original exact matching value", err)
	}
	p := f.trigger(t, 300)
	waitNext(t, p)
	if !p.ready {
		t.Fatal("namespaced player results broke upstream prefetch matching")
	}
	warmed := p.sessions[p.expected]
	if warmed.stream.BehaviorHints.BingeGroup != exactNextGroup {
		t.Fatal("prefetch changed upstream matching value")
	}
	response := nextRequest(t, f.s, "GET", "/stream/series/tt2879552:1:3.json", "")
	var listing struct {
		Streams []Stream `json:"streams"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Streams) != 3 || listing.Streams[p.expected].BehaviorHints.BingeGroup != want {
		t.Fatal("prefetched results changed the player namespace", response.Body.String())
	}
	for range 3 {
		rendered := f.s.streamResponses(httptest.NewRequest("GET", "http://player.test/", nil), []*Session{warmed})
		if rendered[0].BehaviorHints.BingeGroup != want || warmed.stream.BehaviorHints.BingeGroup != exactNextGroup {
			t.Fatal("repeated rendering accumulated prefixes")
		}
	}
	if w := nextRequest(t, f.s, "GET", earlier.URL, ""); w.Code != 200 || !warmed.Playing || f.s.getSession(warmed.ID) != warmed {
		t.Fatal("namespacing broke prepared playback reuse", w.Code)
	}
	views := f.s.status(httptest.NewRequest("GET", "/api/status", nil))["sessions"].([]map[string]any)
	if len(views) != 1 || views[0]["bingeGroup"] != want {
		t.Fatal("dashboard does not show player-facing group", views)
	}
	resumed, err := f.s.readPlayback(warmed.ID, warmed.ticket)
	if err != nil || resumed.Stream.BehaviorHints.BingeGroup != exactNextGroup || !sameSource(resumed.Source, source) {
		t.Fatal("prepared ticket did not retain upstream matching identity", err)
	}
	restarted, err := NewServer(f.s.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.restorePlayback(warmed.ID, warmed.ticket)
	if err != nil {
		t.Fatal(err)
	}
	rendered := restarted.streamResponses(httptest.NewRequest("GET", "http://player.test/", nil), []*Session{restored})
	if rendered[0].BehaviorHints.BingeGroup != want || restored.stream.BehaviorHints.BingeGroup != exactNextGroup {
		t.Fatal("restart changed the namespace or original matching group")
	}
}
