package agent

import (
	"strings"
	"testing"

	"birdcam/internal/protocol"
)

func fps(v ...int) []int { return v }

func h264Cam() Discovered {
	return Discovered{
		Camera: protocol.Camera{ID: "cam1", AudioDevice: "hw:CARD=Camera,DEV=0", Formats: []protocol.VideoFormat{
			{Codec: "mjpeg", Width: 1920, Height: 1080, FPS: fps(30, 15)},
			{Codec: "mjpeg", Width: 1280, Height: 720, FPS: fps(30, 15)},
			{Codec: "mjpeg", Width: 640, Height: 480, FPS: fps(30, 15)},
			{Codec: "h264", Width: 1920, Height: 1080, FPS: fps(30, 15)},
			{Codec: "h264", Width: 1280, Height: 720, FPS: fps(30, 15)},
			{Codec: "h264", Width: 640, Height: 480, FPS: fps(30)},
		}},
		Nodes: map[string]string{"h264": "/dev/h264", "mjpeg": "/dev/mjpeg"},
	}
}

func mjpegCam() Discovered {
	return Discovered{
		Camera: protocol.Camera{ID: "cam2", Formats: []protocol.VideoFormat{
			{Codec: "mjpeg", Width: 640, Height: 480, FPS: fps(30)},
			{Codec: "mjpeg", Width: 320, Height: 240, FPS: fps(30)},
		}},
		Nodes: map[string]string{"mjpeg": "/dev/mjpeg2"},
	}
}

var hwFull = protocol.Hardware{HWH264Encoder: true, HWMJPEGDecoder: true}

func TestSelectMode(t *testing.T) {
	tests := []struct {
		name    string
		cam     Discovered
		req     protocol.Profile
		hw      protocol.Hardware
		hints   Hints
		mode    string
		w, h    int
		fps     int
		kbps    int
		node    string
		audio   string
		wantErr string
	}{
		{name: "unlimited passthrough", cam: h264Cam(), req: protocol.Profile{Width: 1280, Height: 720, FPS: 30, Audio: true}, hw: hwFull,
			mode: protocol.ModeH264Passthrough, w: 1280, h: 720, fps: 30, node: "/dev/h264", audio: "hw:CARD=Camera,DEV=0"},
		{name: "budget above native", cam: h264Cam(), req: protocol.Profile{Width: 1280, Height: 720, FPS: 30, MaxKbps: 8000}, hw: hwFull,
			mode: protocol.ModeH264Passthrough, w: 1280, h: 720, fps: 30, node: "/dev/h264"},
		{name: "nearest lower resolution", cam: h264Cam(), req: protocol.Profile{Width: 1024, Height: 600, FPS: 30}, hw: hwFull,
			mode: protocol.ModeH264Passthrough, w: 640, h: 480, fps: 30, node: "/dev/h264"},
		{name: "fps nearest lower", cam: h264Cam(), req: protocol.Profile{Width: 1280, Height: 720, FPS: 20}, hw: hwFull,
			mode: protocol.ModeH264Passthrough, w: 1280, h: 720, fps: 15, node: "/dev/h264"},
		{name: "agent budget forces hwenc 1000", cam: h264Cam(), req: protocol.Profile{Width: 1280, Height: 720, FPS: 30}, hw: hwFull,
			hints: Hints{MaxUploadKbps: 1000}, mode: protocol.ModeMJPEGHWEnc, w: 640, h: 480, fps: 30, kbps: 1000, node: "/dev/mjpeg"},
		{name: "profile budget 2000 -> 720p", cam: h264Cam(), req: protocol.Profile{Width: 1920, Height: 1080, FPS: 30, MaxKbps: 2000}, hw: hwFull,
			hints: Hints{MaxUploadKbps: 5000}, mode: protocol.ModeMJPEGHWEnc, w: 1280, h: 720, fps: 30, kbps: 2000, node: "/dev/mjpeg"},
		{name: "budget 4000 keeps requested", cam: h264Cam(), req: protocol.Profile{Width: 1920, Height: 1080, FPS: 30, MaxKbps: 4000}, hw: hwFull,
			mode: protocol.ModeMJPEGHWEnc, w: 1920, h: 1080, fps: 30, kbps: 4000, node: "/dev/mjpeg"},
		{name: "native bitrate configurable", cam: h264Cam(), req: protocol.Profile{Width: 1280, Height: 720, FPS: 30, MaxKbps: 3000}, hw: hwFull,
			hints: Hints{NativeKbps: 2500}, mode: protocol.ModeH264Passthrough, w: 1280, h: 720, fps: 30, node: "/dev/h264"},
		{name: "mjpeg only unlimited", cam: mjpegCam(), req: protocol.Profile{Width: 1280, Height: 720, FPS: 30, Audio: true}, hw: hwFull,
			mode: protocol.ModeMJPEGHWEnc, w: 640, h: 480, fps: 30, kbps: 2500, node: "/dev/mjpeg2"},
		{name: "mjpeg only low budget", cam: mjpegCam(), req: protocol.Profile{Width: 640, Height: 480, FPS: 30, MaxKbps: 500}, hw: hwFull,
			mode: protocol.ModeMJPEGHWEnc, w: 640, h: 480, fps: 30, kbps: 500, node: "/dev/mjpeg2"},
		{name: "mjpeg only no hw", cam: mjpegCam(), req: protocol.Profile{Width: 640, Height: 480, FPS: 30}, wantErr: "hardware H.264 encoder"},
		{name: "h264 over budget no hw", cam: h264Cam(), req: protocol.Profile{Width: 640, Height: 480, FPS: 30, MaxKbps: 1000}, wantErr: "below native"},
		{name: "preferred hwenc", cam: h264Cam(), req: protocol.Profile{Width: 1280, Height: 720, FPS: 30}, hw: hwFull,
			hints: Hints{PreferredMode: protocol.ModeMJPEGHWEnc}, mode: protocol.ModeMJPEGHWEnc, w: 1280, h: 720, fps: 30, kbps: 2500, node: "/dev/mjpeg"},
		{name: "audio not requested", cam: h264Cam(), req: protocol.Profile{Width: 640, Height: 480, FPS: 30}, hw: hwFull,
			mode: protocol.ModeH264Passthrough, w: 640, h: 480, fps: 30, node: "/dev/h264"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectMode(tc.cam, tc.req, tc.hw, tc.hints)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := Selection{Mode: tc.mode, Node: tc.node, Width: tc.w, Height: tc.h, FPS: tc.fps, BitrateKbps: tc.kbps, AudioDevice: tc.audio}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestSelectModeNativeControl(t *testing.T) {
	ctl := Hints{NativeBitrateControl: true, NativeKbps: 6500}
	tests := []struct {
		name    string
		req     protocol.Profile
		hints   Hints
		mode    string
		w, h    int
		camKbps int
		kbps    int
	}{
		{"unlimited sets default", protocol.Profile{Width: 1920, Height: 1080, FPS: 30}, ctl, protocol.ModeH264Passthrough, 1920, 1080, 6500, 0},
		{"budget above native", protocol.Profile{Width: 1920, Height: 1080, FPS: 30, MaxKbps: 8000}, ctl, protocol.ModeH264Passthrough, 1920, 1080, 6500, 0},
		{"budget 4000 audio", protocol.Profile{Width: 1920, Height: 1080, FPS: 30, MaxKbps: 4000, Audio: true}, ctl, protocol.ModeH264Passthrough, 1920, 1080, 3760, 0},
		{"budget 2000 -> 720p", protocol.Profile{Width: 1920, Height: 1080, FPS: 30, MaxKbps: 2000, Audio: true}, ctl, protocol.ModeH264Passthrough, 1280, 720, 1760, 0},
		{"budget 1000 -> vga", protocol.Profile{Width: 1280, Height: 720, FPS: 30, MaxKbps: 1000, Audio: true}, ctl, protocol.ModeH264Passthrough, 640, 480, 760, 0},
		{"budget 1000 no audio", protocol.Profile{Width: 1280, Height: 720, FPS: 30, MaxKbps: 1000}, ctl, protocol.ModeH264Passthrough, 640, 480, 940, 0},
		{"budget 300 floor", protocol.Profile{Width: 640, Height: 480, FPS: 30, MaxKbps: 300, Audio: true}, ctl, protocol.ModeH264Passthrough, 640, 480, 150, 0},
		{"budget 250 -> hwenc", protocol.Profile{Width: 640, Height: 480, FPS: 30, MaxKbps: 250}, ctl, protocol.ModeMJPEGHWEnc, 640, 480, 0, 250},
		{"no control, low budget -> hwenc", protocol.Profile{Width: 1280, Height: 720, FPS: 30, MaxKbps: 2000}, Hints{NativeKbps: 6500}, protocol.ModeMJPEGHWEnc, 1280, 720, 0, 2000},
		{"preferred hwenc wins", protocol.Profile{Width: 1280, Height: 720, FPS: 30, MaxKbps: 2000}, Hints{NativeBitrateControl: true, PreferredMode: protocol.ModeMJPEGHWEnc}, protocol.ModeMJPEGHWEnc, 1280, 720, 0, 2000},
		{"preferred passthrough", protocol.Profile{Width: 1280, Height: 720, FPS: 30, MaxKbps: 2000}, Hints{NativeBitrateControl: true, NativeKbps: 6500, PreferredMode: protocol.ModeH264Passthrough}, protocol.ModeH264Passthrough, 1280, 720, 2000 - 60, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectMode(h264Cam(), tc.req, hwFull, tc.hints)
			if err != nil {
				t.Fatal(err)
			}
			if got.Mode != tc.mode || got.Width != tc.w || got.Height != tc.h || got.CameraBitrateKbps != tc.camKbps || got.BitrateKbps != tc.kbps {
				t.Fatalf("got %+v", got)
			}
		})
	}
	// no hw and tiny budget: error even with control
	if _, err := SelectMode(h264Cam(), protocol.Profile{MaxKbps: 200}, protocol.Hardware{}, ctl); err == nil {
		t.Fatal("want error")
	}
}

func TestBudget(t *testing.T) {
	for _, c := range [][3]int{{0, 0, 0}, {1000, 0, 1000}, {0, 800, 800}, {1000, 800, 800}, {500, 800, 500}} {
		if got := Budget(c[0], c[1]); got != c[2] {
			t.Errorf("Budget(%d,%d)=%d want %d", c[0], c[1], got, c[2])
		}
	}
}
