package agent

import (
	"fmt"
	"sort"

	"birdcam/internal/protocol"
)

// Selection is the outcome of mode selection for one start request.
type Selection struct {
	Mode        string
	Node        string // device node providing the chosen codec
	Width       int
	Height      int
	FPS         int
	BitrateKbps int // target bitrate for mjpeg-hwenc; 0 for passthrough
	AudioDevice string
}

// Hints are the agent-side inputs of mode selection.
type Hints struct {
	MaxUploadKbps int    // config max_upload_kbps, 0 = unlimited
	NativeKbps    int    // camera's native H.264 bitrate
	PreferredMode string // optional per-camera preference
}

const defaultHWEncKbps = 2500

// Budget combines the profile budget and the agent budget (0 = unlimited).
func Budget(profileKbps, agentKbps int) int {
	switch {
	case profileKbps <= 0:
		return max(agentKbps, 0)
	case agentKbps <= 0:
		return profileKbps
	}
	return min(profileKbps, agentKbps)
}

// SelectMode picks the pipeline mode and the concrete resolution/fps. Pure.
func SelectMode(cam Discovered, req protocol.Profile, hw protocol.Hardware, h Hints) (Selection, error) {
	budget := Budget(req.MaxKbps, h.MaxUploadKbps)
	native := h.NativeKbps
	if native <= 0 {
		native = DefaultNativeKbps
	}
	h264 := formatsFor(cam, "h264")
	mjpeg := formatsFor(cam, "mjpeg")

	audio := ""
	if req.Audio {
		audio = cam.AudioDevice
	}

	passOK := len(h264) > 0 && (budget == 0 || budget >= native)
	hwOK := len(mjpeg) > 0 && hw.HWH264Encoder && hw.HWMJPEGDecoder

	tryPass := func() Selection {
		f := nearestFormat(h264, req.Width, req.Height)
		return Selection{
			Mode: protocol.ModeH264Passthrough, Node: cam.Nodes["h264"],
			Width: f.Width, Height: f.Height, FPS: nearestFPS(f.FPS, req.FPS), AudioDevice: audio,
		}
	}
	tryHW := func() Selection {
		kbps := budget
		if kbps == 0 {
			kbps = defaultHWEncKbps
		}
		w, hh := req.Width, req.Height
		if budget > 0 {
			capW, capH := 0, 0
			switch {
			case budget <= 1000:
				capW, capH = 640, 480
			case budget <= 2500:
				capW, capH = 1280, 720
			}
			if capW > 0 && (w == 0 || hh == 0 || w > capW || hh > capH) {
				w, hh = capW, capH
			}
		}
		f := nearestFormat(mjpeg, w, hh)
		return Selection{
			Mode: protocol.ModeMJPEGHWEnc, Node: cam.Nodes["mjpeg"],
			Width: f.Width, Height: f.Height, FPS: nearestFPS(f.FPS, req.FPS),
			BitrateKbps: kbps, AudioDevice: audio,
		}
	}

	if h.PreferredMode == protocol.ModeMJPEGHWEnc && hwOK {
		return tryHW(), nil
	}
	if h.PreferredMode == protocol.ModeH264Passthrough && len(h264) > 0 {
		return tryPass(), nil
	}
	if passOK {
		return tryPass(), nil
	}
	if hwOK {
		return tryHW(), nil
	}

	switch {
	case len(h264) > 0 && len(mjpeg) == 0:
		return Selection{}, fmt.Errorf("camera %s: H.264 native bitrate ~%d kbps exceeds budget %d kbps and camera has no MJPEG format to re-encode", cam.ID, native, budget)
	case len(h264) > 0:
		return Selection{}, fmt.Errorf("camera %s: budget %d kbps is below native H.264 bitrate ~%d kbps and hardware transcoding is unavailable (hw encoder=%v, hw jpeg decoder=%v)", cam.ID, budget, native, hw.HWH264Encoder, hw.HWMJPEGDecoder)
	case len(mjpeg) > 0:
		return Selection{}, fmt.Errorf("camera %s: MJPEG-only camera needs the Pi hardware H.264 encoder and JPEG decoder (encoder=%v, decoder=%v)", cam.ID, hw.HWH264Encoder, hw.HWMJPEGDecoder)
	}
	return Selection{}, fmt.Errorf("camera %s: no usable H.264 or MJPEG format", cam.ID)
}

func formatsFor(cam Discovered, codec string) []protocol.VideoFormat {
	if cam.Nodes[codec] == "" {
		return nil
	}
	var out []protocol.VideoFormat
	for _, f := range cam.Formats {
		if f.Codec == codec {
			out = append(out, f)
		}
	}
	return out
}

// nearestFormat picks the largest format that fits within w x h; if none fits
// the smallest one. w or h == 0 means "no preference" (largest).
func nearestFormat(fs []protocol.VideoFormat, w, h int) protocol.VideoFormat {
	sorted := append([]protocol.VideoFormat(nil), fs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Width*sorted[i].Height > sorted[j].Width*sorted[j].Height
	})
	if w == 0 || h == 0 {
		return sorted[0]
	}
	for _, f := range sorted {
		if f.Width <= w && f.Height <= h {
			return f
		}
	}
	return sorted[len(sorted)-1]
}

// nearestFPS picks the highest supported fps <= want, else the lowest one.
func nearestFPS(fps []int, want int) int {
	if len(fps) == 0 {
		if want > 0 {
			return want
		}
		return 30
	}
	s := append([]int(nil), fps...)
	sort.Sort(sort.Reverse(sort.IntSlice(s)))
	if want <= 0 {
		return s[0]
	}
	for _, f := range s {
		if f <= want {
			return f
		}
	}
	return s[len(s)-1]
}
