package dublift

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const errorScreenDuration = 30.0

// FFmpeg first parses the filtergraph and then the drawtext options. Escape
// both layers so a quote or backslash in the temporary directory survives.
func escapeDrawtextPath(path string) string {
	escape := func(s, special string) string {
		var b strings.Builder
		for _, r := range s {
			if strings.ContainsRune(special, r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		return b.String()
	}
	return escape(escape(path, `\':`), `\',;[]`)
}

// Render error details as data, never as drawtext expressions or filter syntax.
// Wrap and bound provider messages so the instruction remains on screen.
func playbackErrorText(err error) string {
	detail := strings.ToValidUTF8(safeFFmpegError(err.Error()), "?")
	words := strings.Fields(detail)
	lines := []string{}
	line := ""
	for _, word := range words {
		for len([]rune(word)) > 68 {
			r := []rune(word)
			wordsPart := string(r[:68])
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			lines = append(lines, wordsPart)
			word = string(r[68:])
		}
		if len([]rune(line))+len([]rune(word))+1 > 68 {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		lines = append(lines, line)
	}
	if len(lines) > 5 {
		lines = lines[:5]
		lines[4] += "..."
	}
	return "DubLift: playback cannot continue\n\n" + strings.Join(lines, "\n") + "\n\nPlease try a different source."
}

// Startup errors use one finite H.264 clip with its own initialization map.
func (e *Engine) preparationErrorVideo(ctx context.Context, err error) ([]byte, error) {
	file, failure := os.CreateTemp("", "dublift-error-*.txt")
	if failure != nil {
		return nil, failure
	}
	defer os.Remove(file.Name())
	_, failure = file.WriteString(playbackErrorText(err))
	closeErr := file.Close()
	if failure != nil {
		return nil, failure
	}
	if closeErr != nil {
		return nil, closeErr
	}
	font := "font=Sans"
	encoder := "libx264"
	profile := "baseline"
	if runtime.GOOS == "android" {
		// Android has system fonts but no desktop fontconfig database.
		font = ""
		for _, name := range []string{"Roboto-Regular.ttf", "RobotoStatic-Regular.ttf", "NotoSans-Regular.ttf", "Roboto-VF.ttf"} {
			candidate := "/system/fonts/" + name
			if _, failure := os.Stat(candidate); failure == nil {
				font = "fontfile='" + candidate + "'"
				break
			}
		}
		if font == "" {
			return nil, errors.New("no Android system font found for the error screen")
		}
		encoder = "libopenh264"
		profile = "constrained_baseline"
	}
	filter := fmt.Sprintf("drawtext=%s:textfile=%s:expansion=none:fontcolor=white:fontsize=29:line_spacing=9:x=(w-text_w)/2:y=(h-text_h)/2", font, escapeDrawtextPath(file.Name()))
	encoderArgs := []string{"-c:v", encoder}
	if runtime.GOOS != "android" {
		encoderArgs = append(encoderArgs, "-preset", "ultrafast")
	}
	// VLC's Android HLS playback can display black for a 1 fps clip.
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=0x101820:s=1280x720:r=10",
		"-t", decimal(errorScreenDuration), "-vf", filter, "-an"}
	args = append(args, encoderArgs...)
	args = append(args,
		"-threads", "1", "-pix_fmt", "yuv420p", "-profile:v", profile, "-g", "20", "-bf", "0",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1")
	return e.run(ctx, e.Config.Get().FFmpeg, args, 4<<20)
}

// Preparation can fail before there is any usable source playlist. Publish a
// small, finite replacement under the same playback URL and session lifecycle.
func (s *Server) servePreparationError(w http.ResponseWriter, r *http.Request, v *Session, path []string, err error) {
	if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
		failure(w, http.StatusBadGateway, err)
		return
	}
	// Only a completed, failed preparation can publish this clip. Waiting
	// requests may be canceled independently of the shared preparation.
	select {
	case <-v.ready:
	default:
		failure(w, http.StatusBadGateway, err)
		return
	}
	v.mu.Lock()
	published := v.masterLoaded
	v.mu.Unlock()
	if published || v.prepareErr == nil {
		v.note(err)
		failure(w, http.StatusBadGateway, err)
		return
	}
	err = v.prepareErr
	v.note(err)
	switch {
	case len(path) == 2 && path[1] == "master.m3u8":
		sendPlaylist(w, []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-STREAM-INF:BANDWIDTH=500000\nerror.m3u8\n"))
	case len(path) == 2 && path[1] == "error.m3u8":
		sendPlaylist(w, []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:30\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=\"error-init.mp4\"\n#EXTINF:30,\nerror.m4s\n#EXT-X-ENDLIST\n"))
	case len(path) == 2 && (path[1] == "error-init.mp4" || path[1] == "error.m4s"):
		work, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		data, generationErr := s.Engine.Cache.Get(work, "preparation-error-video:"+v.ID, func() ([]byte, error) {
			return s.Engine.preparationErrorVideo(work, err)
		})
		if generationErr != nil {
			v.note(generationErr)
			failure(w, http.StatusBadGateway, generationErr)
			return
		}
		init, media, generationErr := splitFMP4(data)
		if generationErr != nil {
			failure(w, http.StatusBadGateway, generationErr)
			return
		}
		if path[1] == "error-init.mp4" {
			media = init
		}
		serveBytes(w, r, "video/mp4", media)
	default:
		http.NotFound(w, r)
	}
}
