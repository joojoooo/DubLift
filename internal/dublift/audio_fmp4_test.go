package dublift

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

func audioPart(t *testing.T, data []byte) *fmp4.Part {
	t.Helper()
	var parts fmp4.Parts
	if err := parts.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || len(parts[0].Tracks) != 1 || len(parts[0].Tracks[0].Samples) == 0 {
		t.Fatal("expected one nonempty audio fragment")
	}
	return parts[0]
}

func assertContinuousAudioFragments(t *testing.T, segments ...[]byte) {
	t.Helper()
	var end uint64
	var sequence uint32
	for i, data := range segments {
		part := audioPart(t, data)
		track := part.Tracks[0]
		if i > 0 && (track.BaseTime != end || part.SequenceNumber != sequence+1) {
			t.Fatalf("audio fragment %d: clock %d, want %d; sequence %d after %d", i, track.BaseTime, end, part.SequenceNumber, sequence)
		}
		end, sequence = track.BaseTime, part.SequenceNumber
		for _, sample := range track.Samples {
			if sample.Duration != 1024 || sample.PTSOffset != 0 || len(sample.Payload) == 0 {
				t.Fatal("invalid AAC sample")
			}
			end += uint64(sample.Duration)
		}
	}
}

func TestAudioBoundaryIgnoresSubsampleRoundingNoise(t *testing.T) {
	// This shared boundary falls halfway between two AAC frames. Subtracting
	// and adding a segment start may move its float representation by one ULP.
	at := 281.5 * 1024 / 48000
	for _, boundary := range []float64{math.Nextafter(at, math.Inf(-1)), at, math.Nextafter(at, math.Inf(1))} {
		if got := audioBoundary(boundary); got != 282*1024 {
			t.Fatalf("boundary %.17g moved to sample %d", boundary, got)
		}
	}
}

func TestGeneratedAudioFMP4RequestsAreIndependent(t *testing.T) {
	ffmpegAvailable(t)
	s := lifecycleServer(t)
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: "https://example.test/source.mp4"})
	v.video = &Asset{ID: "source"}
	v.clockBase = 1.4
	v.boundaries = []float64{0, .057, .913, 2.001, 3.373, 6.113}
	// A source gap exercises real AAC encoding without origin dependencies.
	v.tracks = []Track{{ID: "audio", Original: true, Asset: v.video}}
	v.prepare.Do(func() { close(v.ready) })
	base := "/media/" + v.ID + "/track/audio/"
	get := func(method, name, byteRange string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, base+name, nil)
		r.Header.Set("Range", byteRange)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	// Init must work before any media request, with no encoder or source reads.
	cfg := s.Config.Get()
	savedFFmpeg := cfg.FFmpeg
	cfg.FFmpeg = filepath.Join(t.TempDir(), "must-not-run")
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	init := get("GET", "init.mp4", "")
	if init.Code != 200 || init.Header().Get("Content-Type") != "audio/mp4" {
		t.Fatal(init.Code, init.Header(), init.Body.String())
	}
	var movie fmp4.Init
	if err := movie.Unmarshal(bytes.NewReader(init.Body.Bytes())); err != nil {
		t.Fatal(err)
	}
	if len(movie.Tracks) != 1 || movie.Tracks[0].ID != 1 || movie.Tracks[0].TimeScale != 48000 {
		t.Fatalf("invalid audio init: %+v", movie.Tracks)
	}
	aac, ok := movie.Tracks[0].Codec.(*codecs.MPEG4Audio)
	if !ok || aac.Config.SampleRate != 48000 || aac.Config.ChannelConfig != 2 || aac.Config.Type != 2 {
		t.Fatalf("unexpected audio codec: %+v", movie.Tracks[0].Codec)
	}
	if v.Position != 0 || v.Playing || s.Engine.Cache.Used() != 0 {
		t.Fatal("initialization selected playback or generated media")
	}
	cfg.FFmpeg = savedFFmpeg
	if err := s.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	// Cold parallel requests include retries, backward seeks and forward seeks.
	order := []int{3, 1, 3, 0, 4, 2, 1, 4, 0}
	responses := make([]*httptest.ResponseRecorder, len(order))
	var wg sync.WaitGroup
	for i, n := range order {
		wg.Go(func() { responses[i] = get("GET", fmt.Sprintf("%d.m4s", n), "") })
	}
	wg.Wait()
	segments := make([][]byte, len(v.boundaries)-1)
	for i, response := range responses {
		if response.Code != 200 || response.Header().Get("Content-Type") != "audio/mp4" {
			t.Fatal(response.Code, response.Header(), response.Body.String())
		}
		n := order[i]
		if segments[n] != nil && !bytes.Equal(segments[n], response.Body.Bytes()) {
			t.Fatalf("parallel retry changed segment %d", n)
		}
		segments[n] = response.Body.Bytes()
	}
	assertContinuousAudioFragments(t, segments...)
	for n, segment := range segments {
		part := audioPart(t, segment)
		want := v.boundaries[n] + v.clockBase
		actual := float64(part.Tracks[0].BaseTime) / 48000
		if part.SequenceNumber != uint32(n+1) || actual < want-.011 || actual > want+.011 {
			t.Fatalf("segment %d clock/sequence: %.6f/%d, expected %.6f/%d", n, actual, part.SequenceNumber, want, n+1)
		}
		name := fmt.Sprintf("%d.m4s", n)
		if retry := get("GET", name, ""); retry.Code != 200 || !bytes.Equal(segment, retry.Body.Bytes()) {
			t.Fatalf("sequential retry changed segment %d", n)
		}
		if head := get("HEAD", name, ""); head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(segment)) {
			t.Fatalf("HEAD differs for segment %d: %d %v", n, head.Code, head.Header())
		}
		if partial := get("GET", name, "bytes=8-31"); partial.Code != 206 || !bytes.Equal(partial.Body.Bytes(), segment[8:32]) {
			t.Fatalf("range differs for segment %d: %d", n, partial.Code)
		}
	}
	if got := get("GET", "0.ts", ""); got.Code != 404 {
		t.Fatal("removed MPEG-TS audio endpoint still accepts requests", got.Code)
	}
	for _, req := range []struct{ method, byteRange string }{{"GET", ""}, {"HEAD", ""}, {"GET", "bytes=8-31"}} {
		response := get(req.method, "init.mp4", req.byteRange)
		if req.method == "HEAD" {
			if response.Code != 200 || response.Body.Len() != 0 {
				t.Fatal("invalid init HEAD response")
			}
		} else if req.byteRange != "" {
			if response.Code != 206 || !bytes.Equal(response.Body.Bytes(), init.Body.Bytes()[8:32]) {
				t.Fatal("invalid init range response")
			}
		} else if !bytes.Equal(response.Body.Bytes(), init.Body.Bytes()) {
			t.Fatal("init changed after media requests")
		}
	}
	s.Engine.Cache.Clear()
	if rebuilt := get("GET", "4.m4s", ""); rebuilt.Code != 200 || !bytes.Equal(rebuilt.Body.Bytes(), segments[4]) {
		t.Fatal("audio changed after cache eviction", rebuilt.Code)
	}
	// Check the entire contiguous result with an independent demuxer/decoder.
	media := append([]byte(nil), init.Body.Bytes()...)
	for _, segment := range segments {
		media = append(media, segment...)
	}
	path := filepath.Join(t.TempDir(), "audio.mp4")
	if err := os.WriteFile(path, media, 0600); err != nil {
		t.Fatal(err)
	}
	command(t, "ffmpeg", "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
}

func TestGeneratedTrackPlaylistsUseAudioInitAndPreserveDiscontinuities(t *testing.T) {
	s := &Server{}
	v := &Session{boundaries: []float64{0, 6, 12}, video: &Asset{HLS: &HLS{Segments: []HLSSegment{
		{Sequence: 17}, {Sequence: 18, Discontinuity: true},
	}}}}
	for _, subtitle := range []bool{false, true} {
		track := &Track{ID: "rendition", Subtitle: subtitle}
		playlist := s.generatedPlaylist(v, track)
		if !strings.Contains(playlist, "#EXT-X-MEDIA-SEQUENCE:17\n") || strings.Count(playlist, "#EXT-X-DISCONTINUITY\n") != 1 {
			t.Fatal("source timeline lost", playlist)
		}
		if subtitle {
			if strings.Contains(playlist, "#EXT-X-MAP:") || !strings.Contains(playlist, "rendition/0.vtt\n") {
				t.Fatal("subtitle format changed", playlist)
			}
		} else if strings.Count(playlist, "#EXT-X-MAP:") != 1 || !strings.Contains(playlist, "#EXT-X-MAP:URI=\"rendition/init.mp4\"\n") || !strings.Contains(playlist, "rendition/0.m4s\n") || strings.Contains(playlist, ".ts\n") {
			t.Fatal("missing fMP4 audio map/segments", playlist)
		}
	}
}

func TestGeneratedAudioShortFinalGOP(t *testing.T) {
	ffmpegAvailable(t)
	dir := t.TempDir()
	command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=60", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "2.016667", "-c:v", "libx264", "-preset", "ultrafast", "-g", "120", "-keyint_min", "120", "-sc_threshold", "0", "-c:a", "aac", "-movflags", "+faststart", filepath.Join(dir, "source.mp4"))
	origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer origin.Close()
	s := lifecycleServer(t)
	v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: origin.URL + "/source.mp4"})
	v.listedVix = []Track{} // Keep preparation entirely local.
	local := httptest.NewServer(s)
	defer local.Close()
	getBytes(t, local.URL+"/media/"+v.ID+"/master.m3u8")
	if len(v.boundaries) != 3 || v.boundaries[1] != 2 || math.Abs(v.boundaries[2]-2.017) > .001 {
		t.Fatal("fixture lost its short final GOP", v.boundaries)
	}
	base := local.URL + "/media/" + v.ID + "/track/" + v.tracks[0].ID
	playlist := getBytes(t, base+".m3u8")
	if !bytes.Contains(playlist, []byte("/1.m4s\n")) {
		t.Fatal("final segment missing from playlist", string(playlist))
	}
	first, last := getBytes(t, base+"/0.m4s"), getBytes(t, base+"/1.m4s")
	assertContinuousAudioFragments(t, first, last)
	if got := len(audioPart(t, last).Tracks[0].Samples); got != 1 {
		t.Fatalf("short final GOP has %d AAC frames, want one", got)
	}
	if retry := getBytes(t, base+"/1.m4s"); !bytes.Equal(last, retry) {
		t.Fatal("short final segment changed on retry")
	}
	media := getBytes(t, base+"/init.mp4")
	media = append(media, first...)
	media = append(media, last...)
	path := filepath.Join(dir, "audio.mp4")
	if err := os.WriteFile(path, media, 0600); err != nil {
		t.Fatal(err)
	}
	command(t, "ffmpeg", "-v", "error", "-xerror", "-i", path, "-f", "null", "-")
}

func TestGeneratedAudioWrappedHLSClock(t *testing.T) {
	ffmpegAvailable(t)
	for _, test := range []struct {
		offset int
		mapped bool
	}{{-1, false}, {0, false}, {-1, true}, {0, true}} {
		t.Run(fmt.Sprintf("offset=%d/map=%t", test.offset, test.mapped), func(t *testing.T) {
			dir := t.TempDir()
			for i, timestamp := range []string{strconv.Itoa(test.offset), strconv.Itoa(test.offset + 6)} {
				command(t, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-g", "144", "-bf", "0", "-c:a", "aac", "-metadata:s:a:0", "language=eng", "-avoid_negative_ts", "disabled", "-output_ts_offset", timestamp, "-mpegts_copyts", "1", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts", filepath.Join(dir, fmt.Sprintf("segment-%d.ts", i)))
			}
			// Each section crosses the MPEG-TS rollover. The second also exercises
			// sectionClock after an explicit reset to the same negative probe clock.
			playlist := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\nsegment-0.ts\n#EXTINF:6,\nsegment-1.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:6,\nsegment-0.ts\n#EXTINF:6,\nsegment-1.ts\n#EXT-X-ENDLIST\n"
			if test.mapped {
				// TS initialization maps carry PAT/PMT packets, not MP4 boxes.
				first, err := os.ReadFile(filepath.Join(dir, "segment-0.ts"))
				if err != nil {
					t.Fatal(err)
				}
				var init []byte
				for i := 0; i+188 <= len(first) && len(init) < 376; i += 188 {
					pid := uint16(first[i+1]&0x1f)<<8 | uint16(first[i+2])
					if pid == 0 || pid == 4096 {
						init = append(init, first[i:i+188]...)
					}
				}
				if len(init) != 376 {
					t.Fatal("fixture has no PAT/PMT initialization")
				}
				if err := os.WriteFile(filepath.Join(dir, "init.bin"), init, 0600); err != nil {
					t.Fatal(err)
				}
				playlist = strings.Replace(playlist, "#EXT-X-VERSION:3\n", "#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"init.bin\"\n", 1)
			}
			if err := os.WriteFile(filepath.Join(dir, "source.m3u8"), []byte(playlist), 0600); err != nil {
				t.Fatal(err)
			}
			origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
			defer origin.Close()
			s := lifecycleServer(t)
			asset, err := s.Engine.OpenAsset(t.Context(), Origin{URL: origin.URL + "/source.m3u8"}, false)
			if err != nil {
				t.Fatal(err)
			}
			probe, err := s.Engine.Probe(t.Context(), asset)
			if err != nil {
				t.Fatal(err)
			}
			clock, err := strconv.ParseFloat(probe.Format.StartTime, 64)
			if err != nil || clock >= 0 {
				t.Fatal("fixture lost its negative presentation clock", probe.Format.StartTime, err)
			}
			subPlaylist := "#EXTM3U\n#EXT-X-TARGETDURATION:24\n#EXTINF:24,\nsub.vtt\n#EXT-X-ENDLIST\n"
			if err := os.WriteFile(filepath.Join(dir, "sub.m3u8"), []byte(subPlaylist), 0600); err != nil {
				t.Fatal(err)
			}
			subtitle := fmt.Sprintf("WEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:%d\n\n00:00:01.000 --> 00:00:02.000\nOriginal cue.\n", int64(math.Round(clock*90000))&((1<<33)-1))
			if err := os.WriteFile(filepath.Join(dir, "sub.vtt"), []byte(subtitle), 0600); err != nil {
				t.Fatal(err)
			}
			subs, err := s.loadSubtitle(t.Context(), Origin{URL: origin.URL + "/sub.m3u8"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "official"), []byte("\ufeffWEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:888888\n\n00:05.000 --> 00:19.000\nMovy VOD cue.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: origin.URL + "/source.m3u8", Subtitles: []Subtitle{{URL: origin.URL + "/official", Lang: "en"}}})
			v.listedVix = []Track{
				{ID: "italian", Name: "Italian · Vixsrc", Lang: "it", Asset: asset, Selector: "0:a:0"},
				{ID: "original-subs", Name: "English", Lang: "en", Asset: subs, Original: true, Subtitle: true},
			}
			local := httptest.NewServer(s)
			defer local.Close()
			master := local.URL + "/media/" + v.ID + "/master.m3u8"
			getBytes(t, master)
			const period = float64(uint64(1)<<33) / 90000
			if math.Abs(v.clockBase-clock) > .001 {
				t.Fatalf("signed source clock changed from %.6f to %.6f", clock, v.clockBase)
			}
			originalSubs := getBytes(t, local.URL+"/media/"+v.ID+"/track/original-subs/0.vtt")
			if !bytes.Contains(originalSubs, []byte("00:00:01.000 --> 00:00:02.000\nOriginal cue.")) {
				t.Fatal("wrapped audio clock changed original subtitle timing", string(originalSubs))
			}
			var official Track
			for _, track := range v.tracks {
				if track.Standalone != nil {
					official = track
				}
			}
			for _, n := range []int{2, 0, 3, 1, 0} {
				base, err := s.sectionClock(v.ctx, v, n)
				if err != nil {
					t.Fatal(err)
				}
				data := getBytes(t, fmt.Sprintf("%s/media/%s/track/%s/%d.vtt", local.URL, v.ID, official.ID, n))
				const want = "00:00:05.000 --> 00:00:19.000\nMovy VOD cue."
				stamp := fmt.Sprintf("MPEGTS:%d", int64(math.Round(base*90000))&((1<<33)-1))
				if !bytes.Contains(data, []byte(want)) || !bytes.Contains(data, []byte(stamp)) {
					t.Fatalf("standalone cue lost VOD timing in section %d: %s", n, data)
				}
			}
			base := local.URL + "/media/" + v.ID + "/track/italian"
			segments := make([][]byte, 4)
			// Request the reset section first and retry across the rollover. Neither
			// the clock epoch nor extraction positions may depend on request order.
			for _, n := range []int{3, 0, 2, 1} {
				segments[n] = getBytes(t, fmt.Sprintf("%s/%d.m4s", base, n))
			}
			for n, segment := range segments {
				track := audioPart(t, segment).Tracks[0]
				want := clock + period + float64(n%2)*6
				if got := float64(track.BaseTime) / audioSampleRate; math.Abs(got-want) > .011 {
					t.Fatalf("segment %d clock %.6f, want %.6f", n, got, want)
				}
				if retry := getBytes(t, fmt.Sprintf("%s/%d.m4s", base, n)); !bytes.Equal(segment, retry) {
					t.Fatalf("segment %d changed on retry", n)
				}
			}
			assertContinuousAudioFragments(t, segments[:2]...)
			assertContinuousAudioFragments(t, segments[2:]...)
			init := getBytes(t, base+"/init.mp4")
			path := filepath.Join(dir, "audio.mp4")
			if err := os.WriteFile(path, append(append(init, segments[0]...), segments[1]...), 0600); err != nil {
				t.Fatal(err)
			}
			pcm := command(t, "ffmpeg", "-v", "error", "-xerror", "-i", path, "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:1")
			if len(pcm) < 2*11*8000 {
				t.Fatal("wrapped-clock audio was truncated", len(pcm))
			}
			var power float64
			for i := 0; i+1 < len(pcm); i += 2 {
				sample := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
				power += sample * sample
			}
			if power/float64(len(pcm)/2) < 100000 {
				t.Fatal("clock normalization shifted extraction into source padding")
			}
			if os.Getenv("DUBLIFT_VLC_TESTS") == "1" {
				// VLC also fails seeking directly into the reset section of the
				// unmodified TS fixture. Check a seek across rollover within a section;
				// the fragment checks above cover the explicit discontinuity separately.
				checkVLCPlaybackAndSeek(t, master, 8)
			}
		})
	}
}
