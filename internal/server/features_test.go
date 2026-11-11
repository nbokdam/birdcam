package server

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"birdcam/internal/fakeagent"
	"birdcam/internal/protocol"
	"birdcam/internal/server/config"
	"birdcam/internal/server/ingest"
)

func TestHostsDefaultDevice(t *testing.T) {
	for _, trust := range []bool{false, true} {
		e := newTestEnvWith(t, 1, false, func(c *config.Config) {
			c.TrustProxyHeaders = trust
			c.Hosts = []config.Host{{Host: "Maci.Birdcam.example", DefaultDevice: "dev2"}, {Host: "garden.example", DefaultDevice: "dev1"}}
		})
		for _, tc := range []struct {
			host, fwd, want string
		}{
			{"maci.birdcam.example", "", "dev2"},
			{"MACI.birdcam.example:8443", "", "dev2"},
			{"garden.example.", "", "dev1"},
			{"other.example", "", ""},
			{"other.example", "maci.birdcam.example, proxy", map[bool]string{true: "dev2", false: ""}[trust]},
			{"maci.birdcam.example", "other.example", map[bool]string{true: "", false: "dev2"}[trust]},
		} {
			req, _ := http.NewRequest(http.MethodGet, e.hs.URL+"/api/session", nil)
			req.Host = tc.host
			if tc.fwd != "" {
				req.Header.Set("X-Forwarded-Host", tc.fwd)
			}
			var si SessionInfo
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			decodeJSON(t, resp, &si)
			if si.DefaultDeviceID != tc.want {
				t.Errorf("trust=%v host=%q fwd=%q: default_device_id=%q, want %q", trust, tc.host, tc.fwd, si.DefaultDeviceID, tc.want)
			}
		}
	}
}

func TestRotationAndNames(t *testing.T) {
	r0, r270 := 0, 270
	e := newTestEnvWith(t, 1, false, func(c *config.Config) {
		c.Devices[0].NightGrayscale = config.NightAuto
		c.Cameras = append(c.Cameras,
			config.CameraOverride{DeviceID: "dev1", CameraID: "zero", Rotation: &r0, Name: "Configured", NightGrayscale: config.NightOn},
			config.CameraOverride{DeviceID: "dev1", CameraID: "over", Rotation: &r270})
	})
	a, _, _ := e.runAgent(fakeagent.Config{NoStream: true, Cameras: []protocol.Camera{
		{ID: "agent", Name: "Agent name", Rotation: 90}, // agent value used
		{ID: "zero", Name: "Agent zero", Rotation: 180}, // overridden to 0
		{ID: "over", Rotation: 90},                      // overridden to 270; name falls back to id
		{ID: "bogus", Name: "B", Rotation: 45},          // invalid → 0
	}})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "cameras", 2*time.Second, func() bool { return len(e.s.Cameras()) == 4 })
	want := map[string]struct {
		rot  int
		name string
	}{"agent": {90, "Agent name"}, "zero": {0, "Configured"}, "over": {270, "over"}, "bogus": {0, "B"}}
	var cams []CameraInfo
	resp, err := http.Get(e.hs.URL + "/api/cameras")
	if err != nil {
		t.Fatal(err)
	}
	decodeJSON(t, resp, &cams)
	for _, c := range cams {
		wantNight := config.NightAuto
		if c.CameraID == "zero" {
			wantNight = config.NightOn
		}
		if c.NightGrayscale != wantNight {
			t.Errorf("%s: night_grayscale=%q, want %q", c.CameraID, c.NightGrayscale, wantNight)
		}
		w := want[c.CameraID]
		if c.Rotation != w.rot || c.Name != w.name || c.DeviceName != "Garden" || !c.Public {
			t.Errorf("%s: rotation=%d name=%q device=%q public=%v, want %d %q", c.CameraID, c.Rotation, c.Name, c.DeviceName, c.Public, w.rot, w.name)
		}
	}
	// Device name defaults to the id.
	b, _, _ := e.runAgent(fakeagent.Config{NoStream: true, DeviceID: "dev2", Key: "key2"})
	waitEvent(t, b, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "dev2 camera", 2*time.Second, func() bool { c, ok := e.camera("dev2", "fakecam"); return ok && c.DeviceName == "dev2" })
}

func TestBandwidthBudgetSplit(t *testing.T) {
	e := newTestEnvWith(t, 4, false, func(c *config.Config) {
		c.Devices[0].MaxKbps = 3000
		c.Cameras = append(c.Cameras, config.CameraOverride{DeviceID: "dev1", CameraID: "c3", MaxKbps: 500})
	})
	a, _, _ := e.runAgent(fakeagent.Config{NoStream: true, Cameras: []protocol.Camera{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}}})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "cameras", 2*time.Second, func() bool { return len(e.s.Cameras()) == 3 })

	e.addViewer("dev1", "c1")
	if st := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start; st.CameraID != "c1" || st.Profile.MaxKbps != 3000 {
		t.Fatalf("single streaming camera must get the whole budget: %+v", st)
	}
	e.addViewer("dev1", "c2")
	if st := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start; st.CameraID != "c2" || st.Profile.MaxKbps != 1500 {
		t.Fatalf("second camera: %+v", st)
	}
	e.addViewer("dev1", "c3")
	if st := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start; st.CameraID != "c3" || st.Profile.MaxKbps != 500 {
		t.Fatalf("per-camera max_kbps must cap the share (1000): %+v", st)
	}
	// Unlimited device: the per-camera value applies as is.
	e2 := newTestEnvWith(t, 2, false, func(c *config.Config) {
		c.Cameras = append(c.Cameras, config.CameraOverride{DeviceID: "dev1", CameraID: "fakecam", MaxKbps: 800})
	})
	b, _, _ := e2.runAgent(fakeagent.Config{NoStream: true})
	waitEvent(t, b, protocol.TypeWelcome, 2*time.Second)
	e2.addViewer("dev1", "fakecam")
	if st := waitEvent(t, b, protocol.TypeStart, 2*time.Second).Start; st.Profile.MaxKbps != 800 {
		t.Fatalf("unlimited device: %+v", st)
	}
}

func TestIngestTLS(t *testing.T) {
	e := newTestEnv(t, 2, false)
	a, _, _ := e.runAgent(fakeagent.Config{NoStream: true})
	wm := waitEvent(t, a, protocol.TypeWelcome, 2*time.Second).Welcome
	if len(wm.IngestCertSHA256) != 64 || wm.IngestCertSHA256 != e.s.ingestFingerprint {
		t.Fatalf("welcome fingerprint %q (server %q)", wm.IngestCertSHA256, e.s.ingestFingerprint)
	}
	e.addViewer("dev1", "fakecam")
	st := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start

	// Plain TCP is rejected even with a valid preamble.
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(st.Port))
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte(protocol.IngestPreamble(st.SessionID, st.Token)))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.Copy(io.Discard, conn) // until the server closes it
	conn.Close()
	if c, _ := e.camera("dev1", "fakecam"); c.Streaming {
		t.Fatal("plain TCP connection accepted")
	}

	// A client pinning a different fingerprint refuses the server.
	wrong := "00" + e.s.ingestFingerprint[2:]
	if c, err := tls.Dial("tcp", "127.0.0.1:"+strconv.Itoa(st.Port), ingest.PinnedClientConfig(wrong)); err == nil {
		c.Close()
		t.Fatal("wrong pin accepted")
	}

	// The fake agent streams over TLS (pin from welcome).
	e2 := newTestEnv(t, 2, false)
	b, _, _ := e2.runAgent(fakeagent.Config{})
	waitEvent(t, b, protocol.TypeWelcome, 2*time.Second)
	e2.addViewer("dev1", "fakecam")
	waitFor(t, "streaming over TLS", 3*time.Second, func() bool { c, _ := e2.camera("dev1", "fakecam"); return c.Streaming })

	// The same state dir yields the same certificate after a restart.
	_, fp, err := ingest.LoadOrCreateCert("", "", e2.s.cfg.StateDir)
	if err != nil || fp != e2.s.ingestFingerprint {
		t.Fatalf("persisted cert: %s %v, want %s", fp, err, e2.s.ingestFingerprint)
	}
}

func TestIngestPlainWhenTLSDisabled(t *testing.T) {
	off := false
	e := newTestEnvWith(t, 2, false, func(c *config.Config) { c.Ingest.TLS = &off })
	a, _, _ := e.runAgent(fakeagent.Config{})
	if wm := waitEvent(t, a, protocol.TypeWelcome, 2*time.Second).Welcome; wm.IngestCertSHA256 != "" {
		t.Fatalf("fingerprint sent with TLS disabled: %q", wm.IngestCertSHA256)
	}
	e.addViewer("dev1", "fakecam")
	waitFor(t, "streaming over plain TCP", 3*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return c.Streaming })
}

// TestFakeAgentWrongPin: an agent pinning another certificate cannot stream.
func TestFakeAgentWrongPin(t *testing.T) {
	e := newTestEnv(t, 2, false)
	a, _, _ := e.runAgent(fakeagent.Config{IngestFingerprint: "ab" + e.s.ingestFingerprint[2:]})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	e.addViewer("dev1", "fakecam")
	waitEvent(t, a, protocol.TypeStart, 2*time.Second)
	// The agent reports failure; the server restarts the session.
	waitFor(t, "failure reported", 3*time.Second, func() bool {
		c, _ := e.camera("dev1", "fakecam")
		return c.Error != "" && !c.Streaming
	})
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}
