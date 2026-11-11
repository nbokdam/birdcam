package agent

import (
	"os"
	"path/filepath"
	"strings"

	"birdcam/internal/protocol"
)

// DetectHardware reads the Pi model and looks for the bcm2835 codec nodes by
// their V4L2 names. sysRoot is normally "/sys"; deviceTreeModel is the path of
// the device-tree model file.
func DetectHardware(sysRoot, deviceTreeModel string, maxUploadKbps int) protocol.Hardware {
	hw := protocol.Hardware{MaxUploadKbps: maxUploadKbps}
	if b, err := os.ReadFile(deviceTreeModel); err == nil {
		hw.Model = strings.TrimRight(strings.TrimSpace(string(b)), "\x00")
	}
	names, _ := filepath.Glob(filepath.Join(sysRoot, "class/video4linux/*/name"))
	for _, p := range names {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		n := strings.TrimSpace(string(b))
		switch {
		case strings.Contains(n, "bcm2835-codec-encode"):
			hw.HWH264Encoder = true
		case strings.Contains(n, "bcm2835-codec-decode"):
			hw.HWMJPEGDecoder = true
		}
	}
	return hw
}
