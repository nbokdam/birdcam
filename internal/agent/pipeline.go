package agent

import (
	"fmt"
	"strings"

	"birdcam/internal/protocol"
)

// Pipeline templates, verified on a Raspberry Pi 3 (see docs/PIPELINES.md).
// Each template is split on whitespace into separate argv tokens (gst-launch
// re-joins and parses them itself, so no shell quoting is needed or wanted;
// none of the values contain spaces).

const (
	// Common RTP payloader tail of every video branch. timestamp-offset=0 on
	// both payloaders gives audio and video a common time origin.
	tplVideoPay = "h264parse config-interval=-1 ! " +
		"rtph264pay pt=96 mtu=1200 config-interval=-1 aggregate-mode=zero-latency timestamp-offset=0 ! f."

	tplPassthrough = "v4l2src device=%[1]s ! video/x-h264,width=%[2]d,height=%[3]d,framerate=%[4]d/1 ! " + tplVideoPay

	// HW JPEG decode → ISP (fixes the decoder's 1088-line padding at 1080p,
	// zero-copy via dmabuf) → HW H.264 encode. VBR is required: CBR drops fps.
	tplHWDecEnc = "v4l2src device=%[1]s ! image/jpeg,width=%[2]d,height=%[3]d,framerate=%[4]d/1 ! " +
		"v4l2jpegdec capture-io-mode=dmabuf ! " +
		"v4l2convert output-io-mode=dmabuf-import capture-io-mode=dmabuf ! " +
		"v4l2h264enc output-io-mode=dmabuf-import " + tplEncControls + " ! " + tplEncCaps + " ! " + tplVideoPay

	// SW JPEG decode → HW H.264 encode. For small/USB 1.1 cameras whose
	// (often corrupt) JPEGs stall the HW decoder.
	tplSWDecEnc = "v4l2src device=%[1]s ! image/jpeg,width=%[2]d,height=%[3]d,framerate=%[4]d/1 ! jpegdec ! " +
		"v4l2h264enc " + tplEncControls + " ! " + tplEncCaps + " ! " + tplVideoPay

	tplEncControls = "extra-controls=controls,video_bitrate=%[5]d,video_bitrate_mode=0,h264_i_frame_period=%[6]d,repeat_sequence_header=1"
	tplEncCaps     = "video/x-h264,profile=constrained-baseline,level=(string)%[7]s"

	// provide-clock=false + re-timestamp: the system clock drives the
	// pipeline, not the USB mic (whose sample loss caused v1's A/V skew).
	tplAudio = "alsasrc device=%[1]s provide-clock=false slave-method=re-timestamp ! " +
		"audio/x-raw,rate=%[2]d,channels=1 ! audiorate ! audioconvert ! audioresample ! audio/x-raw,rate=48000 ! " +
		"opusenc bitrate=32000 complexity=5 ! rtpopuspay pt=111 timestamp-offset=0 ! f."

	tplTail = "funnel name=f ! rtpstreampay ! fdsink fd=1 sync=false"
)

// SWDecodeMaxPixels: at or below this frame size, MJPEG is decoded in software
// (cheap at this size, and robust against corrupt frames).
const SWDecodeMaxPixels = 640 * 480

// PipelineParams describes one pipeline to build.
type PipelineParams struct {
	Mode        string
	H264Node    string
	MJPEGNode   string
	Width       int
	Height      int
	FPS         int
	BitrateKbps int    // mjpeg-hwenc only
	SWDecode    bool   // mjpeg-hwenc only: use jpegdec instead of v4l2jpegdec
	AudioDevice string // "" = no audio
	AudioRate   int    // capture rate of the mic (default 48000)
}

// UseSWDecode reports the default decoder choice for a frame size.
func UseSWDecode(width, height int) bool { return width*height <= SWDecodeMaxPixels }

// h264Level returns the H.264 level for the encoder caps.
func h264Level(w, h, fps int) string {
	if w*h > 1280*720 || (w*h == 1280*720 && fps > 30) {
		return "4"
	}
	return "3.1"
}

// BuildGstArgs returns the gst-launch-1.0 argv (without the program name).
// "-q" is essential: without it gst-launch prints progress to stdout, which
// carries the media stream. No "-e": EOS on SIGINT hangs with the HW decoder.
func BuildGstArgs(p PipelineParams) ([]string, error) {
	var video string
	switch p.Mode {
	case protocol.ModeH264Passthrough:
		if p.H264Node == "" {
			return nil, fmt.Errorf("passthrough needs an H264 device node")
		}
		video = fmt.Sprintf(tplPassthrough, p.H264Node, p.Width, p.Height, p.FPS)
	case protocol.ModeMJPEGHWEnc:
		if p.MJPEGNode == "" {
			return nil, fmt.Errorf("mjpeg-hwenc needs an MJPEG device node")
		}
		if p.BitrateKbps <= 0 {
			return nil, fmt.Errorf("mjpeg-hwenc needs a bitrate")
		}
		tpl := tplHWDecEnc
		if p.SWDecode {
			tpl = tplSWDecEnc
		}
		gop := 2 * p.FPS
		if gop <= 0 {
			gop = 60
		}
		video = fmt.Sprintf(tpl, p.MJPEGNode, p.Width, p.Height, p.FPS, p.BitrateKbps*1000, gop, h264Level(p.Width, p.Height, p.FPS))
	default:
		return nil, fmt.Errorf("unknown mode %q", p.Mode)
	}
	args := []string{"-q"}
	args = append(args, strings.Fields(video)...)
	if p.AudioDevice != "" {
		rate := p.AudioRate
		if rate <= 0 {
			rate = 48000
		}
		args = append(args, strings.Fields(fmt.Sprintf(tplAudio, p.AudioDevice, rate))...)
	}
	args = append(args, strings.Fields(tplTail)...)
	return args, nil
}
