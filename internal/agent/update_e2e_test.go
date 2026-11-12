package agent

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"birdcam/internal/server"
	"birdcam/internal/server/config"
)

// A real agent connects to a real server that ships a different build: the
// agent downloads it over HTTP with its device key, verifies it, stages it and
// ends Run (systemd would start it again).
func TestAgentUpdatesFromServer(t *testing.T) {
	oldV, oldP := server.Version, agentPlatform
	server.Version, agentPlatform = "v2", "linux/arm64"
	t.Cleanup(func() { server.Version, agentPlatform = oldV, oldP })
	idlePollForTest(t, 10*time.Millisecond)

	// The shipped "build" is a script that reports v2.
	build := fakeBuild("v2")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "birdcam-agent-linux-arm64"), build, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Devices:      []config.Device{{ID: "pi1", Key: "secret"}},
		AgentUpdates: config.AgentUpdates{Dir: dir},
		StateDir:     t.TempDir(),
	}
	cfg.ApplyDefaults()
	cfg.Ingest.Bind = "127.0.0.1"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{Config: cfg, Logger: quiet, IngestPorts: make([]int, 1), DisableWebRTC: true})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { hs.CloseClientConnections(); srv.Close(); hs.Close() })

	acfg := &Config{
		ServerURL: "ws" + strings.TrimPrefix(hs.URL, "http") + "/agent",
		DeviceID:  "pi1", Key: "secret", StateDir: t.TempDir(), GstLaunch: "gst-launch-1.0",
	}
	a := New(acfg, "v1", slog.New(slog.DiscardHandler))
	done := make(chan struct{})
	go func() { a.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not restart into the update")
	}
	if got, err := binaryVersion(newUpdateStore(acfg.StateDir).bin()); err != nil || got != "v2" {
		t.Fatalf("staged build reports %q, %v", got, err)
	}

	// The restarted baseline would now exec the staged build.
	var execPath string
	oldExec := execSelf
	execSelf = func(p string, _ []string) error { execPath = p; return os.ErrPermission }
	t.Cleanup(func() { execSelf = oldExec })
	ExecOverride(acfg.StateDir, "v1", slog.New(slog.DiscardHandler))
	if execPath != newUpdateStore(acfg.StateDir).bin() {
		t.Fatalf("launcher did not pick the staged build (exec %q)", execPath)
	}

	// An agent that already runs v2 is left alone.
	a2 := New(&Config{ServerURL: acfg.ServerURL, DeviceID: "pi1", Key: "secret", StateDir: t.TempDir(), GstLaunch: "x"}, "v2", slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	finished := make(chan struct{})
	go func() { a2.Run(ctx); close(finished) }()
	<-finished
	if ctx.Err() == nil {
		t.Fatal("up-to-date agent restarted")
	}
}
