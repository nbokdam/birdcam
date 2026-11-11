package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"birdcam/internal/protocol"
)

func TestSlugAndSplit(t *testing.T) {
	if got := Slug("usb-USB_Camera_Manufacture_H264_USB_Camera_2020052801"); got != "usb-usb-camera-manufacture-h264-usb-camera-2020052801" {
		t.Error(got)
	}
	if got := Slug("usb-Generic_GENERAL_-_UVC"); got != "usb-generic-general-uvc" {
		t.Error(got)
	}
	p, i, ok := SplitByIDName("usb-Foo_Bar-video-index12")
	if !ok || p != "usb-Foo_Bar" || i != 12 {
		t.Error(p, i, ok)
	}
	if _, _, ok := SplitByIDName("usb-Foo-event-if00"); ok {
		t.Error("should not match")
	}
}

func mk(t *testing.T, p string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeTree builds a fake /dev, /sys and /proc with one two-function USB camera
// (video0 = MJPEG, video1 = metadata, video2 = H264, with sound card1 "Camera")
// plus one MJPEG-only camera without a microphone.
func fakeTree(t *testing.T) *Discoverer {
	root := t.TempDir()
	dev := filepath.Join(root, "dev")
	sys := filepath.Join(root, "sys")
	proc := filepath.Join(root, "proc/asound")
	byid := filepath.Join(dev, "v4l/by-id")
	if err := os.MkdirAll(byid, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"video0", "video1", "video2", "video3"} {
		mk(t, filepath.Join(dev, n), "")
	}
	link := func(name, target string) {
		if err := os.Symlink(target, filepath.Join(byid, name)); err != nil {
			t.Fatal(err)
		}
	}
	link("usb-Acme_Cam_42-video-index0", "../../video0")
	link("usb-Acme_Cam_42-video-index1", "../../video1")
	link("usb-Acme_Cam_42-video-index2", "../../video2")
	link("usb-Other_Thing-video-index0", "../../video3")

	// USB device 1-1 with video interface 1.0 and audio interface 1.2
	usb1 := filepath.Join(sys, "devices/usb1/1-1")
	mk(t, filepath.Join(usb1, "1-1:1.0/placeholder"), "")
	if err := os.MkdirAll(filepath.Join(usb1, "1-1:1.2/sound/card1"), 0o755); err != nil {
		t.Fatal(err)
	}
	usb2 := filepath.Join(sys, "devices/usb1/1-2")
	mk(t, filepath.Join(usb2, "1-2:1.0/placeholder"), "")
	for n, ifc := range map[string]string{"video0": usb1, "video1": usb1, "video2": usb1, "video3": usb2} {
		d := filepath.Join(sys, "class/video4linux", n)
		mk(t, filepath.Join(d, "name"), "Acme Cam: Acme Cam \n")
		if err := os.Symlink(filepath.Join(ifc, "1-"+map[bool]string{true: "1", false: "2"}[ifc == usb1]+":1.0"), filepath.Join(d, "device")); err != nil {
			t.Fatal(err)
		}
	}
	mk(t, filepath.Join(sys, "class/video4linux/video3/name"), "Other Thing\n")
	mk(t, filepath.Join(proc, "card1/id"), "Camera\n")

	formats := map[string][]protocol.VideoFormat{
		"video0": {{Codec: "mjpeg", Width: 1280, Height: 720, FPS: []int{30}}},
		"video1": nil,
		"video2": {{Codec: "h264", Width: 1280, Height: 720, FPS: []int{30}}},
		"video3": {{Codec: "mjpeg", Width: 640, Height: 480, FPS: []int{30}}},
	}
	return &Discoverer{
		ByIDDir: byid, SysRoot: sys, ProcAsound: proc,
		Query: func(p string) ([]protocol.VideoFormat, error) {
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				return nil, err
			}
			return formats[filepath.Base(real)], nil
		},
	}
}

func TestDiscoverGroupsNodes(t *testing.T) {
	d := fakeTree(t)
	cams := d.Discover()
	if len(cams) != 2 {
		t.Fatalf("got %d cameras: %+v", len(cams), cams)
	}
	c := cams[0]
	if c.ID != "usb-acme-cam-42" || c.Name != "Acme Cam" {
		t.Errorf("id/name %q %q", c.ID, c.Name)
	}
	byid := d.ByIDDir
	wantNodes := map[string]string{
		"mjpeg": filepath.Join(byid, "usb-Acme_Cam_42-video-index0"),
		"h264":  filepath.Join(byid, "usb-Acme_Cam_42-video-index2"),
	}
	if !reflect.DeepEqual(c.Nodes, wantNodes) {
		t.Errorf("nodes %v", c.Nodes)
	}
	if c.VideoDevice != wantNodes["h264"] {
		t.Errorf("primary %q", c.VideoDevice)
	}
	if len(c.Formats) != 2 {
		t.Errorf("formats %v", c.Formats)
	}
	if c.AudioDevice != "hw:CARD=Camera,DEV=0" {
		t.Errorf("audio %q", c.AudioDevice)
	}
	if cams[1].ID != "usb-other-thing" || cams[1].AudioDevice != "" {
		t.Errorf("second camera %+v", cams[1])
	}
}

func TestFindALSANoSound(t *testing.T) {
	d := fakeTree(t)
	if got := d.FindALSA("/dev/video3"); got != "" {
		t.Errorf("got %q", got)
	}
	if got := d.FindALSA("/dev/video0"); got != "hw:CARD=Camera,DEV=0" {
		t.Errorf("got %q", got)
	}
}

func TestDetectHardware(t *testing.T) {
	root := t.TempDir()
	mk(t, filepath.Join(root, "sys/class/video4linux/video10/name"), "bcm2835-codec-decode\n")
	mk(t, filepath.Join(root, "sys/class/video4linux/video11/name"), "bcm2835-codec-encode\n")
	mk(t, filepath.Join(root, "sys/class/video4linux/video0/name"), "Some Cam\n")
	mk(t, filepath.Join(root, "model"), "Raspberry Pi 3 Model B Rev 1.2\x00")
	hw := DetectHardware(filepath.Join(root, "sys"), filepath.Join(root, "model"), 1234)
	want := protocol.Hardware{Model: "Raspberry Pi 3 Model B Rev 1.2", HWH264Encoder: true, HWMJPEGDecoder: true, MaxUploadKbps: 1234}
	if hw != want {
		t.Errorf("%+v", hw)
	}
	hw = DetectHardware(filepath.Join(root, "nosys"), filepath.Join(root, "nomodel"), 0)
	if hw.HWH264Encoder || hw.Model != "" {
		t.Errorf("%+v", hw)
	}
}

func TestApplyOverrides(t *testing.T) {
	in := []Discovered{
		{Camera: protocol.Camera{ID: "a", Name: "A", AudioDevice: "hw:1"}},
		{Camera: protocol.Camera{ID: "b", Name: "B"}},
		{Camera: protocol.Camera{ID: "c", Name: "C", AudioDevice: "hw:3"}},
	}
	out := ApplyOverrides(in, map[string]CameraConfig{
		"a": {Name: "Garden", AudioDevice: "none"},
		"b": {Disabled: true},
		"c": {AudioDevice: "hw:CARD=X,DEV=0"},
	})
	if len(out) != 2 || out[0].Name != "Garden" || out[0].AudioDevice != "" || out[1].AudioDevice != "hw:CARD=X,DEV=0" {
		t.Errorf("%+v", out)
	}
}
