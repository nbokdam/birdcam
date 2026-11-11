package media

import (
	"sync"
	"time"

	"github.com/pion/rtp"
)

// RTPWriter is implemented by *webrtc.TrackLocalStaticRTP.
type RTPWriter interface {
	WriteRTP(p *rtp.Packet) error
}

// Viewer is one receiver of a Hub. Its writers get rewritten packets:
// continuous sequence numbers and timestamps across source restarts, and video
// only from the first keyframe on.
type Viewer struct {
	Video, Audio RTPWriter

	gateOpen bool
	vrw, arw rewriter
}

// NewViewer creates a viewer writing to the given tracks (either may be nil).
func NewViewer(video, audio RTPWriter) *Viewer {
	return &Viewer{
		Video: video, Audio: audio,
		vrw: rewriter{clock: 90000},
		arw: rewriter{clock: 48000},
	}
}

// Hub broadcasts one camera's packets to its viewers. A Hub lives as long as
// the camera is known; ingest sessions come and go (NewSource).
type Hub struct {
	mu      sync.Mutex
	viewers map[*Viewer]struct{}
	gen     uint64
	sps     []byte
	pps     []byte
	now     func() time.Time
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{viewers: map[*Viewer]struct{}{}, now: time.Now}
}

// Add attaches a viewer. It receives video from the next keyframe on.
func (h *Hub) Add(v *Viewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v.gateOpen = false
	h.viewers[v] = struct{}{}
}

// Remove detaches a viewer.
func (h *Hub) Remove(v *Viewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.viewers, v)
}

// Len returns the number of attached viewers.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.viewers)
}

// NewSource must be called when a new ingest stream starts. Timestamps and
// sequence numbers of the new stream are unrelated to the old one, so viewers
// re-base their rewriters and wait for a keyframe again.
func (h *Hub) NewSource() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen++
	h.sps, h.pps = nil, nil
	for v := range h.viewers {
		v.gateOpen = false
	}
}

// WriteVideo forwards an H.264 packet.
func (h *Hub) WriteVideo(p *rtp.Packet) {
	info := ParseH264(p.Payload)
	h.mu.Lock()
	defer h.mu.Unlock()
	if info.SPS != nil {
		h.sps = append(h.sps[:0], info.SPS...)
	}
	if info.PPS != nil {
		h.pps = append(h.pps[:0], info.PPS...)
	}
	now := h.now()
	for v := range h.viewers {
		if v.Video == nil {
			continue
		}
		if !v.gateOpen {
			// Start at an IDR. Cameras may repeat SPS/PPS before every
			// frame, so SPS alone does not mean a keyframe follows.
			if !info.IDRStart {
				continue
			}
			if info.SPS == nil && h.sps != nil && h.pps != nil {
				// The parameter sets were sent in an earlier packet:
				// replay them right before the IDR.
				ps := &rtp.Packet{Header: p.Header, Payload: BuildSTAPA(h.sps, h.pps)}
				ps.Marker = false
				v.write(v.Video, &v.vrw, ps, h.gen, now)
			}
			v.gateOpen = true
		}
		v.write(v.Video, &v.vrw, p, h.gen, now)
	}
}

// WriteAudio forwards an Opus packet.
func (h *Hub) WriteAudio(p *rtp.Packet) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for v := range h.viewers {
		if v.Audio != nil {
			v.write(v.Audio, &v.arw, p, h.gen, now)
		}
	}
}

func (v *Viewer) write(w RTPWriter, rw *rewriter, p *rtp.Packet, gen uint64, now time.Time) {
	seq, ts := rw.rewrite(p, gen, now)
	out := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        p.Padding,
			Marker:         p.Marker,
			PayloadType:    p.PayloadType,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           p.SSRC,
			PaddingSize:    p.Header.PaddingSize,
		},
		Payload: p.Payload,
	}
	_ = w.WriteRTP(&out)
}

// rewriter maps source sequence numbers/timestamps to a continuous output.
// Sequence numbers are a plain counter (the ingest is TCP, so there is no
// loss to signal). Timestamps keep the source's spacing; on a new source the
// offset is chosen so output time continues by the elapsed wall time.
type rewriter struct {
	clock   float64
	started bool
	gen     uint64
	seq     uint16
	tsOff   uint32
	lastTS  uint32
	lastAt  time.Time
}

func (r *rewriter) rewrite(p *rtp.Packet, gen uint64, now time.Time) (uint16, uint32) {
	if !r.started {
		r.started = true
		r.gen = gen
		r.seq = p.SequenceNumber
		r.tsOff = 0
	} else {
		r.seq++
		if gen != r.gen {
			r.gen = gen
			elapsed := uint32(now.Sub(r.lastAt).Seconds()*r.clock) + 1
			r.tsOff = r.lastTS + elapsed - p.Timestamp
		}
	}
	ts := p.Timestamp + r.tsOff
	if int32(ts-r.lastTS) > 0 || r.lastAt.IsZero() {
		r.lastTS = ts
	}
	r.lastAt = now
	return r.seq, ts
}
