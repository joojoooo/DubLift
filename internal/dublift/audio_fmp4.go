package dublift

import (
	"fmt"
	"math"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

const (
	audioSampleRate   = 48000
	audioFrameSamples = 1024
)

// Quantize both ends on the same presentation-clock grid. Each independently
// encoded segment then ends exactly where its neighbor begins, even when the
// video boundaries are not multiples of an AAC frame.
func audioBoundary(at float64) int64 {
	// Resolve to a PCM sample first so arithmetic noise at a half-frame
	// boundary cannot put adjacent extractions on different AAC frames.
	samples := math.Round(at * audioSampleRate)
	return int64(math.Round(samples/audioFrameSamples)) * audioFrameSamples
}

// Every generated rendition uses the same AAC-LC configuration. The init is
// independent of media extraction, timing offsets, and HTTP request order.
func audioInit() ([]byte, error) {
	init := fmp4.Init{Tracks: []*fmp4.InitTrack{{
		ID: 1, TimeScale: audioSampleRate, AvgBitrate: 192000, MaxBitrate: 192000,
		Codec: &codecs.MPEG4Audio{Config: mpeg4audio.AudioSpecificConfig{
			Type: mpeg4audio.ObjectTypeAACLC, SampleRate: audioSampleRate, ChannelConfig: 2,
		}},
	}}}
	var out seekablebuffer.Buffer
	if err := init.Marshal(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func audioFragment(encoded []byte, base int64, frames int, sequence uint32) ([]byte, error) {
	var packets mpeg4audio.ADTSPackets
	if err := packets.Unmarshal(encoded); err != nil {
		return nil, fmt.Errorf("invalid encoded audio: %w", err)
	}
	// A fresh FFmpeg AAC encoder emits one priming frame. Discard it and any
	// trailing padding; only complete frames belonging to this window survive.
	if len(packets) < frames+1 {
		return nil, fmt.Errorf("incomplete encoded audio: got %d frames, need %d", len(packets), frames+1)
	}
	track := &fmp4.PartTrack{ID: 1, BaseTime: uint64(base)}
	for _, packet := range packets[1 : frames+1] {
		if packet.Type != mpeg4audio.ObjectTypeAACLC || packet.SampleRate != audioSampleRate || packet.ChannelConfig != 2 {
			return nil, fmt.Errorf("unexpected encoded audio configuration")
		}
		track.Samples = append(track.Samples, &fmp4.Sample{Duration: audioFrameSamples, Payload: packet.AU})
	}
	part := fmp4.Part{SequenceNumber: sequence, Tracks: []*fmp4.PartTrack{track}}
	var out seekablebuffer.Buffer
	if err := part.Marshal(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
