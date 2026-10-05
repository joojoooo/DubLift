package dublift

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/bits"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Port of the Nuvio provider's enc=2 source-list envelope. This is metadata,
// not encrypted media; downloaded JavaScript is inspected, never executed.
func decodeMovySources(payload, seed string, mediaID uint32) (movyPayload, error) {
	var result movyPayload
	if len(payload) == 0 || len(payload) > 2800000 || seed == "" || len(seed) > 4096 {
		return result, errors.New("unexpected Movy source envelope")
	}
	payload = strings.NewReplacer("-", "+", "_", "/").Replace(strings.TrimRight(payload, "="))
	data, err := base64.RawStdEncoding.Strict().DecodeString(payload)
	if err != nil {
		return result, errors.New("invalid Movy base64 envelope")
	}
	movyCrypt(data, seed, mediaID)
	if len(data) < 4 || string(data[:4]) != "mvm1" {
		return result, errors.New("Movy envelope changed or seed expired")
	}
	if !utf8.Valid(data[4:]) || json.Unmarshal(data[4:], &result) != nil || result.Sources == nil {
		return movyPayload{}, errors.New("invalid Movy sources payload")
	}
	return result, nil
}

func movyCrypt(data []byte, seed string, mediaID uint32) {
	const golden uint32 = 0x9e3779b9
	mix := func(value uint32) uint32 {
		value = (value ^ (value >> 16)) * 0x85ebca6b
		value = (value ^ (value >> 13)) * 0xc2b2ae35
		return value ^ (value >> 16)
	}
	hash := uint32(0x811c9dc5)
	for _, char := range utf16.Encode([]rune(seed)) {
		hash = (hash ^ uint32(char)) * 0x1000193
	}
	state := mix(mix(hash) ^ mix(mediaID^golden))
	var slots [61]uint32
	var present [61]bool
	for i := 0; i < 8; i++ {
		slot := state % 61
		state = bits.RotateLeft32(state+golden, 7+i)
		slots[slot], present[slot] = state^mix(state), true
		state = mix(state + slot)
	}
	accumulator := mix(0xa5a5a5a5 ^ state)
	for offset, counter := 0, uint32(0); offset < len(data); counter++ {
		slot := accumulator % 61
		right := slots[slot] ^ (golden * (counter + 1))
		combined := accumulator ^ right
		if present[slot] {
			combined |= accumulator & right
		}
		rotated := bits.RotateLeft32(combined+accumulator, int(slot)) ^ bits.RotateLeft32(accumulator, int(slot*7))
		accumulator = mix(rotated + golden)
		slots[slot], present[slot] = accumulator, true
		for shift := 0; shift < 32 && offset < len(data); shift += 8 {
			data[offset] ^= byte(accumulator >> shift)
			offset++
		}
	}
}
