package media

import (
	"math"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestStreamClockWraparound(t *testing.T) {
	c := NewStreamClock(90000)
	t0 := time.Unix(1000, 0)
	start := uint32(math.MaxUint32 - 45000) // wraps after 0.5 s
	c.Update(start, t0)
	// 2 s later, timestamps wrapped; media and wall clock agree.
	if d := c.Update(start+180000, t0.Add(2*time.Second)); math.Abs(d) > 1e-9 {
		t.Fatalf("drift across wrap = %v, want 0", d)
	}
	// Media runs 0.25 s ahead of arrival.
	if d := c.Update(start+180000+90000, t0.Add(2750*time.Millisecond)); math.Abs(d-0.25) > 1e-9 {
		t.Fatalf("drift = %v, want 0.25", d)
	}
	// Many wraps: 3 days of 90 kHz is ~5.4 wraps.
	c2 := NewStreamClock(90000)
	ts := uint32(123)
	at := t0
	c2.Update(ts, at)
	for i := 0; i < 3*24*60; i++ { // one update per minute
		ts += 90000 * 60
		at = at.Add(time.Minute)
		c2.Update(ts, at)
	}
	if d := c2.Drift(); math.Abs(d) > 1e-6 {
		t.Fatalf("drift after many wraps = %v", d)
	}
	// Small reordering (older timestamp) is handled as negative step.
	c3 := NewStreamClock(1000)
	c3.Update(5, t0)
	c3.Update(3, t0)
	if d := c3.Drift(); math.Abs(d+0.002) > 1e-9 {
		t.Fatalf("reorder drift = %v", d)
	}
}

func TestDriftTrackerAV(t *testing.T) {
	d := NewDriftTracker(90000, 48000)
	t0 := time.Unix(0, 0)
	d.Video(1000, t0)
	if _, ok := d.AV(); ok {
		t.Fatal("AV should not be available without audio")
	}
	d.Audio(4000000000, t0) // near wrap
	// After 10 s: video is in sync, audio clock ran 1.5 s fast.
	d.Video(1000+900000, t0.Add(10*time.Second))
	d.Audio(4000000000+uint32(11.5*48000), t0.Add(10*time.Second))
	diff, ok := d.AV()
	if !ok || math.Abs(diff-(-1.5)) > 1e-9 {
		t.Fatalf("AV diff = %v %v, want -1.5", diff, ok)
	}
}

func TestParseH264(t *testing.T) {
	sps := []byte{0x67, 1, 2}
	pps := []byte{0x68, 3}
	idr := []byte{0x65, 0x88, 9}
	cases := []struct {
		name          string
		payload       []byte
		sps, pps, idr bool
	}{
		{"single IDR", idr, false, false, true},
		{"single P", []byte{0x41, 0x9a}, false, false, false},
		{"single SPS", sps, true, false, false},
		{"STAP-A sps pps", BuildSTAPA(sps, pps), true, true, false},
		{"STAP-A sps pps idr", BuildSTAPA(sps, pps, idr), true, true, true},
		{"FU-A IDR start", []byte{0x7c, 0x85, 1, 2}, false, false, true},
		{"FU-A IDR middle", []byte{0x7c, 0x05, 1, 2}, false, false, false},
		{"FU-A P start", []byte{0x5c, 0x81, 1, 2}, false, false, false},
		{"truncated STAP-A", []byte{0x78, 0x00, 0x10, 0x67}, false, false, false},
		{"empty", nil, false, false, false},
	}
	for _, c := range cases {
		info := ParseH264(c.payload)
		if (info.SPS != nil) != c.sps || (info.PPS != nil) != c.pps || info.IDRStart != c.idr {
			t.Errorf("%s: got sps=%v pps=%v idr=%v", c.name, info.SPS != nil, info.PPS != nil, info.IDRStart)
		}
	}
}

type capture struct{ pkts []rtp.Packet }

func (c *capture) WriteRTP(p *rtp.Packet) error {
	cp := *p
	cp.Payload = append([]byte(nil), p.Payload...)
	c.pkts = append(c.pkts, cp)
	return nil
}

func vpkt(seq uint16, ts uint32, payload ...byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts}, Payload: payload}
}

func TestHubGatingAndRewrite(t *testing.T) {
	h := NewHub()
	now := time.Unix(100, 0)
	h.now = func() time.Time { return now }
	vc, ac := &capture{}, &capture{}
	v := NewViewer(vc, ac)

	sps := []byte{0x67, 0x4d, 0x40, 0x1f}
	pps := []byte{0x68, 0xee}
	// Parameter sets arrive before the viewer joins.
	h.WriteVideo(vpkt(10, 1000, BuildSTAPA(sps, pps)...))
	h.Add(v)
	h.WriteVideo(vpkt(11, 1000, 0x41, 0x9a))              // P frame: dropped
	h.WriteVideo(vpkt(12, 4000, BuildSTAPA(sps, pps)...)) // SPS before a P frame: dropped
	h.WriteVideo(vpkt(13, 4000, 0x41, 0x9a))              // dropped
	h.WriteAudio(&rtp.Packet{Header: rtp.Header{PayloadType: 111, SequenceNumber: 500, Timestamp: 7000}, Payload: []byte{0xf8}})
	if len(vc.pkts) != 0 {
		t.Fatalf("video forwarded before keyframe: %d packets", len(vc.pkts))
	}
	if len(ac.pkts) != 1 {
		t.Fatalf("audio not forwarded")
	}
	h.WriteVideo(vpkt(14, 7000, 0x7c, 0x85, 1)) // FU-A IDR start: SPS/PPS replayed first
	h.WriteVideo(vpkt(15, 7000, 0x7c, 0x45, 2)) // FU-A end
	h.WriteVideo(vpkt(16, 10000, 0x41, 0x9a))
	if len(vc.pkts) != 4 {
		t.Fatalf("got %d video packets, want 4", len(vc.pkts))
	}
	if info := ParseH264(vc.pkts[0].Payload); info.SPS == nil || info.PPS == nil || vc.pkts[0].Timestamp != 7000 {
		t.Fatalf("first packet should be replayed SPS/PPS at IDR timestamp, got %x ts=%d", vc.pkts[0].Payload, vc.pkts[0].Timestamp)
	}
	for i := 1; i < len(vc.pkts); i++ {
		if vc.pkts[i].SequenceNumber != vc.pkts[i-1].SequenceNumber+1 {
			t.Fatalf("sequence not continuous at %d", i)
		}
	}

	// New source (agent restarted the pipeline): random seq/ts; viewer waits
	// for the next IDR and output continues seamlessly.
	h.NewSource()
	now = now.Add(2 * time.Second)
	h.WriteVideo(vpkt(40000, 3_000_000_000, 0x41, 0x9a)) // dropped
	h.WriteVideo(vpkt(40001, 3_000_000_000, BuildSTAPA(sps, pps, []byte{0x65, 0x88})...))
	last := vc.pkts[len(vc.pkts)-1]
	prev := vc.pkts[len(vc.pkts)-2]
	if last.SequenceNumber != prev.SequenceNumber+1 {
		t.Fatalf("seq jump after new source: %d -> %d", prev.SequenceNumber, last.SequenceNumber)
	}
	wantTS := uint32(10000 + 2*90000 + 1)
	if last.Timestamp != wantTS {
		t.Fatalf("ts after new source = %d, want %d", last.Timestamp, wantTS)
	}
	h.Remove(v)
	n := len(vc.pkts)
	h.WriteVideo(vpkt(40002, 3_000_003_000, 0x41, 0x9a))
	if len(vc.pkts) != n {
		t.Fatal("removed viewer still receives packets")
	}
}
