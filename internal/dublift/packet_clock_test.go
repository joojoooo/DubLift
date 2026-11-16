package dublift

import (
	"math"
	"reflect"
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

func TestPacketClockObserver(t *testing.T) {
	input := "#tb 0: 1/1000\n0, -80, 0, 40, 100, 0\n0, -40, 120, 40, 100, 0, F=0x0\n0, 0, 40, 40, 100, 0, F=0x3\n"
	type packet struct {
		dts, pts float64
		key      bool
	}
	want := []packet{{-.08, 0, true}, {-.04, .12, false}, {0, .04, true}}
	for _, chunk := range []int{1, 7, len(input)} {
		var got []packet
		clock := packetClock{packet: func(dts, pts float64, key bool) { got = append(got, packet{dts, pts, key}) }}
		for rest := input; len(rest) > 0; {
			n := min(chunk, len(rest))
			clock.Write([]byte(rest[:n]))
			rest = rest[n:]
		}
		if !clock.known || clock.invalid || clock.first != 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("chunk %d: clock=%+v, packets=%+v", chunk, clock, got)
		}
	}
	for _, input := range []string{
		"#tb 0: 1/1000\n0, bad, 0, 40, 100, 0\n",
		"#tb 0: 1/1000\n0, 0, 0, 40, 100, 0, F=bad\n",
	} {
		clock := packetClock{packet: func(float64, float64, bool) { t.Fatal("observed invalid packet") }}
		clock.Write([]byte(input))
		if !clock.invalid {
			t.Fatalf("accepted invalid packet: %+v", clock)
		}
	}
}
