package dublift

import (
	"bytes"
	"encoding/hex"
	"strings"

	mp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/gohlslib/v2/pkg/codecparams"
	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// FFprobe emits codec initialization data as a hex dump with an ASCII column.
// Invalid or missing data leaves CODECS unspecified rather than guessing a
// profile, level or HDR format. The probe's existing output limit still applies.
func probeExtradata(raw string) []byte {
	var data []byte
	for _, line := range strings.Split(raw, "\n") {
		_, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		value, _, _ = strings.Cut(value, "  ")
		b, err := hex.DecodeString(strings.ReplaceAll(value, " ", ""))
		if err != nil {
			return nil
		}
		data = append(data, b...)
	}
	return data
}

func probeVideoCodec(p Probe) string {
	for _, stream := range p.Streams {
		if stream.CodecType != "video" {
			continue
		}
		data := probeExtradata(stream.Extradata)
		var nalus h264.AnnexB // HEVC uses the same Annex-B delimiters.
		if len(data) > 0 && data[0] == 1 {
			switch stream.CodecName {
			case "h264":
				config := mp4.AVCDecoderConfiguration{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.BoxTypeAvcC()}}
				if _, err := mp4.Unmarshal(bytes.NewReader(data), uint64(len(data)), &config, mp4.Context{}); err != nil {
					return ""
				}
				for _, sps := range config.SequenceParameterSets {
					nalus = append(nalus, sps.NALUnit)
				}
			case "hevc":
				var config mp4.HvcC
				if _, err := mp4.Unmarshal(bytes.NewReader(data), uint64(len(data)), &config, mp4.Context{}); err != nil {
					return ""
				}
				for _, array := range config.NaluArrays {
					for _, nalu := range array.Nalus {
						nalus = append(nalus, nalu.NALUnit)
					}
				}
			}
		} else if nalus.Unmarshal(data) != nil {
			return ""
		}
		for _, nalu := range nalus {
			if len(nalu) == 0 {
				continue
			}
			switch stream.CodecName {
			case "h264":
				if nalu[0]&0x1f == 7 {
					return codecparams.Marshal(&codecs.H264{SPS: nalu})
				}
			case "hevc":
				if nalu[0]>>1&0x3f == 33 {
					codec := codecparams.Marshal(&codecs.H265{SPS: nalu})
					if stream.CodecTag == "hev1" {
						codec = strings.Replace(codec, "hvc1.", "hev1.", 1)
					}
					return codec
				}
			}
		}
		return ""
	}
	return ""
}
