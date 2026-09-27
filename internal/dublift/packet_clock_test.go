package dublift

import (
	"math"
	"strings"
	"testing"
)

func TestPacketClock(t *testing.T) {
	// Pipes split records arbitrarily; negative PTS represents seek preroll.
	input := "#software: Lavf\n#tb 0: 1/24000\n#media_type 0: video\n0, -152000, -150000, 1001, 10000, 0x12345678\n0, 0, 0, 1001, 50, 0\n"
	for _, chunk := range []int{1, 7, len(input)} {
		var clock packetClock
		for rest := input; len(rest) > 0; {
			n := min(chunk, len(rest))
			clock.Write([]byte(rest[:n]))
			rest = rest[n:]
		}
		if !clock.known || clock.invalid || math.Abs(clock.first+6.25) > 1e-9 {
			t.Fatalf("chunk %d: %+v", chunk, clock)
		}
	}
	for _, input := range []string{"0, 1, 1, 1, 1, 0\n", "#tb 0: 1/0\n0, 1, 1, 1, 1, 0\n", strings.Repeat("x", 4097), "#tb 0: 1/1000\n0, 0, garbage, 1, 1, 0\n"} {
		var clock packetClock
		clock.Write([]byte(input))
		if clock.known || !clock.invalid {
			t.Fatalf("accepted invalid clock: %+v", clock)
		}
	}
}
