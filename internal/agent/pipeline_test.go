package agent

import (
	"strings"
	"testing"

	"birdcam/internal/protocol"
)

func TestBuildGstArgsPassthrough(t *testing.T) {
	args, err := BuildGstArgs(PipelineParams{Mode: protocol.ModeH264Passthrough, H264Node: "/dev/v4l/by-id/x-index2", Width: 1280, Height: 720, FPS: 30, AudioDevice: "hw:CARD=Camera,DEV=0", AudioRate: 48000})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	want := "-q v4l2src device=/dev/v4l/by-id/x-index2 ! video/x-h264,width=1280,height=720,framerate=30/1 ! " +
		"h264parse config-interval=-1 ! rtph264pay pt=96 mtu=1200 config-interval=-1 aggregate-mode=zero-latency timestamp-offset=0 ! f. " +
		"alsasrc device=hw:CARD=Camera,DEV=0 provide-clock=false slave-method=re-timestamp ! audio/x-raw,rate=48000,channels=1 ! " +
		"audiorate ! audioconvert ! audioresample ! audio/x-raw,rate=48000 ! opusenc bitrate=32000 complexity=5 ! rtpopuspay pt=111 timestamp-offset=0 ! f. " +
		"funnel name=f ! rtpstreampay ! fdsink fd=1 sync=false"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestBuildGstArgsHWEnc(t *testing.T) {
	args, err := BuildGstArgs(PipelineParams{Mode: protocol.ModeMJPEGHWEnc, MJPEGNode: "/dev/video0", Width: 1920, Height: 1080, FPS: 30, BitrateKbps: 1500})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "alsasrc") {
		t.Error("audio present without device")
	}
	found := false
	for _, a := range args {
		if a == "extra-controls=controls,video_bitrate=1500000,video_bitrate_mode=0,h264_i_frame_period=60,repeat_sequence_header=1" {
			found = true
		}
		if strings.ContainsAny(a, "\"' ") {
			t.Errorf("token %q contains quote or space", a)
		}
	}
	if !found {
		t.Errorf("extra-controls token missing: %v", args)
	}
	for _, want := range []string{
		"image/jpeg,width=1920,height=1080,framerate=30/1",
		"v4l2jpegdec capture-io-mode=dmabuf ! v4l2convert output-io-mode=dmabuf-import capture-io-mode=dmabuf ! v4l2h264enc output-io-mode=dmabuf-import",
		"video/x-h264,profile=constrained-baseline,level=(string)4 ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if args[0] != "-q" || args[1] == "-e" {
		t.Errorf("args start %v", args[:2])
	}
}

func TestBuildGstArgsSWDecode(t *testing.T) {
	args, err := BuildGstArgs(PipelineParams{Mode: protocol.ModeMJPEGHWEnc, MJPEGNode: "/dev/video0", Width: 640, Height: 480, FPS: 30, BitrateKbps: 1000, SWDecode: true, AudioDevice: "hw:CARD=UVC,DEV=0", AudioRate: 32000})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"! jpegdec ! v4l2h264enc extra-controls=", "level=(string)3.1", "audio/x-raw,rate=32000,channels=1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "v4l2jpegdec") || strings.Contains(joined, "dmabuf") {
		t.Errorf("unexpected HW decode in %s", joined)
	}
}

func TestUseSWDecode(t *testing.T) {
	if !UseSWDecode(640, 480) || !UseSWDecode(320, 240) || UseSWDecode(1280, 720) {
		t.Error("wrong decoder choice")
	}
}

func TestBuildGstArgsErrors(t *testing.T) {
	if _, err := BuildGstArgs(PipelineParams{Mode: "nope"}); err == nil {
		t.Error("want error for unknown mode")
	}
	if _, err := BuildGstArgs(PipelineParams{Mode: protocol.ModeH264Passthrough}); err == nil {
		t.Error("want error for missing node")
	}
	if _, err := BuildGstArgs(PipelineParams{Mode: protocol.ModeMJPEGHWEnc, MJPEGNode: "/dev/video0"}); err == nil {
		t.Error("want error for missing bitrate")
	}
}
