package dublift

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

type FileIndex struct {
	Duration   float64
	Boundaries []float64
	Container  string
	VideoStart float64
}
type metadataReader struct {
	ctx      context.Context
	f        *RemoteFile
	used     int64
	cacheOff int64
	cache    []byte
}

func (r *metadataReader) read(off, n int64) ([]byte, error) {
	if n < 0 || n > 32<<20 || off < 0 || off > r.f.Size-n {
		return nil, errors.New("container metadata exceeds bounded read budget")
	}
	if off >= r.cacheOff && off+n <= r.cacheOff+int64(len(r.cache)) {
		return r.cache[off-r.cacheOff : off-r.cacheOff+n], nil
	}
	length := min(r.f.Size-off, max(n, 64<<10))
	if r.used+length > 40<<20 {
		length = n
	}
	if r.used+length > 40<<20 {
		return nil, errors.New("container metadata exceeds bounded read budget")
	}
	r.used += length
	r.cacheOff = off
	r.cache = make([]byte, length)
	_, e := r.f.ReadAtContext(r.ctx, r.cache, off)
	return r.cache[:n], e
}
func BuildFileIndex(ctx context.Context, f *RemoteFile) (FileIndex, error) {
	r := &metadataReader{ctx: ctx, f: f, used: int64(len(f.prefix)), cache: f.prefix}
	b, e := r.read(0, min(16, f.Size))
	if e != nil {
		return FileIndex{}, e
	}
	if len(b) >= 4 && binary.BigEndian.Uint32(b) == 0x1a45dfa3 {
		return indexMKV(r)
	}
	if len(b) >= 8 && (string(b[4:8]) == "ftyp" || string(b[4:8]) == "moov" || string(b[4:8]) == "free") {
		return indexMP4(r)
	}
	return FileIndex{}, errors.New("seekable file requires indexed Matroska or MP4; no sequential scan is allowed")
}
func segmentBoundaries(keys []float64, duration float64) ([]float64, error) {
	if duration <= 0 || duration > 24*3600 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return nil, errors.New("invalid container duration")
	}
	sort.Float64s(keys)
	out := []float64{0}
	for _, t := range keys {
		span := 5.999
		if len(out) == 1 {
			span = 1
		} // Make the first playable GOP available sooner.
		if t >= out[len(out)-1]+span && t < duration-.01 {
			out = append(out, t)
		}
	}
	out = append(out, duration)
	for i := 1; i < len(out); i++ {
		if out[i]-out[i-1] > 40 {
			return nil, errors.New("keyframe spacing exceeds 40 seconds; bounded remux cannot support this file")
		}
	}
	return out, nil
}

type ebmlElement struct {
	id              uint64
	data, size, end int64
}

func vint(b []byte, id bool) (uint64, int, error) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, errors.New("invalid EBML integer")
	}
	n := 1
	mask := byte(0x80)
	for b[0]&mask == 0 {
		n++
		mask >>= 1
	}
	if n > 8 || n > len(b) {
		return 0, 0, errors.New("truncated EBML integer")
	}
	v := uint64(b[0])
	if !id {
		v &= uint64(mask - 1)
	}
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	return v, n, nil
}
func ebmlHeader(b []byte, off int64) (ebmlElement, error) {
	id, n, e := vint(b, true)
	if e != nil {
		return ebmlElement{}, e
	}
	size, k, e := vint(b[n:], false)
	if e != nil {
		return ebmlElement{}, e
	}
	d := off + int64(n+k)
	if size == (uint64(1)<<uint(7*k))-1 {
		return ebmlElement{id, d, -1, -1}, nil
	}
	if size > math.MaxInt64 || int64(size) > math.MaxInt64-d {
		return ebmlElement{}, errors.New("EBML element overflow")
	}
	return ebmlElement{id, d, int64(size), d + int64(size)}, nil
}
func (r *metadataReader) element(off int64) (ebmlElement, error) {
	b, e := r.read(off, min(16, r.f.Size-off))
	if e != nil {
		return ebmlElement{}, e
	}
	return ebmlHeader(b, off)
}

type ebmlField struct {
	id uint64
	b  []byte
}

func ebmlFields(b []byte) ([]ebmlField, error) {
	var fields []ebmlField
	for i := int64(0); i < int64(len(b)); {
		v, e := ebmlHeader(b[i:], i)
		if e != nil {
			return nil, e
		}
		if v.size < 0 || v.end > int64(len(b)) {
			return nil, errors.New("truncated EBML metadata")
		}
		fields = append(fields, ebmlField{v.id, b[v.data:v.end]})
		i = v.end
		if len(fields) > 200000 {
			return nil, errors.New("too many EBML fields")
		}
	}
	return fields, nil
}
func uintBE(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}
func ebmlFloat(b []byte) float64 {
	if len(b) == 4 {
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	}
	if len(b) == 8 {
		return math.Float64frombits(binary.BigEndian.Uint64(b))
	}
	return 0
}
func indexMKV(r *metadataReader) (FileIndex, error) {
	first, e := r.element(0)
	if e != nil {
		return FileIndex{}, e
	}
	seg, e := r.element(first.end)
	if e != nil || seg.id != 0x18538067 {
		return FileIndex{}, errors.New("Matroska Segment missing")
	}
	loc := map[uint64]int64{}
	off := seg.data
	for count := 0; count < 32 && off < r.f.Size; count++ {
		v, e := r.element(off)
		if e != nil {
			return FileIndex{}, e
		}
		if v.id == 0x1f43b675 {
			break
		}
		loc[v.id] = off
		if v.id == 0x114d9b74 {
			b, e := r.read(v.data, v.size)
			if e != nil {
				return FileIndex{}, e
			}
			entries, e := ebmlFields(b)
			if e != nil {
				return FileIndex{}, e
			}
			for _, entry := range entries {
				if entry.id != 0x4dbb {
					continue
				}
				fields, e := ebmlFields(entry.b)
				if e != nil {
					return FileIndex{}, e
				}
				var id, pos uint64
				for _, f := range fields {
					if f.id == 0x53ab {
						id = uintBE(f.b)
					}
					if f.id == 0x53ac {
						pos = uintBE(f.b)
					}
				}
				if pos > uint64(r.f.Size-seg.data) {
					return FileIndex{}, errors.New("invalid Matroska seek offset")
				}
				loc[id] = seg.data + int64(pos)
			}
		}
		if v.end < 0 {
			break
		}
		off = v.end
	}
	readFields := func(id uint64) ([]ebmlField, error) {
		o, ok := loc[id]
		if !ok {
			return nil, fmt.Errorf("Matroska index missing element %x; refusing to scan media", id)
		}
		v, e := r.element(o)
		if e != nil {
			return nil, e
		}
		if v.id != id {
			return nil, errors.New("invalid Matroska seek table")
		}
		b, e := r.read(v.data, v.size)
		if e != nil {
			return nil, e
		}
		return ebmlFields(b)
	}
	info, e := readFields(0x1549a966)
	if e != nil {
		return FileIndex{}, e
	}
	scale := float64(1000000)
	duration := 0.0
	for _, f := range info {
		if f.id == 0x2ad7b1 {
			scale = float64(uintBE(f.b))
		}
		if f.id == 0x4489 {
			duration = ebmlFloat(f.b)
		}
	}
	duration *= scale / 1e9
	tracks, e := readFields(0x1654ae6b)
	if e != nil {
		return FileIndex{}, e
	}
	var video uint64
	for _, t := range tracks {
		if t.id != 0xae {
			continue
		}
		fs, e := ebmlFields(t.b)
		if e != nil {
			return FileIndex{}, e
		}
		var num, kind uint64
		for _, f := range fs {
			if f.id == 0xd7 {
				num = uintBE(f.b)
			}
			if f.id == 0x83 {
				kind = uintBE(f.b)
			}
		}
		if kind == 1 && video == 0 {
			video = num
		}
	}
	if video == 0 {
		return FileIndex{}, errors.New("Matroska has no video track")
	}
	cues, e := readFields(0x1c53bb6b)
	if e != nil {
		return FileIndex{}, e
	}
	var keys []float64
	for _, cue := range cues {
		if cue.id != 0xbb {
			continue
		}
		fs, e := ebmlFields(cue.b)
		if e != nil {
			return FileIndex{}, e
		}
		var t float64
		match := false
		for _, f := range fs {
			if f.id == 0xb3 {
				t = float64(uintBE(f.b)) * scale / 1e9
			}
			if f.id == 0xb7 {
				ps, e := ebmlFields(f.b)
				if e != nil {
					return FileIndex{}, e
				}
				for _, p := range ps {
					if p.id == 0xf7 && uintBE(p.b) == video {
						match = true
					}
				}
			}
		}
		if match {
			keys = append(keys, t)
		}
	}
	if len(keys) == 0 {
		return FileIndex{}, errors.New("Matroska has no video cues")
	}
	bounds, e := segmentBoundaries(keys, duration)
	return FileIndex{duration, bounds, "matroska", keys[0]}, e
}

type mp4Box struct {
	kind string
	data []byte
}

func mp4Boxes(b []byte) ([]mp4Box, error) {
	var out []mp4Box
	for i := uint64(0); i < uint64(len(b)); {
		if uint64(len(b))-i < 8 {
			return nil, errors.New("truncated MP4 box")
		}
		size := uint64(binary.BigEndian.Uint32(b[i:]))
		head := uint64(8)
		kind := string(b[i+4 : i+8])
		if size == 1 {
			if uint64(len(b))-i < 16 {
				return nil, errors.New("truncated MP4 size")
			}
			size = binary.BigEndian.Uint64(b[i+8:])
			head = 16
		}
		if size == 0 {
			size = uint64(len(b)) - i
		}
		if size < head || size > uint64(len(b))-i {
			return nil, errors.New("invalid MP4 box size")
		}
		out = append(out, mp4Box{kind, b[i+head : i+size]})
		i += size
		if len(out) > 200000 {
			return nil, errors.New("too many MP4 boxes")
		}
	}
	return out, nil
}
func mp4Child(b []byte, names ...string) []byte {
	for _, name := range names {
		xs, e := mp4Boxes(b)
		if e != nil {
			return nil
		}
		b = nil
		for _, x := range xs {
			if x.kind == name {
				b = x.data
				break
			}
		}
		if b == nil {
			return nil
		}
	}
	return b
}
func indexMP4(r *metadataReader) (FileIndex, error) {
	var moov []byte
	for off, count := int64(0), 0; off < r.f.Size && count < 256; count++ {
		b, e := r.read(off, min(16, r.f.Size-off))
		if e != nil {
			return FileIndex{}, e
		}
		if len(b) < 8 {
			return FileIndex{}, errors.New("truncated MP4")
		}
		size := int64(binary.BigEndian.Uint32(b))
		head := int64(8)
		if size == 1 {
			if len(b) < 16 {
				return FileIndex{}, errors.New("truncated MP4")
			}
			n := binary.BigEndian.Uint64(b[8:])
			if n > math.MaxInt64 {
				return FileIndex{}, errors.New("MP4 size overflow")
			}
			size = int64(n)
			head = 16
		}
		if size == 0 {
			size = r.f.Size - off
		}
		if size < head || size > r.f.Size-off {
			return FileIndex{}, errors.New("invalid MP4 box")
		}
		if string(b[4:8]) == "moov" {
			moov, e = r.read(off+head, size-head)
			if e != nil {
				return FileIndex{}, e
			}
			break
		}
		off += size
	}
	if moov == nil {
		return FileIndex{}, errors.New("MP4 has no bounded moov index")
	}
	boxes, e := mp4Boxes(moov)
	if e != nil {
		return FileIndex{}, e
	}
	for _, trak := range boxes {
		if trak.kind != "trak" {
			continue
		}
		hdlr := mp4Child(trak.data, "mdia", "hdlr")
		if len(hdlr) < 12 || string(hdlr[8:12]) != "vide" {
			continue
		}
		mdhd := mp4Child(trak.data, "mdia", "mdhd")
		if len(mdhd) < 24 {
			return FileIndex{}, errors.New("MP4 mdhd missing")
		}
		scale := uint32(0)
		var ticks uint64
		if mdhd[0] == 1 {
			if len(mdhd) < 36 {
				return FileIndex{}, errors.New("truncated MP4 mdhd")
			}
			scale = binary.BigEndian.Uint32(mdhd[20:])
			ticks = binary.BigEndian.Uint64(mdhd[24:])
		} else {
			scale = binary.BigEndian.Uint32(mdhd[12:])
			ticks = uint64(binary.BigEndian.Uint32(mdhd[16:]))
		}
		if scale == 0 {
			return FileIndex{}, errors.New("invalid MP4 timescale")
		}
		duration := float64(ticks) / float64(scale)
		stbl := mp4Child(trak.data, "mdia", "minf", "stbl")
		stts := mp4Child(stbl, "stts")
		stss := mp4Child(stbl, "stss")
		ctts := mp4Child(stbl, "ctts")
		if len(stts) < 8 {
			return FileIndex{}, errors.New("fragmented MP4 without a seek table is unsupported")
		}
		// Multiple edits / dwell edits cannot be represented by a 1:1 source timeline.
		mediaShift := int64(0)
		if elst := mp4Child(trak.data, "edts", "elst"); len(elst) >= 8 {
			if binary.BigEndian.Uint32(elst[4:]) != 1 {
				return FileIndex{}, errors.New("complex MP4 edit list is unsupported")
			}
			if elst[0] == 1 {
				if len(elst) < 28 {
					return FileIndex{}, errors.New("truncated edit list")
				}
				mediaShift = int64(binary.BigEndian.Uint64(elst[16:]))
			} else {
				if len(elst) < 20 {
					return FileIndex{}, errors.New("truncated edit list")
				}
				mediaShift = int64(int32(binary.BigEndian.Uint32(elst[12:])))
			}
			if mediaShift < 0 {
				return FileIndex{}, errors.New("empty MP4 edit is unsupported")
			}
		}
		syncs := map[uint32]bool{}
		if len(stss) > 0 {
			if len(stss) < 8 || uint64(binary.BigEndian.Uint32(stss[4:]))*4 > uint64(len(stss)-8) {
				return FileIndex{}, errors.New("invalid sync sample table")
			}
			for i := 8; i < len(stss); i += 4 {
				if i+4 > len(stss) {
					break
				}
				syncs[binary.BigEndian.Uint32(stss[i:])] = true
			}
		}
		type run struct {
			count uint32
			value int64
		}
		var offsets []run
		if len(ctts) > 0 {
			if len(ctts) < 8 || (len(ctts)-8)%8 != 0 {
				return FileIndex{}, errors.New("invalid composition table")
			}
			for i := 8; i+8 <= len(ctts); i += 8 {
				v := int64(binary.BigEndian.Uint32(ctts[i+4:]))
				if ctts[0] == 1 {
					v = int64(int32(v))
				}
				offsets = append(offsets, run{binary.BigEndian.Uint32(ctts[i:]), v})
			}
		}
		sample := uint32(1)
		dts := int64(0)
		oi := 0
		oc := uint32(0)
		var keys []float64
		if uint64(binary.BigEndian.Uint32(stts[4:]))*8 > uint64(len(stts)-8) {
			return FileIndex{}, errors.New("invalid time sample table")
		}
		for i := 8; i+8 <= len(stts); i += 8 {
			count := binary.BigEndian.Uint32(stts[i:])
			delta := binary.BigEndian.Uint32(stts[i+4:])
			if uint64(sample)+uint64(count) > 10000000 {
				return FileIndex{}, errors.New("MP4 sample count exceeds index budget")
			}
			for j := uint32(0); j < count; j++ {
				shift := int64(0)
				if oi < len(offsets) {
					shift = offsets[oi].value
					oc++
					if oc >= offsets[oi].count {
						oi++
						oc = 0
					}
				}
				if len(stss) == 0 || syncs[sample] {
					t := float64(dts+shift-mediaShift) / float64(scale)
					if t >= 0 && (len(keys) == 0 || t >= keys[len(keys)-1]+.1) {
						keys = append(keys, t)
					}
				}
				dts += int64(delta)
				sample++
			}
		}
		if len(keys) == 0 {
			return FileIndex{}, errors.New("MP4 has no sync samples")
		}
		bounds, e := segmentBoundaries(keys, duration)
		return FileIndex{duration, bounds, "mp4", keys[0]}, e
	}
	return FileIndex{}, errors.New("MP4 video track not found")
}
