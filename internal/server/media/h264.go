// Package media fans RTP packets from one ingest stream out to WebRTC
// viewers, without transcoding.
package media

import "encoding/binary"

// H.264 NAL unit types used here.
const (
	nalSlice = 1
	nalIDR   = 5
	nalSPS   = 7
	nalPPS   = 8
	nalSTAPA = 24
	nalFUA   = 28
)

// H264Info describes what an H.264 RTP payload (RFC 6184) contains.
type H264Info struct {
	// SPS and PPS are the parameter set NAL units in this packet (sub-slices
	// of the payload), if any.
	SPS, PPS []byte
	// IDRStart is true when the packet starts an IDR slice.
	IDRStart bool
}

// ParseH264 inspects an H.264 RTP payload (single NAL, STAP-A or FU-A).
func ParseH264(payload []byte) H264Info {
	var info H264Info
	if len(payload) < 1 {
		return info
	}
	switch t := payload[0] & 0x1f; t {
	case nalSTAPA:
		rest := payload[1:]
		for len(rest) >= 2 {
			n := int(binary.BigEndian.Uint16(rest))
			rest = rest[2:]
			if n == 0 || n > len(rest) {
				break
			}
			info.addNAL(rest[:n])
			rest = rest[n:]
		}
	case nalFUA:
		if len(payload) >= 2 && payload[1]&0x80 != 0 && payload[1]&0x1f == nalIDR {
			info.IDRStart = true
		}
	default:
		info.addNAL(payload)
	}
	return info
}

func (i *H264Info) addNAL(nal []byte) {
	if len(nal) == 0 {
		return
	}
	switch nal[0] & 0x1f {
	case nalSPS:
		i.SPS = nal
	case nalPPS:
		i.PPS = nal
	case nalIDR:
		i.IDRStart = true
	}
}

// BuildSTAPA builds a STAP-A payload aggregating the given NAL units.
func BuildSTAPA(nals ...[]byte) []byte {
	var nri byte
	size := 1
	for _, n := range nals {
		size += 2 + len(n)
		if len(n) > 0 && n[0]&0x60 > nri {
			nri = n[0] & 0x60
		}
	}
	out := make([]byte, 0, size)
	out = append(out, nri|nalSTAPA)
	for _, n := range nals {
		out = binary.BigEndian.AppendUint16(out, uint16(len(n)))
		out = append(out, n...)
	}
	return out
}
