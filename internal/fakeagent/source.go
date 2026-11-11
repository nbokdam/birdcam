package fakeagent

import (
	"bytes"
	"errors"
	"os"
)

// FrameSource yields H.264 access units in Annex-B format.
type FrameSource interface {
	Next() []byte
}

// SplitNALs splits an Annex-B byte stream into NAL units (without start codes).
func SplitNALs(data []byte) [][]byte {
	var nals [][]byte
	start := -1
	i := 0
	for i+2 < len(data) {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if start >= 0 {
				end := i
				for end > start && data[end-1] == 0 { // trailing zero of a 4-byte start code
					end--
				}
				if end > start {
					nals = append(nals, data[start:end])
				}
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		nals = append(nals, data[start:])
	}
	return nals
}

// GroupAccessUnits groups NAL units into access units (frames). A new access
// unit starts at an AUD/SPS/PPS/SEI after a slice, or at a slice with
// first_mb_in_slice == 0 after a slice.
func GroupAccessUnits(nals [][]byte) [][][]byte {
	var aus [][][]byte
	var cur [][]byte
	hasSlice := false
	for _, n := range nals {
		if len(n) == 0 {
			continue
		}
		t := n[0] & 0x1f
		isSlice := t == 1 || t == 5
		newAU := false
		switch {
		case isSlice && hasSlice && len(n) > 1 && n[1]&0x80 != 0:
			newAU = true
		case (t == 6 || t == 7 || t == 8 || t == 9) && hasSlice:
			newAU = true
		}
		if newAU {
			aus = append(aus, cur)
			cur, hasSlice = nil, false
		}
		cur = append(cur, n)
		if isSlice {
			hasSlice = true
		}
	}
	if len(cur) > 0 && hasSlice {
		aus = append(aus, cur)
	}
	return aus
}

// annexB joins NAL units with 4-byte start codes.
func annexB(nals [][]byte) []byte {
	var b bytes.Buffer
	for _, n := range nals {
		b.Write([]byte{0, 0, 0, 1})
		b.Write(n)
	}
	return b.Bytes()
}

// loopSource cycles through a fixed list of access units.
type loopSource struct {
	aus [][]byte
	i   int
}

func (l *loopSource) Next() []byte {
	au := l.aus[l.i]
	l.i = (l.i + 1) % len(l.aus)
	return au
}

// FileSource loads a raw H.264 Annex-B file and loops it, starting at the
// first IDR frame.
func FileSource(path string) (FrameSource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	groups := GroupAccessUnits(SplitNALs(data))
	first := -1
	for i, g := range groups {
		for _, n := range g {
			if n[0]&0x1f == 5 {
				first = i
				break
			}
		}
		if first >= 0 {
			break
		}
	}
	if first < 0 {
		return nil, errors.New("no IDR frame found in " + path)
	}
	src := &loopSource{}
	for _, g := range groups[first:] {
		src.aus = append(src.aus, annexB(g))
	}
	return src, nil
}

// SyntheticSource produces fake (not decodable) H.264 access units with real
// NAL headers: SPS+PPS+IDR every gop frames, P slices in between. It is
// enough to exercise packetization, keyframe gating and forwarding.
func SyntheticSource(gop, frameSize int) FrameSource {
	sps := []byte{0x67, 0x4d, 0x40, 0x1f, 0x96, 0x54, 0x05, 0x01, 0xec, 0x80}
	pps := []byte{0x68, 0xee, 0x38, 0x80}
	src := &loopSource{}
	for i := 0; i < gop; i++ {
		var nals [][]byte
		slice := make([]byte, frameSize)
		for j := range slice {
			slice[j] = byte(j*7 + i)
		}
		if i == 0 {
			slice[0], slice[1] = 0x65, 0x88
			nals = [][]byte{sps, pps, slice}
		} else {
			slice[0], slice[1] = 0x41, 0x9a
			nals = [][]byte{sps, pps, slice}[2:]
		}
		src.aus = append(src.aus, annexB(nals))
	}
	return src
}
