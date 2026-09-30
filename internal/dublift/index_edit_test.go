package dublift

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

func TestMP4LeadingEmptyEditTimeline(t *testing.T) {
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:], 1000)
	elst := make([]byte, 32)
	binary.BigEndian.PutUint32(elst[4:], 2)
	binary.BigEndian.PutUint32(elst[8:], 41)
	binary.BigEndian.PutUint32(elst[12:], ^uint32(0)) // empty edit
	binary.BigEndian.PutUint32(elst[16:], 0x10000)
	binary.BigEndian.PutUint32(elst[20:], 9382375)
	binary.BigEndian.PutUint32(elst[24:], 1328)
	binary.BigEndian.PutUint32(elst[28:], 0x10000)
	timeline, err := parseMP4EditTimeline(elst, mvhd)
	if err != nil {
		t.Fatal(err)
	}
	if timeline.mediaStart != 1328 || math.Abs(timeline.emptyStart-.041) > 1e-9 || math.Abs(timeline.duration-9382.416) > 1e-9 {
		t.Fatalf("incorrect edit timeline: %+v", timeline)
	}
	// The first displayed sample starts after the leading empty edit.
	first := timeline.emptyStart + float64(1328-timeline.mediaStart)/16000
	if math.Abs(first-.041) > 1e-9 {
		t.Fatalf("first displayed sample at %.6f", first)
	}
	binary.BigEndian.PutUint32(elst[12:], 0)
	if _, err = parseMP4EditTimeline(elst, mvhd); err == nil || !strings.Contains(err.Error(), "complex") {
		t.Fatalf("nonempty first edit was accepted: %v", err)
	}
}
