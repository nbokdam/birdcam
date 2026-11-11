package agent

import (
	"time"

	"birdcam/internal/protocol"
)

// Backoff yields 1,2,5,10,15,15... seconds (capped at protocol.MaxReconnectBackoff).
type Backoff struct {
	steps []time.Duration
	i     int
}

func NewBackoff() *Backoff {
	return &Backoff{steps: []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second}}
}

// NewBackoffSteps is for tests.
func NewBackoffSteps(steps ...time.Duration) *Backoff { return &Backoff{steps: steps} }

func (b *Backoff) Next() time.Duration {
	d := b.steps[min(b.i, len(b.steps)-1)]
	if b.i < len(b.steps) {
		b.i++
	}
	return min(d, max(protocol.MaxReconnectBackoff, b.steps[0]))
}

func (b *Backoff) Reset() { b.i = 0 }
