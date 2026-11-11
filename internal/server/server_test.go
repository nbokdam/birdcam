package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"

	"birdcam/internal/fakeagent"
	"birdcam/internal/protocol"
	"birdcam/internal/server/config"
	"birdcam/internal/server/ingest"
	"birdcam/internal/server/media"
)

func testTiming() *Timing {
	return &Timing{
		HelloTimeout:   2 * time.Second,
		PingInterval:   200 * time.Millisecond,
		DeadAfter:      2 * time.Second,
		StopLinger:     300 * time.Millisecond,
		StartTimeout:   time.Second,
		RTPTimeout:     time.Second,
		MaxDrift:       300 * time.Millisecond,
		WatchdogTick:   50 * time.Millisecond,
		ViewerGrace:    time.Second,
		ViewerConnect:  5 * time.Second,
		BackoffMin:     100 * time.Millisecond,
		BackoffMax:     500 * time.Millisecond,
		HealthyAfter:   5 * time.Second,
		NoPortRetry:    200 * time.Millisecond,
		ICEGatherLimit: 5 * time.Second,
	}
}

func testLogger(t *testing.T) *slog.Logger {
	if os.Getenv("BIRDCAM_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type testEnv struct {
	t   *testing.T
	s   *Server
	hs  *httptest.Server
	url string // ws://.../agent
}

func newTestEnv(t *testing.T, ports int, webrtc bool) *testEnv {
	return newTestEnvWith(t, ports, webrtc, nil)
}

// newTestEnvWith is newTestEnv with a hook to adjust the config. The HTTP
// listener exists before the hook runs, so cfg.ExternalURL is already set to
// its URL.
func newTestEnvWith(t *testing.T, ports int, webrtc bool, mod func(*config.Config)) *testEnv {
	t.Helper()
	hs := httptest.NewUnstartedServer(nil)
	cfg := &config.Config{
		ExternalURL: "http://" + hs.Listener.Addr().String(),
		Devices:     []config.Device{{ID: "dev1", Key: "key1", Name: "Garden"}, {ID: "dev2", Key: "key2"}},
		Cameras:     []config.CameraOverride{{DeviceID: "dev1", CameraID: "hiddencam", Hidden: true}},
	}
	cfg.StateDir = t.TempDir()
	if mod != nil {
		mod(cfg)
	}
	cfg.ApplyDefaults()
	cfg.Ingest.Bind = "127.0.0.1"
	cfg.WebRTC.IncludeLoopback = true
	cfg.WebRTC.UDPPort = freeUDPPort(t)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Config:        cfg,
		Logger:        testLogger(t),
		Timing:        testTiming(),
		IngestPorts:   make([]int, ports),
		DisableWebRTC: !webrtc,
	})
	if err != nil {
		hs.Close()
		t.Fatal(err)
	}
	hs.Config.Handler = s.Handler()
	hs.Start()
	t.Cleanup(func() {
		hs.CloseClientConnections()
		s.Close()
		hs.Close()
	})
	return &testEnv{t: t, s: s, hs: hs, url: "ws" + strings.TrimPrefix(hs.URL, "http") + "/agent"}
}

// dialIngest connects to an ingest port with TLS, pinning the server's
// certificate fingerprint.
func (e *testEnv) dialIngest(port int) net.Conn {
	e.t.Helper()
	conn, err := tls.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port), ingest.PinnedClientConfig(e.s.ingestFingerprint))
	if err != nil {
		e.t.Fatal(err)
	}
	return conn
}

func freeUDPPort(t *testing.T) int {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// runAgent starts a fake agent; the returned cancel disconnects it.
func (e *testEnv) runAgent(cfg fakeagent.Config) (*fakeagent.Agent, context.CancelFunc, <-chan error) {
	if cfg.ServerURL == "" {
		cfg.ServerURL = e.url
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID, cfg.Key = "dev1", "key1"
	}
	cfg.Log = testLogger(e.t)
	a := fakeagent.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	e.t.Cleanup(cancel)
	return a, cancel, done
}

func waitEvent(t *testing.T, a *fakeagent.Agent, typ string, timeout time.Duration) fakeagent.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-a.Events:
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", typ)
		}
	}
}

func noEvent(t *testing.T, a *fakeagent.Agent, typ string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev := <-a.Events:
			if ev.Type == typ {
				t.Fatalf("unexpected %q event: %+v", typ, ev)
			}
		case <-deadline:
			return
		}
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *testEnv) camera(dev, cam string) (CameraInfo, bool) {
	for _, c := range e.s.Cameras() {
		if c.DeviceID == dev && c.CameraID == cam {
			return c, true
		}
	}
	return CameraInfo{}, false
}

func (e *testEnv) addViewer(dev, cam string) (*viewer, error) {
	v := &viewer{id: randHex(8), s: e.s, log: e.s.log}
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	_, err := e.s.addViewerLocked(v, dev, cam)
	return v, err
}

func (e *testEnv) removeViewer(v *viewer) {
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	e.s.removeViewerLocked(v)
}

func (e *testEnv) hub(dev, cam string) *media.Hub {
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	return e.s.devices[dev].cameras[cam].hub
}

// counter is a concurrency-safe RTPWriter.
type counter struct {
	n        atomic.Int64
	mu       sync.Mutex
	firstPay []byte
}

func (c *counter) WriteRTP(p *rtp.Packet) error {
	if c.n.Add(1) == 1 {
		c.mu.Lock()
		c.firstPay = append([]byte(nil), p.Payload...)
		c.mu.Unlock()
	}
	return nil
}

func TestAgentAuth(t *testing.T) {
	e := newTestEnv(t, 2, false)
	for _, tc := range []fakeagent.Config{
		{DeviceID: "dev1", Key: "wrong"},
		{DeviceID: "nope", Key: "key1"},
		{DeviceID: "dev1", Key: "key1", ProtocolVersion: 99},
	} {
		a, _, done := e.runAgent(tc)
		ev := waitEvent(t, a, protocol.TypeError, 2*time.Second)
		if err := <-done; !errors.Is(err, fakeagent.ErrRejected) {
			t.Fatalf("%+v: want ErrRejected, got %v (%s)", tc, err, ev.Error)
		}
	}
	a, _, _ := e.runAgent(fakeagent.Config{})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "camera online", 2*time.Second, func() bool { c, ok := e.camera("dev1", "fakecam"); return ok && c.Online })
}

func TestSessionLifecycle(t *testing.T) {
	e := newTestEnv(t, 2, false)
	a, _, _ := e.runAgent(fakeagent.Config{})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "camera", 2*time.Second, func() bool { _, ok := e.camera("dev1", "fakecam"); return ok })

	if _, err := e.addViewer("dev1", "hiddencam"); !errors.Is(err, errUnknownCamera) {
		t.Fatalf("hidden camera: %v", err)
	}
	if _, err := e.addViewer("dev1", "nope"); !errors.Is(err, errUnknownCamera) {
		t.Fatalf("unknown camera: %v", err)
	}

	cnt := &counter{}
	e.hub("dev1", "fakecam").Add(media.NewViewer(cnt, nil))
	v1, err := e.addViewer("dev1", "fakecam")
	if err != nil {
		t.Fatal(err)
	}
	st := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
	if st.CameraID != "fakecam" || st.Token == "" || st.SessionID == "" || st.Profile.Width != 1280 || !st.Profile.Audio {
		t.Fatalf("bad start: %+v", st)
	}
	waitFor(t, "streaming", 3*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return c.Streaming })
	waitFor(t, "video packets at hub", 2*time.Second, func() bool { return cnt.n.Load() > 20 })
	cnt.mu.Lock()
	if info := media.ParseH264(cnt.firstPay); !info.IDRStart && info.SPS == nil {
		t.Errorf("first forwarded packet is not a keyframe: %x", cnt.firstPay[:4])
	}
	cnt.mu.Unlock()
	c, _ := e.camera("dev1", "fakecam")
	if c.Stats == nil || c.Stats.VideoPackets == 0 || c.Stats.AudioPackets == 0 || c.Viewers != 1 {
		t.Fatalf("stats: %+v %+v", c, c.Stats)
	}

	// Second viewer: no second start.
	v2, _ := e.addViewer("dev1", "fakecam")
	noEvent(t, a, protocol.TypeStart, 200*time.Millisecond)
	e.removeViewer(v1)
	e.removeViewer(v2)
	// Viewer comes back within the linger time: the session continues.
	time.Sleep(100 * time.Millisecond)
	v3, _ := e.addViewer("dev1", "fakecam")
	noEvent(t, a, protocol.TypeStop, 400*time.Millisecond)
	e.removeViewer(v3)
	stop := waitEvent(t, a, protocol.TypeStop, 2*time.Second).Stop
	if stop.SessionID != st.SessionID {
		t.Fatalf("stop for wrong session: %+v", stop)
	}
	if n := e.s.pool.InUse(); n != 0 {
		t.Fatalf("ports in use after stop: %d", n)
	}
	waitFor(t, "idle", 2*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return c.State == StateIdle })
}

func TestAgentDisconnectReconnect(t *testing.T) {
	e := newTestEnv(t, 2, false)
	a, cancel, done := e.runAgent(fakeagent.Config{})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	if _, err := e.addViewer("dev1", "fakecam"); err != nil {
		t.Fatal(err)
	}
	st1 := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
	waitFor(t, "streaming", 3*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return c.Streaming })

	cancel()
	<-done
	waitFor(t, "offline", 2*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return !c.Online })
	c, _ := e.camera("dev1", "fakecam")
	if c.Viewers != 1 || c.State != StateOffline || c.Stats != nil {
		t.Fatalf("after disconnect: %+v", c)
	}
	if n := e.s.pool.InUse(); n != 0 {
		t.Fatalf("ports in use after agent disconnect: %d", n)
	}

	b, _, _ := e.runAgent(fakeagent.Config{})
	st2 := waitEvent(t, b, protocol.TypeStart, 2*time.Second).Start
	if st2.SessionID == st1.SessionID || st2.Token == st1.Token {
		t.Fatal("reconnect must use a fresh session")
	}
	waitFor(t, "streaming again", 3*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return c.Streaming })
}

func TestDuplicateAgentReplacesOld(t *testing.T) {
	e := newTestEnv(t, 2, false)
	a, _, doneA := e.runAgent(fakeagent.Config{NoStream: true})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	e.addViewer("dev1", "fakecam")
	waitEvent(t, a, protocol.TypeStart, 2*time.Second)

	b, _, _ := e.runAgent(fakeagent.Config{})
	waitEvent(t, b, protocol.TypeWelcome, 2*time.Second)
	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatal("old connection not closed")
	}
	waitEvent(t, b, protocol.TypeStart, 2*time.Second)
	waitFor(t, "streaming via new connection", 3*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return c.Streaming })
	if n := e.s.pool.InUse(); n != 1 {
		t.Fatalf("ports in use: %d, want 1", n)
	}
}

func TestWatchdogNoIngest(t *testing.T) {
	e := newTestEnv(t, 2, false)
	a, _, _ := e.runAgent(fakeagent.Config{NoStream: true})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	e.addViewer("dev1", "fakecam")
	st1 := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
	stop := waitEvent(t, a, protocol.TypeStop, 3*time.Second).Stop
	if stop.SessionID != st1.SessionID {
		t.Fatal("stop for wrong session")
	}
	st2 := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
	if st2.SessionID == st1.SessionID {
		t.Fatal("expected fresh session")
	}
	c, _ := e.camera("dev1", "fakecam")
	if !strings.Contains(c.Error, "no ingest connection") {
		t.Fatalf("error = %q", c.Error)
	}
}

func TestWatchdogNoRTP(t *testing.T) {
	e := newTestEnv(t, 2, false)
	a, _, _ := e.runAgent(fakeagent.Config{NoStream: true})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	e.addViewer("dev1", "fakecam")
	st := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
	// Connect and send the preamble but no RTP.
	conn := e.dialIngest(st.Port)
	defer conn.Close()
	conn.Write([]byte(protocol.IngestPreamble(st.SessionID, st.Token)))
	stop := waitEvent(t, a, protocol.TypeStop, 3*time.Second).Stop
	if stop.SessionID != st.SessionID {
		t.Fatal("stop for wrong session")
	}
	c, _ := e.camera("dev1", "fakecam")
	if !strings.Contains(c.Error, "no RTP") {
		t.Fatalf("error = %q", c.Error)
	}
	// The ingest connection of the stopped session is closed by the server.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("ingest conn not closed: %v", err)
	}
}

func TestWatchdogDrift(t *testing.T) {
	e := newTestEnv(t, 2, false)
	// Audio clock runs 50% fast: drift grows 0.5 s per second.
	a, _, _ := e.runAgent(fakeagent.Config{AudioRate: 1.5})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	e.addViewer("dev1", "fakecam")
	st1 := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
	waitEvent(t, a, protocol.TypeStop, 3*time.Second)
	st2 := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
	if st2.SessionID == st1.SessionID {
		t.Fatal("expected fresh session")
	}
	c, _ := e.camera("dev1", "fakecam")
	if !strings.Contains(c.Error, "drift") {
		t.Fatalf("error = %q", c.Error)
	}
}

func TestStreamStatusFailedRetries(t *testing.T) {
	e := newTestEnv(t, 2, false)
	a, _, _ := e.runAgent(fakeagent.Config{NoStream: true})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	e.addViewer("dev1", "fakecam")
	st := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
	a.SendStatus(context.Background(), protocol.StreamStatus{CameraID: st.CameraID, SessionID: st.SessionID, State: protocol.StateFailed, Error: "camera busy"})
	waitEvent(t, a, protocol.TypeStop, time.Second)
	waitFor(t, "retrying state", time.Second, func() bool {
		c, _ := e.camera("dev1", "fakecam")
		return c.Error == "agent reported failure: camera busy"
	})
	begin := time.Now()
	st2 := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
	if st2.SessionID == st.SessionID || time.Since(begin) < 50*time.Millisecond {
		t.Fatalf("retry should be a fresh session after backoff")
	}
	// Stale status for the old session is ignored.
	a.SendStatus(context.Background(), protocol.StreamStatus{CameraID: st.CameraID, SessionID: st.SessionID, State: protocol.StateFailed})
	noEvent(t, a, protocol.TypeStop, 300*time.Millisecond)
}

func TestNoFreePort(t *testing.T) {
	e := newTestEnv(t, 1, false)
	cams := []protocol.Camera{{ID: "cam1", Name: "One"}, {ID: "cam2", Name: "Two"}}
	a, _, _ := e.runAgent(fakeagent.Config{Cameras: cams, NoStream: true})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "cameras", 2*time.Second, func() bool { return len(e.s.Cameras()) == 2 })
	if _, err := e.addViewer("dev1", "cam1"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, a, protocol.TypeStart, 2*time.Second)
	if _, err := e.addViewer("dev1", "cam2"); !errors.Is(err, errNoPort) {
		t.Fatalf("want errNoPort, got %v", err)
	}
	// Via HTTP the viewer gets a 503 with a message.
	e2 := newTestEnv(t, 1, true)
	b, _, _ := e2.runAgent(fakeagent.Config{Cameras: cams, NoStream: true})
	waitEvent(t, b, protocol.TypeWelcome, 2*time.Second)
	e2.addViewer("dev1", "cam1")
	resp, err := http.Post(e2.hs.URL+"/api/cameras/dev1/cam2/whep", "application/sdp", strings.NewReader("v=0\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "ingest ports") {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestIngestRejectsBadToken(t *testing.T) {
	e := newTestEnv(t, 1, false)
	a, _, _ := e.runAgent(fakeagent.Config{NoStream: true})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	e.addViewer("dev1", "fakecam")
	st := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
	for _, pre := range []string{
		protocol.IngestPreamble(st.SessionID, st.Token+"x"),
		protocol.IngestPreamble("other", st.Token),
		"GET / HTTP/1.1\r\n",
	} {
		conn := e.dialIngest(st.Port)
		conn.Write([]byte(pre))
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("%q: expected close, got %v", pre, err)
		}
		conn.Close()
	}
	c, _ := e.camera("dev1", "fakecam")
	if c.Streaming {
		t.Fatal("must not be streaming")
	}
}

func TestCamerasUpdate(t *testing.T) {
	e := newTestEnv(t, 2, false)
	a, _, _ := e.runAgent(fakeagent.Config{})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	e.addViewer("dev1", "fakecam")
	waitEvent(t, a, protocol.TypeStart, 2*time.Second)
	// The camera is unplugged: session stops, camera shows offline.
	a.SendCameras(context.Background(), nil)
	waitEvent(t, a, protocol.TypeStop, 2*time.Second)
	waitFor(t, "camera offline", 2*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return !c.Online })
	// Plugged back in: the waiting viewer gets a new session.
	a.SendCameras(context.Background(), []protocol.Camera{{ID: "fakecam", Name: "Fake", AudioDevice: "hw:1"}})
	waitEvent(t, a, protocol.TypeStart, 2*time.Second)
}
