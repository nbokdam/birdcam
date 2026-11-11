package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ALSACardID extracts the card id from "hw:CARD=<id>,DEV=0" (or "plughw:...").
func ALSACardID(device string) string {
	i := strings.Index(device, "CARD=")
	if i < 0 {
		return ""
	}
	id := device[i+len("CARD="):]
	if j := strings.IndexByte(id, ','); j >= 0 {
		id = id[:j]
	}
	return id
}

// ParseCaptureRates returns the capture sample rates listed in a USB audio
// /proc/asound/<card>/stream0 file.
func ParseCaptureRates(stream0 string) []int {
	var rates []int
	inCapture := false
	for _, line := range strings.Split(stream0, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "Capture:":
			inCapture = true
		case t == "Playback:":
			inCapture = false
		case inCapture && strings.HasPrefix(t, "Rates:"):
			for _, f := range strings.Split(strings.TrimPrefix(t, "Rates:"), ",") {
				if r, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
					rates = append(rates, r)
				}
			}
		}
	}
	return rates
}

// PickAudioRate prefers 48000 (no resampling for Opus), otherwise the
// highest supported rate. Returns 48000 if nothing is known.
func PickAudioRate(rates []int) int {
	best := 0
	for _, r := range rates {
		if r == 48000 {
			return r
		}
		best = max(best, r)
	}
	if best == 0 {
		return 48000
	}
	return best
}

// CaptureRate looks up the best capture rate for an ALSA device.
func CaptureRate(procAsound, device string) int {
	id := ALSACardID(device)
	if id == "" {
		return 48000
	}
	b, err := os.ReadFile(filepath.Join(procAsound, id, "stream0"))
	if err != nil {
		return 48000
	}
	return PickAudioRate(ParseCaptureRates(string(b)))
}

// UnmuteMic switches the mic capture on; some cameras boot muted. Errors are
// ignored (not every card has this control).
func UnmuteMic(device string) {
	id := ALSACardID(device)
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "amixer", "-q", "-c", id, "cset", "name=Mic Capture Switch", "on").Run()
}
