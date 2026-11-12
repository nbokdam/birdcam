package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(`
devices:
  - id: pi1
    key: secret
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Ingest.PortMin != 9001 || c.Ingest.PortMax != 9010 || c.WebRTC.UDPPort != 8189 {
		t.Fatalf("defaults: %+v", c)
	}
	if p := c.DefaultProfile; p.Width != 1280 || p.Height != 720 || p.FPS != 30 || p.Audio == nil || !*p.Audio {
		t.Fatalf("default profile: %+v", p)
	}
	if c.Devices[0].Name != "pi1" || len(c.Ports()) != 10 {
		t.Fatalf("device name / ports: %+v %v", c.Devices[0], c.Ports())
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"devices: [{id: a}]", "missing key"},
		{"devices: [{id: a, key: k}, {id: a, key: k}]", "duplicate id"},
		{"ingest: {port_min: 9010, port_max: 9001}", "invalid port range"},
		{"webrtc: {public_ips: [nope]}", "invalid public ip"},
		{"cameras: [{device_id: x, camera_id: y}]", "unknown device"},
		{"ingest: {public_host: \"node.example.com:9001\"}", "public_host"},
		{"ingest: {public_host: \"https://node.example.com\"}", "public_host"},
	} {
		_, err := Parse([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.yaml, tc.want, err)
		}
	}
}

func TestExampleConfig(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "../../../deploy/server.example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skip("example config not found")
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Devices) == 0 {
		t.Fatal("example has no devices")
	}
	o, ok := c.Camera(c.Cameras[0].DeviceID, c.Cameras[0].CameraID)
	if !ok || o.Name == "" {
		t.Fatalf("camera override lookup failed: %+v", o)
	}
}

func TestAuthAndAccessConfig(t *testing.T) {
	c, err := Parse([]byte(`
external_url: https://birdcam.example.com/
hosts: [{host: maci.example, default_device: pi1}]
auth:
  oidc: {issuer: https://auth.example.com, client_id: birdcam, client_secret: x}
  session_ttl: 12h
devices:
  - {id: pi1, key: k, groups: [family]}
cameras:
  - {device_id: pi1, camera_id: a, rotation: 0}
  - {device_id: pi1, camera_id: b, groups: [admins]}
  - {device_id: pi1, camera_id: c, public: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Auth.OIDC
	if !c.Auth.Enabled() || o.RedirectURL != "" || o.GroupsClaim != "groups" ||
		o.TokenAuthMethod != ClientSecretBasic || len(o.Scopes) != 4 || c.Auth.SessionTTL != 12*time.Hour {
		t.Fatalf("auth defaults: %+v ttl=%v", o, c.Auth.SessionTTL)
	}
	if !c.Ingest.TLSEnabled() || c.StateDir != "/var/lib/birdcam" {
		t.Fatalf("ingest tls default / state dir: %v %q", c.Ingest.TLSEnabled(), c.StateDir)
	}
	if r := c.Cameras[0].Rotation; r == nil || *r != 0 {
		t.Fatalf("rotation 0 must be kept as an override: %v", r)
	}
	for cam, want := range map[string][]string{"a": {"family"}, "b": {"admins"}, "c": nil, "other": {"family"}} {
		if got := c.CameraAccess("pi1", cam); strings.Join(got, ",") != strings.Join(want, ",") || (want == nil) != (got == nil) {
			t.Errorf("access %s: %v, want %v", cam, got, want)
		}
	}
	if c.HostDefaultDevice("MACI.example:443") != "pi1" || c.HostDefaultDevice("x") != "" {
		t.Error("host mapping")
	}
	if k, err := ParseKey(strings.Repeat("ab", 32)); err != nil || len(k) != 32 {
		t.Error("hex key")
	}
}

func TestValidateNewKeys(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"devices: [{id: a, key: k}]\ncameras: [{device_id: a, camera_id: c, rotation: 45}]", "rotation"},
		{"devices: [{id: a, key: k, night_grayscale: x}]", "night_grayscale"},
		{"devices: [{id: a, key: k}]\ncameras: [{device_id: a, camera_id: c, night_grayscale: ON}]", "night_grayscale"},
		{"hosts: [{host: h, default_device: nope}]", "unknown device"},
		{"auth: {oidc: {issuer: https://i}}", "client_id"},
		{"auth: {oidc: {issuer: https://i, client_id: c, client_secret: s}}", "external_url"},
		{"external_url: https://x\nauth: {oidc: {issuer: https://i, client_id: c, client_secret: s, token_auth_method: nope}}", "token_auth_method"},
		{"auth: {session_key: short}", "session_key"},
		{"external_url: ftp://x", "external_url"},
		{"ingest: {cert_file: /a}", "together"},
	} {
		_, err := Parse([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.yaml, tc.want, err)
		}
	}
}

func TestNightGrayscaleInheritance(t *testing.T) {
	c, err := Parse([]byte(`
devices:
  - {id: a, key: k, night_grayscale: auto}
  - {id: b, key: k}
cameras:
  - {device_id: a, camera_id: off, night_grayscale: "off"}
  - {device_id: a, camera_id: on, night_grayscale: "on"}
  - {device_id: b, camera_id: auto, night_grayscale: auto}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ dev, cam, want string }{
		{"a", "none", "auto"}, {"a", "off", "off"}, {"a", "on", "on"},
		{"b", "none", "off"}, {"b", "auto", "auto"}, {"zz", "x", "off"},
	} {
		if got := c.NightGrayscaleFor(tc.dev, tc.cam); got != tc.want {
			t.Errorf("%s/%s: got %q, want %q", tc.dev, tc.cam, got, tc.want)
		}
	}
}

func TestIngestPublicHostAndAgentUpdates(t *testing.T) {
	c, err := Parse([]byte("ingest: {public_host: 203.0.113.7}"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Ingest.PublicHost != "203.0.113.7" || c.AgentUpdates.Dir != "/usr/share/birdcam/agents" || c.AgentUpdates.Disabled {
		t.Fatalf("%+v %+v", c.Ingest, c.AgentUpdates)
	}
	for _, h := range []string{"node1.example.com", "2001:db8::1", ""} {
		if _, err := Parse([]byte("ingest: {public_host: \"" + h + "\"}")); err != nil {
			t.Errorf("public_host %q rejected: %v", h, err)
		}
	}
	c, err = Parse([]byte("agent_updates: {dir: /srv/agents, disabled: true}"))
	if err != nil || c.AgentUpdates.Dir != "/srv/agents" || !c.AgentUpdates.Disabled {
		t.Fatalf("%+v %v", c.AgentUpdates, err)
	}
}
