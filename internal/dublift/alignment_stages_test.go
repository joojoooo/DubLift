package dublift

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAlignmentDownloadTimeDoesNotConsumeCalculationTimeout(t *testing.T) {
	pcm := signalPCM(30)
	options := alignmentOptions{DownloadTimeout: 3 * time.Second, CalculationTimeout: time.Second}
	vix := func(ctx context.Context, at, length float64) ([]int16, error) {
		select {
		case <-time.After(1100 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return pcm[int(at*pcmRate):int((at+length)*pcmRate)], nil
	}
	source := func(ctx context.Context, at, length float64) ([]int16, error) {
		return pcm[int(at*pcmRate):int((at+length)*pcmRate)], ctx.Err()
	}
	anchor, err := findAnchor(context.Background(), source, vix, 10, 0, 10, 30, 5, options)
	if err != nil || anchor.Confidence < .68 {
		t.Fatalf("slow download consumed matching budget: %+v, %v", anchor, err)
	}
}

func TestAlignmentFailuresIdentifyDownloadOrCalculation(t *testing.T) {
	pcm := signalPCM(30)
	extract := func(_ context.Context, at, length float64) ([]int16, error) {
		return pcm[int(at*pcmRate):int((at+length)*pcmRate)], nil
	}
	stall := func(ctx context.Context, _, _ float64) ([]int16, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	for _, tc := range []struct {
		name               string
		source, vix        PCMExtractor
		calculationTimeout time.Duration
		message            string
	}{
		{"Vixsrc download", extract, stall, time.Second, "Downloading and decoding Vixsrc English audio timed out"},
		{"upstream download", stall, extract, time.Second, "Downloading and decoding the upstream English clip"},
		{"calculation", extract, extract, time.Nanosecond, "alignment calculations timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := findAnchor(context.Background(), tc.source, tc.vix, 10, 0, 10, 30, 5, alignmentOptions{
				DownloadTimeout: 20 * time.Millisecond, CalculationTimeout: tc.calculationTimeout,
			})
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), tc.message) || strings.Contains(err.Error(), "context deadline exceeded") {
				t.Fatal("ambiguous timeout", err)
			}
			if tc.name == "calculation" && !strings.Contains(err.Error(), "already downloaded and decoded") {
				t.Fatal("download outcome missing", err)
			}
			if tc.name != "calculation" && !strings.Contains(err.Error(), "calculations had not started") {
				t.Fatal("matching outcome missing", err)
			}
		})
	}
}

func TestAlignmentReportsOriginFailureInsteadOfFFmpegProxyError(t *testing.T) {
	ffmpegAvailable(t)
	e := testEngine(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/audio.m3u8" {
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nblocked.ts?token=secret\n#EXT-X-ENDLIST\n")
			return
		}
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer origin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := e.OpenAsset(ctx, Origin{URL: origin.URL + "/audio.m3u8"}, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.PCM(ctx, Track{Asset: a, Selector: "0:a:0"}, 0, 5)
	if err == nil || !strings.Contains(err.Error(), "origin HTTP 403") || strings.Contains(err.Error(), "secret") {
		t.Fatal("origin cause lost or credentials exposed", err)
	}
}

func TestFailedDashboardAlignmentRetriesAtPlaybackPosition(t *testing.T) {
	for _, during := range []bool{false, true} {
		name := "after preparation"
		if during {
			name = "during preparation"
		}
		t.Run(name, func(t *testing.T) {
			s := lifecycleServer(t)
			// A process that fails without networking lets us hold the manual
			// attempt in the background queue while playback begins.
			binary := filepath.Join(t.TempDir(), "ffmpeg")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg := s.Config.Get()
			cfg.FFmpeg = binary
			if err := s.Config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			v := s.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: "http://example.test/source.m3u8"})
			v.video = &Asset{ID: "test", HLS: &HLS{Origin: Origin{URL: v.stream.URL}, Duration: 100,
				Segments: []HLSSegment{{Start: 0, Duration: 100, URI: "http://example.test/segment.ts"}}}}
			for at := 0.; at <= 96; at += 6 {
				v.boundaries = append(v.boundaries, at)
			}
			track := &Track{Asset: v.video, Selector: "0:a:0"}
			v.sourceEnglish, v.vixEnglish = track, track
			v.prepare.Do(func() { close(v.ready) })
			if during {
				s.Engine.backgroundSlots <- struct{}{}
			}
			if !s.startAlignment(v, nil) {
				t.Fatal("manual analysis did not start")
			}
			v.mu.Lock()
			manualDone := v.alignDone
			v.mu.Unlock()
			if !during {
				select {
				case <-manualDone:
				case <-time.After(time.Second):
					t.Fatal("manual attempt did not finish")
				}
				if s.Alignments.Get(v.Key).reusable(cfg) {
					t.Fatal("failed result reused")
				}
			}
			w := httptest.NewRecorder()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			s.ServeHTTP(w, httptest.NewRequest("GET", "/media/"+v.ID+"/master.m3u8", nil).WithContext(ctx))
			if w.Code != 200 || ctx.Err() != nil {
				t.Fatal("playback waited for alignment", w.Code, w.Body.String())
			}
			if during {
				<-s.Engine.backgroundSlots
			}
			deadline := time.Now().Add(time.Second)
			var playbackDone chan struct{}
			for time.Now().Before(deadline) {
				v.mu.Lock()
				if v.playbackAlignmentStarted {
					playbackDone = v.alignDone
				}
				v.mu.Unlock()
				if playbackDone != nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if playbackDone == nil || playbackDone == manualDone {
				t.Fatal("playback did not start a fresh alignment")
			}
			v.videoSegmentRequested(7) // player seeks to 42 seconds
			select {
			case <-playbackDone:
			case <-time.After(time.Second):
				t.Fatal("retry did not follow selected position")
			}
			v.mu.Lock()
			messages := strings.Join(v.Errors, "\n")
			v.mu.Unlock()
			if !strings.Contains(messages, "alignment sample at 52s") {
				t.Fatal("retry did not use seeked area", messages)
			}
		})
	}
}

func TestLegacyStartupAndTimeoutConfigResetsToDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"startImmediately":false,"preparationTimeoutSeconds":240,"alignmentDownloadTimeoutSeconds":600,"alignmentTimeoutSeconds":1,"alignmentSamples":4}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := OpenConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := c.Get()
	if cfg.AlignmentSamples != DefaultSettings().AlignmentSamples || cfg.ConfigVersion != currentConfigVersion || cfg.SetupCompleted {
		t.Fatal("legacy settings did not reset to versioned defaults", cfg)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"startImmediately", "preparationTimeoutSeconds", "alignmentDownloadTimeoutSeconds", "alignmentTimeoutSeconds"} {
		if strings.Contains(string(b), key) {
			t.Fatal("removed setting persisted", key)
		}
	}
}
