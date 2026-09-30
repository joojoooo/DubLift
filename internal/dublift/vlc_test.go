package dublift

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt in with DUBLIFT_VLC_TESTS=1. Check actual decoded video and audio, then
// seek and require new decoded frames; a playable playlist alone is not enough.
func checkVLCPlaybackAndSeek(t *testing.T, mediaURL string, seek int) {
	t.Helper()
	vlc, err := exec.LookPath("vlc")
	if err != nil {
		t.Fatal("VLC requested but unavailable", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	password := token()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, vlc, "--ignore-config", "--no-one-instance", "--intf=dummy", "--extraintf=http", "--http-host=127.0.0.1", "--http-port="+strconv.Itoa(port), "--http-password="+password, "--aout=dummy", "--vout=dummy", "--no-video-title-show", mediaURL)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	type status struct {
		Time        int `json:"time"`
		Information struct {
			Category map[string]map[string]string `json:"category"`
		} `json:"information"`
		Stats struct {
			Video int `json:"decodedvideo"`
			Audio int `json:"decodedaudio"`
		} `json:"stats"`
	}
	client := &http.Client{Timeout: time.Second}
	fetch := func(query string) (status, error) {
		var state status
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/requests/status.json%s", port, query), nil)
		req.SetBasicAuth("", password)
		resp, err := client.Do(req)
		if err != nil {
			return state, err
		}
		defer resp.Body.Close()
		err = json.NewDecoder(resp.Body).Decode(&state)
		return state, err
	}
	selectedAudio := func(state status) string {
		for _, info := range state.Information.Category {
			if info["Type"] == "Audio" && info["Decoded_format"] != "" {
				return info["Description"]
			}
		}
		return ""
	}
	phase, video, audio := 0, 0, 0
	var latest status
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("VLC stalled in phase %d: time=%d decoded video=%d audio=%d", phase, latest.Time, latest.Stats.Video, latest.Stats.Audio)
		case <-ticker.C:
			state, err := fetch("")
			if err != nil {
				continue
			}
			latest = state
			if phase == 0 && state.Time >= 2 && state.Stats.Video > 20 && state.Stats.Audio > 20 {
				if selected := selectedAudio(state); !strings.Contains(selected, "Italian · Vixsrc") {
					t.Fatalf("VLC initially selected %q instead of Italian audio", selected)
				}
				video, audio = state.Stats.Video, state.Stats.Audio
				if _, err = fetch("?command=seek&val=" + strconv.Itoa(seek)); err != nil {
					t.Fatal(err)
				}
				phase = 1
			} else if phase == 1 && state.Time >= seek+2 && state.Stats.Video > video+20 && state.Stats.Audio > audio+20 {
				if selected := selectedAudio(state); !strings.Contains(selected, "Italian · Vixsrc") {
					t.Fatalf("VLC selected %q instead of Italian audio", selected)
				}
				t.Logf("VLC video + Italian audio, seek to %ds: %d new video frames / %d audio blocks", seek, state.Stats.Video-video, state.Stats.Audio-audio)
				return
			}
		}
	}
}
