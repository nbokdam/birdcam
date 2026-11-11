package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const stream0Camera = `USB Camera Manufacture H264 USB Camera at usb-3f980000.usb-1.4, high speed : USB Audio

Capture:
  Status: Stop
  Interface 4
    Altset 1
    Format: S16_LE
    Channels: 1
    Endpoint: 0x84 (4 IN) (ASYNC)
    Rates: 8000, 11025, 16000, 22050, 24000, 44100, 48000
`

func TestALSACardID(t *testing.T) {
	for in, want := range map[string]string{"hw:CARD=Camera,DEV=0": "Camera", "plughw:CARD=UVC": "UVC", "default": ""} {
		if got := ALSACardID(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestCaptureRates(t *testing.T) {
	rates := ParseCaptureRates(stream0Camera)
	if !reflect.DeepEqual(rates, []int{8000, 11025, 16000, 22050, 24000, 44100, 48000}) {
		t.Fatalf("rates %v", rates)
	}
	if PickAudioRate(rates) != 48000 || PickAudioRate([]int{16000, 32000}) != 32000 || PickAudioRate(nil) != 48000 {
		t.Error("PickAudioRate")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "UVC"), 0o755)
	os.WriteFile(filepath.Join(dir, "UVC", "stream0"), []byte("x\n\nCapture:\n    Rates: 32000\n"), 0o644)
	if r := CaptureRate(dir, "hw:CARD=UVC,DEV=0"); r != 32000 {
		t.Errorf("CaptureRate = %d", r)
	}
	if r := CaptureRate(dir, "hw:CARD=Missing,DEV=0"); r != 48000 {
		t.Errorf("CaptureRate missing = %d", r)
	}
}
