package agent

import (
	"testing"
	"time"
)

func TestBackoffSequence(t *testing.T) {
	b := NewBackoff()
	want := []int{1, 2, 5, 10, 15, 15, 15}
	for i, w := range want {
		if got := b.Next(); got != time.Duration(w)*time.Second {
			t.Fatalf("step %d: got %v want %ds", i, got, w)
		}
	}
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Fatalf("after reset got %v", got)
	}
}
