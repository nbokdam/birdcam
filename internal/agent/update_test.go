package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"birdcam/internal/protocol"
)

var quiet = slog.New(slog.DiscardHandler)

// fakeBuild is a shell script that reports ver for -version.
func fakeBuild(ver string) []byte { return []byte("#!/bin/sh\necho " + ver + "\n") }

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type updateServer struct {
	*httptest.Server
	hits      atomic.Int32
	gotDevice atomic.Value // string
	gotAuth   atomic.Value // string
}

// newUpdateServer serves body at /agent/binary/linux-arm64 for the right credentials.
func newUpdateServer(t *testing.T, body []byte) *updateServer {
	t.Helper()
	us := &updateServer{}
	us.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		us.hits.Add(1)
		us.gotDevice.Store(r.Header.Get("X-Birdcam-Device"))
		us.gotAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/agent/binary/linux-arm64" || r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(us.Close)
	return us
}

func testUpdater(t *testing.T, srv *updateServer, version string) *updater {
	t.Helper()
	cfg := &Config{
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent",
		DeviceID:  "pi1", Key: "secret", StateDir: t.TempDir(),
	}
	return newUpdater(cfg, version, quiet)
}

func offer(ver string, body []byte) protocol.AgentUpdate {
	return protocol.AgentUpdate{Version: ver, SHA256: sum(body), Size: int64(len(body)), Path: "/agent/binary/linux-arm64"}
}

func TestStageDownloadsVerifiesAndInstalls(t *testing.T) {
	body := fakeBuild("v2")
	srv := newUpdateServer(t, body)
	u := testUpdater(t, srv, "v1")

	ready, err := u.stage(context.Background(), offer("v2", body))
	if err != nil || !ready {
		t.Fatalf("stage: ready=%v err=%v", ready, err)
	}
	if got := srv.gotDevice.Load(); got != "pi1" {
		t.Errorf("device header %v", got)
	}
	if v, err := binaryVersion(u.store.bin()); err != nil || v != "v2" {
		t.Fatalf("installed binary reports %q, %v", v, err)
	}
	if st, _ := os.Stat(u.store.bin()); st.Mode()&0o111 == 0 {
		t.Error("installed binary is not executable")
	}
	if n, ok := u.store.pending(); !ok || n != 0 {
		t.Errorf("pending = %d, %v; want 0, true", n, ok)
	}

	// Already staged: offered again after a reconnect, no second download.
	hits := srv.hits.Load()
	if ready, err := u.stage(context.Background(), offer("v2", body)); err != nil || !ready {
		t.Fatalf("restage: %v %v", ready, err)
	}
	if srv.hits.Load() != hits {
		t.Error("downloaded an update that was already staged")
	}
}

func TestStageRejectsBadOffers(t *testing.T) {
	body := fakeBuild("v2")
	srv := newUpdateServer(t, body)
	good := offer("v2", body)

	cases := map[string]func(*protocol.AgentUpdate){
		"wrong sha":        func(o *protocol.AgentUpdate) { o.SHA256 = sum([]byte("other")) },
		"short sha":        func(o *protocol.AgentUpdate) { o.SHA256 = "abc" },
		"size too small":   func(o *protocol.AgentUpdate) { o.Size-- },
		"size too large":   func(o *protocol.AgentUpdate) { o.Size++ },
		"huge size":        func(o *protocol.AgentUpdate) { o.Size = maxUpdateSize + 1 },
		"version mismatch": func(o *protocol.AgentUpdate) { o.Version = "v3" },
		"unknown path":     func(o *protocol.AgentUpdate) { o.Path = "/agent/binary/linux-riscv" },
		"relative path":    func(o *protocol.AgentUpdate) { o.Path = "agent/binary/linux-arm64" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			u := testUpdater(t, srv, "v1")
			o := good
			mod(&o)
			ready, err := u.stage(context.Background(), o)
			if err == nil || ready {
				t.Fatalf("accepted a bad offer (ready=%v err=%v)", ready, err)
			}
			if _, err := os.Stat(u.store.bin()); err == nil {
				t.Error("override was installed")
			}
			if _, ok := u.store.pending(); ok {
				t.Error("pending marker was left behind")
			}
			if left, _ := os.ReadDir(u.store.dir); len(left) != 0 {
				t.Errorf("leftover files: %v", left)
			}
		})
	}
}

func TestStageRejectsNonRunnableBuild(t *testing.T) {
	body := []byte("this is not an executable for any architecture")
	srv := newUpdateServer(t, body)
	u := testUpdater(t, srv, "v1")
	if ready, err := u.stage(context.Background(), offer("v2", body)); err == nil || ready {
		t.Fatalf("installed a build that cannot run: ready=%v err=%v", ready, err)
	}
	if _, err := os.Stat(u.store.bin()); err == nil {
		t.Error("override was installed")
	}
}

func TestStageWrongKeyAndSameVersion(t *testing.T) {
	body := fakeBuild("v2")
	srv := newUpdateServer(t, body)
	u := testUpdater(t, srv, "v1")
	u.key = "wrong"
	if _, err := u.stage(context.Background(), offer("v2", body)); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401 error, got %v", err)
	}
	u.key = "secret"
	srv.hits.Store(0)
	if ready, err := u.stage(context.Background(), offer("v1", body)); ready || err != nil || srv.hits.Load() != 0 {
		t.Fatalf("offer of the running version: ready=%v err=%v hits=%d", ready, err, srv.hits.Load())
	}
}

func TestStageDoesNotReinstallRolledBackVersion(t *testing.T) {
	body := fakeBuild("v2")
	srv := newUpdateServer(t, body)
	u := testUpdater(t, srv, "v1")
	_ = os.MkdirAll(u.store.dir, 0o755)
	u.store.writeBad("v2")
	if ready, err := u.stage(context.Background(), offer("v2", body)); ready || err == nil || srv.hits.Load() != 0 {
		t.Fatalf("ready=%v err=%v hits=%d", ready, err, srv.hits.Load())
	}
	// A different build is fine.
	body3 := fakeBuild("v3")
	srv3 := newUpdateServer(t, body3)
	u.serverURL = "ws" + strings.TrimPrefix(srv3.URL, "http") + "/agent"
	if ready, err := u.stage(context.Background(), offer("v3", body3)); !ready || err != nil {
		t.Fatalf("v3: ready=%v err=%v", ready, err)
	}
}

func TestDownloadURL(t *testing.T) {
	for in, want := range map[string]string{
		"wss://birdcam.example.com/agent":        "https://birdcam.example.com/agent/binary/linux-arm64",
		"ws://10.0.0.5:8080/agent?x=1":           "http://10.0.0.5:8080/agent/binary/linux-arm64",
		"wss://birdcam.example.com:8443/some/ws": "https://birdcam.example.com:8443/agent/binary/linux-arm64",
	} {
		got, err := downloadURL(in, "/agent/binary/linux-arm64")
		if err != nil || got != want {
			t.Errorf("%s -> %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := downloadURL("http://x/agent", "/p"); err == nil {
		t.Error("accepted an http:// server_url")
	}
}

func TestRunRestartsOnlyWhenIdle(t *testing.T) {
	old := idlePollForTest(t, 10*time.Millisecond)
	_ = old
	body := fakeBuild("v2")
	srv := newUpdateServer(t, body)
	u := testUpdater(t, srv, "v1")

	var busy atomic.Bool
	busy.Store(true)
	restarted := make(chan struct{})
	go u.run(context.Background(), offer("v2", body), func() bool { return !busy.Load() }, func() { close(restarted) })

	select {
	case <-restarted:
		t.Fatal("restarted while a stream was running")
	case <-time.After(300 * time.Millisecond):
	}
	busy.Store(false)
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("did not restart once idle")
	}
}

func TestRunDoesNotRestartOnFailure(t *testing.T) {
	body := fakeBuild("v2")
	srv := newUpdateServer(t, body)
	u := testUpdater(t, srv, "v1")
	o := offer("v2", body)
	o.SHA256 = sum([]byte("tampered"))
	restarted := false
	u.run(context.Background(), o, func() bool { return true }, func() { restarted = true })
	if restarted {
		t.Fatal("restarted although the update failed verification")
	}
}

// idlePollForTest shortens the idle poll interval for a test.
func idlePollForTest(t *testing.T, d time.Duration) time.Duration {
	t.Helper()
	prev := idlePollInterval
	idlePollInterval = d
	t.Cleanup(func() { idlePollInterval = prev })
	return prev
}

func TestDecideOverride(t *testing.T) {
	dir := t.TempDir()
	st := newUpdateStore(dir)
	_ = os.MkdirAll(st.dir, 0o755)

	if got := decideOverride(st, "v1", "v2"); got != actNone {
		t.Errorf("no override file: %v", got)
	}
	_ = os.WriteFile(st.bin(), fakeBuild("v2"), 0o755)
	if got := decideOverride(st, "v1", "v2"); got != actExec {
		t.Errorf("different version, confirmed: %v", got)
	}
	if got := decideOverride(st, "v2", "v2"); got != actNone {
		t.Errorf("same version: %v", got)
	}
	if got := decideOverride(st, "v1", ""); got != actNone {
		t.Errorf("override cannot run: %v", got)
	}
	for n := 0; n < MaxStartAttempts; n++ {
		_ = st.setPending(n)
		if got := decideOverride(st, "v1", "v2"); got != actExec {
			t.Errorf("pending attempts %d: %v", n, got)
		}
	}
	_ = st.setPending(MaxStartAttempts)
	if got := decideOverride(st, "v1", "v2"); got != actRollback {
		t.Errorf("pending attempts %d: %v", MaxStartAttempts, got)
	}
	st.confirm() // a confirmed build is never rolled back, however often it restarts
	if got := decideOverride(st, "v1", "v2"); got != actExec {
		t.Errorf("confirmed: %v", got)
	}
}

func TestExecOverride(t *testing.T) {
	var execPath string
	var execEnv []string
	old := execSelf
	execSelf = func(p string, env []string) error { execPath, execEnv = p, env; return os.ErrPermission }
	t.Cleanup(func() { execSelf = old })

	t.Run("execs and counts attempts", func(t *testing.T) {
		execPath = ""
		dir := t.TempDir()
		st := newUpdateStore(dir)
		_ = os.MkdirAll(st.dir, 0o755)
		_ = os.WriteFile(st.bin(), fakeBuild("v2"), 0o755)
		_ = st.setPending(1)
		ExecOverride(dir, "v1", quiet)
		if execPath != st.bin() {
			t.Fatalf("exec path %q", execPath)
		}
		if !contains(execEnv, overrideEnv+"=1") {
			t.Error("override env var not set")
		}
		if n, _ := st.pending(); n != 2 {
			t.Errorf("attempts = %d, want 2", n)
		}
	})
	t.Run("rolls back after repeated unconfirmed starts", func(t *testing.T) {
		execPath = ""
		dir := t.TempDir()
		st := newUpdateStore(dir)
		_ = os.MkdirAll(st.dir, 0o755)
		_ = os.WriteFile(st.bin(), fakeBuild("v2"), 0o755)
		_ = st.setPending(MaxStartAttempts)
		ExecOverride(dir, "v1", quiet)
		if execPath != "" {
			t.Fatal("exec'ed a build that failed to start repeatedly")
		}
		if _, err := os.Stat(st.bin()); err == nil {
			t.Error("override not removed")
		}
		if st.readBad() != "v2" {
			t.Errorf("bad version = %q", st.readBad())
		}
	})
	t.Run("same version removes the redundant override", func(t *testing.T) {
		execPath = ""
		dir := t.TempDir()
		st := newUpdateStore(dir)
		_ = os.MkdirAll(st.dir, 0o755)
		_ = os.WriteFile(st.bin(), fakeBuild("v1"), 0o755)
		ExecOverride(dir, "v1", quiet)
		if execPath != "" {
			t.Fatal("exec'ed the same version")
		}
		if _, err := os.Stat(st.bin()); err == nil {
			t.Error("override not removed")
		}
	})
	t.Run("override process never execs again", func(t *testing.T) {
		execPath = ""
		dir := t.TempDir()
		st := newUpdateStore(dir)
		_ = os.MkdirAll(st.dir, 0o755)
		_ = os.WriteFile(st.bin(), fakeBuild("v2"), 0o755)
		t.Setenv(overrideEnv, "1")
		ExecOverride(dir, "v1", quiet)
		if execPath != "" {
			t.Fatal("override exec'ed itself")
		}
	})
	t.Run("nothing downloaded", func(t *testing.T) {
		execPath = ""
		ExecOverride(t.TempDir(), "v1", quiet)
		if execPath != "" {
			t.Fatal("exec'ed without an override")
		}
	})
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestConfirmAfter(t *testing.T) {
	dir := t.TempDir()
	st := newUpdateStore(dir)
	_ = os.MkdirAll(st.dir, 0o755)
	_ = st.setPending(2)
	u := &updater{store: st}

	u.confirmAfter(context.Background(), time.Millisecond) // not the override: nothing happens
	time.Sleep(50 * time.Millisecond)
	if _, ok := st.pending(); !ok {
		t.Fatal("baseline confirmed an override it does not run")
	}
	t.Setenv(overrideEnv, "1")
	u.confirmAfter(context.Background(), 10*time.Millisecond)
	waitUntil(t, func() bool { _, ok := st.pending(); return !ok })
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached")
}

func TestOnWelcomeUpdateFlow(t *testing.T) {
	body := fakeBuild("v2")
	srv := newUpdateServer(t, body)
	idlePollForTest(t, 10*time.Millisecond)
	mk := func(version string, mod func(*Config)) (*Agent, *Config) {
		cfg := &Config{
			ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent",
			DeviceID:  "pi1", Key: "secret", StateDir: t.TempDir(), GstLaunch: "gst-launch-1.0",
		}
		if mod != nil {
			mod(cfg)
		}
		a := New(cfg, version, quiet)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		a.runCtx, a.restart = ctx, cancel
		return a, cfg
	}
	welcome := protocol.Welcome{AgentUpdate: ptr(offer("v2", body))}

	t.Run("applies and restarts", func(t *testing.T) {
		a, _ := mk("v1", nil)
		a.OnWelcome(welcome)
		select {
		case <-a.runCtx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("agent did not restart into the update")
		}
		if v, err := binaryVersion(a.updater.store.bin()); err != nil || v != "v2" {
			t.Fatalf("staged %q, %v", v, err)
		}
	})
	for name, a := range map[string]func() *Agent{
		"dev build": func() *Agent { a, _ := mk("dev", nil); return a },
		"auto_update off": func() *Agent {
			off := false
			a, _ := mk("v1", func(c *Config) { c.AutoUpdate = &off })
			return a
		},
	} {
		t.Run("ignored for "+name, func(t *testing.T) {
			ag := a()
			before := srv.hits.Load()
			ag.OnWelcome(welcome)
			time.Sleep(100 * time.Millisecond)
			if ag.runCtx.Err() != nil || srv.hits.Load() != before {
				t.Fatal("acted on the update offer")
			}
		})
	}
	t.Run("no offer", func(t *testing.T) {
		a, _ := mk("v1", nil)
		a.OnWelcome(protocol.Welcome{})
		time.Sleep(50 * time.Millisecond)
		if a.runCtx.Err() != nil {
			t.Fatal("restarted without an offer")
		}
	})
}

func ptr[T any](v T) *T { return &v }

func TestHelloReportsPlatform(t *testing.T) {
	a := New(&Config{ServerURL: "wss://x/agent", DeviceID: "d", Key: "k"}, "v1", quiet)
	h := a.hello()
	if h.Platform != runtime.GOOS+"/"+runtime.GOARCH || h.AgentVersion != "v1" {
		t.Fatalf("hello: %+v", h)
	}
}

func TestSupervisorActive(t *testing.T) {
	l, port := listen(t)
	rec := &statusRec{}
	sup := testSup(writeScript(t, mediaScript), rec)
	if sup.Active() != 0 {
		t.Fatal("active before start")
	}
	sup.Start(Plan{Start: protocol.Start{CameraID: "cam", SessionID: "s", Port: port, Token: "t"}, Mode: protocol.ModeH264Passthrough, Args: []string{"-q"}})
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if sup.Active() != 1 {
		t.Fatalf("active = %d", sup.Active())
	}
	sup.StopAll()
	if sup.Active() != 0 {
		t.Fatalf("active after StopAll = %d", sup.Active())
	}
}

func TestStreamUsesIngestHostFromStart(t *testing.T) {
	l, port := listen(t)
	rec := &statusRec{}
	sup := NewSupervisor(SupervisorConfig{
		GstPath: writeScript(t, mediaScript), Report: rec.add, Log: quiet,
		ServerHost: "does-not-resolve.invalid", // the control-channel host: must not be used
		RetryDelay: 20 * time.Millisecond, Watchdog: 400 * time.Millisecond, KillGrace: 500 * time.Millisecond,
	})
	defer sup.StopAll()
	plan := ingestPlan(port, "")
	plan.Start.IngestHost = "127.0.0.1"
	sup.Start(plan)
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	readPreambleAndMedia(t, conn)
	rec.waitFor(t, protocol.StateRunning)
}
