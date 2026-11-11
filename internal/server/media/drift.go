package media

import (
	"sync"
	"time"
)

// StreamClock measures how far an RTP stream's media clock drifts from
// arrival (wall clock) time:
//
//	drift = (rtp_ts - first_ts)/clock - (arrival - first_arrival)
//
// RTP timestamp wraparound (32 bit) is handled by unwrapping each step as a
// signed 32-bit difference.
type StreamClock struct {
	clock        float64
	started      bool
	lastTS       uint32
	extTS        int64 // unwrapped ts relative to first ts
	firstArrival time.Time
	drift        float64
}

// NewStreamClock returns a clock for the given RTP clock rate.
func NewStreamClock(clockRate int) StreamClock {
	return StreamClock{clock: float64(clockRate)}
}

// Update feeds one packet and returns the current drift in seconds.
// Positive drift means media time runs ahead of arrival time.
func (s *StreamClock) Update(ts uint32, arrival time.Time) float64 {
	if !s.started {
		s.started = true
		s.lastTS = ts
		s.firstArrival = arrival
		s.drift = 0
		return 0
	}
	s.extTS += int64(int32(ts - s.lastTS))
	s.lastTS = ts
	s.drift = float64(s.extTS)/s.clock - arrival.Sub(s.firstArrival).Seconds()
	return s.drift
}

// Started reports whether at least one packet was seen.
func (s *StreamClock) Started() bool { return s.started }

// Drift returns the last computed drift.
func (s *StreamClock) Drift() float64 { return s.drift }

// DriftTracker tracks the relative drift between a video and an audio stream.
// Safe for concurrent use.
type DriftTracker struct {
	mu    sync.Mutex
	video StreamClock
	audio StreamClock
}

// NewDriftTracker creates a tracker for the given clock rates.
func NewDriftTracker(videoClock, audioClock int) *DriftTracker {
	return &DriftTracker{video: NewStreamClock(videoClock), audio: NewStreamClock(audioClock)}
}

// Video feeds a video packet.
func (d *DriftTracker) Video(ts uint32, arrival time.Time) {
	d.mu.Lock()
	d.video.Update(ts, arrival)
	d.mu.Unlock()
}

// Audio feeds an audio packet.
func (d *DriftTracker) Audio(ts uint32, arrival time.Time) {
	d.mu.Lock()
	d.audio.Update(ts, arrival)
	d.mu.Unlock()
}

// AV returns video drift minus audio drift in seconds; ok is false until
// both streams have been seen.
func (d *DriftTracker) AV() (diff float64, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.video.Started() || !d.audio.Started() {
		return 0, false
	}
	return d.video.Drift() - d.audio.Drift(), true
}
