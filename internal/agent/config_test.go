package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"birdcam/internal/protocol"
)

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig([]byte(`
server_url: wss://birds.example.org/agent
device_id: pi1
key: secret
max_upload_kbps: 3000
cameras:
  usb-foo:
    name: Garden
    disabled: false
    audio_device: none
    preferred_mode: mjpeg-hwenc
    native_kbps: 4000
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.GstLaunch != "gst-launch-1.0" || c.ServerHost() != "birds.example.org" || c.MaxUploadKbps != 3000 {
		t.Errorf("%+v", c)
	}
	if c.NativeKbps("usb-foo") != 4000 || c.NativeKbps("other") != DefaultNativeKbps {
		t.Error("native kbps")
	}
}

func TestParseConfigErrors(t *testing.T) {
	for _, y := range []string{
		"server_url: http://x/agent\ndevice_id: a\nkey: b",
		"server_url: ws://x/agent\nkey: b",
		"server_url: ws://x/agent\ndevice_id: a",
		"server_url: ws://x/agent\ndevice_id: a\nkey: b\nbogus: 1",
		"server_url: ws://x/agent\ndevice_id: a\nkey: b\ncameras:\n  c:\n    preferred_mode: zzz",
	} {
		if _, err := ParseConfig([]byte(y)); err == nil {
			t.Errorf("expected error for %q", y)
		}
	}
}

func TestConfigRotation(t *testing.T) {
	base := "server_url: ws://h/agent\ndevice_id: d\nkey: k\nrequire_ingest_tls: true\ncameras:\n  cam:\n    rotation: "
	for _, r := range []string{"0", "90", "180", "270"} {
		c, err := ParseConfig([]byte(base + r))
		if err != nil {
			t.Fatalf("rotation %s: %v", r, err)
		}
		if !c.RequireIngestTLS {
			t.Error("require_ingest_tls not parsed")
		}
	}
	for _, r := range []string{"45", "-90", "360"} {
		if _, err := ParseConfig([]byte(base + r)); err == nil {
			t.Errorf("rotation %s accepted", r)
		}
	}
}

func TestRotationInHelloAndCameras(t *testing.T) {
	cams := []Discovered{
		{Camera: protocol.Camera{ID: "a", Name: "A"}},
		{Camera: protocol.Camera{ID: "b", Name: "B"}},
	}
	out := ApplyOverrides(cams, map[string]CameraConfig{"a": {Rotation: 180}})
	a := &Agent{cfg: &Config{DeviceID: "d", Key: "k"}, cameras: out}
	h := a.hello()
	if h.Cameras[0].Rotation != 180 || h.Cameras[1].Rotation != 0 {
		t.Errorf("hello rotations: %+v", h.Cameras)
	}
	raw, _ := json.Marshal(protocol.Cameras{Cameras: ProtocolCameras(out)})
	if !strings.Contains(string(raw), `"rotation":180`) {
		t.Errorf("cameras json %s", raw)
	}
}
