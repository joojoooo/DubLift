package dublift

// FFmpeg creates each audio segment with a fresh MPEG-TS muxer. HLS requires
// continuity counters to carry on across segments in a rendition. Rewrite the
// counters as segments are delivered, leaving the cached source untouched so
// a seek or a repeated request can start a fresh sequence.
type tsContinuityState struct {
	segment  int
	counters map[uint16]byte
}

func (s *Session) continuousAudioTS(track string, segment int, source []byte) []byte {
	if len(source) == 0 || len(source)%188 != 0 {
		return source
	}
	for i := 0; i < len(source); i += 188 {
		if source[i] != 0x47 {
			return source
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audioTS == nil {
		s.audioTS = make(map[string]tsContinuityState)
	}
	previous, seen := s.audioTS[track]
	continueStream := seen && previous.segment+1 == segment
	if continueStream && s.video != nil && s.video.HLS != nil && segment < len(s.video.HLS.Segments) {
		continueStream = !s.video.HLS.Segments[segment].Discontinuity
	}
	data := append([]byte(nil), source...)
	last := make(map[uint16]byte)
	shifts := make(map[uint16]byte)
	for i := 0; i < len(data); i += 188 {
		packet := data[i : i+188]
		pid := uint16(packet[1]&0x1f)<<8 | uint16(packet[2])
		control := packet[3] >> 4 & 3
		if pid == 0x1fff || control == 0 {
			continue
		}
		original := packet[3] & 15
		shift, seenPID := shifts[pid]
		if !seenPID {
			if continueStream {
				if prior, ok := previous.counters[pid]; ok {
					expected := prior
					if control&1 != 0 { // Only packets carrying payload advance the counter.
						expected++
					}
					shift = (expected - original) & 15
					if control&2 != 0 && packet[4] > 0 {
						packet[5] &^= 0x80 // No discontinuity in a continuous rendition.
					}
				}
			}
			shifts[pid] = shift
		}
		packet[3] = packet[3]&^byte(15) | (original+shift)&15
		last[pid] = packet[3] & 15
	}
	s.audioTS[track] = tsContinuityState{segment: segment, counters: last}
	return data
}
