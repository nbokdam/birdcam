package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"birdcam/internal/fakeagent"
	"birdcam/internal/protocol"
	"birdcam/internal/server/config"
)

func TestStartCarriesIngestHost(t *testing.T) {
	for name, host := range map[string]string{"set": "127.0.0.1", "unset": ""} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnvWith(t, 1, false, func(c *config.Config) { c.Ingest.PublicHost = host })
			a, _, _ := e.runAgent(fakeagent.Config{})
			waitFor(t, "camera", 2*time.Second, func() bool { _, ok := e.camera("dev1", "fakecam"); return ok })
			if _, err := e.addViewer("dev1", "fakecam"); err != nil {
				t.Fatal(err)
			}
			st := waitEvent(t, a, protocol.TypeStart, 2*time.Second).Start
			if st.IngestHost != host {
				t.Fatalf("start.ingest_host = %q, want %q", st.IngestHost, host)
			}
			// The fake agent follows start.ingest_host; with no override it
			// must still reach the server via the host of its server_url.
			waitFor(t, "streaming", 3*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return c.Streaming })
		})
	}
}

// agentDir writes fake agent builds and returns the directory.
func agentDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := Version
	Version = v
	t.Cleanup(func() { Version = old })
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func welcomeFor(t *testing.T, e *testEnv, cfg fakeagent.Config) protocol.Welcome {
	t.Helper()
	a, cancel, _ := e.runAgent(cfg)
	defer cancel()
	return waitEvent(t, a, protocol.TypeWelcome, 2*time.Second).Welcome
}

func TestAgentUpdateOffer(t *testing.T) {
	withVersion(t, "v2")
	dir := agentDir(t, map[string]string{
		"birdcam-agent-linux-arm64": "ARM64 BUILD",
		"birdcam-agent-linux-armv7": "ARMV7 BUILD",
	})
	e := newTestEnvWith(t, 1, false, func(c *config.Config) { c.AgentUpdates.Dir = dir })

	w := welcomeFor(t, e, fakeagent.Config{AgentVersion: "v1", Platform: "linux/arm64"})
	u := w.AgentUpdate
	if u == nil || u.Version != "v2" || u.SHA256 != sha("ARM64 BUILD") || u.Size != int64(len("ARM64 BUILD")) || u.Path != "/agent/binary/linux-arm64" {
		t.Fatalf("arm64 offer: %+v", u)
	}
	w = welcomeFor(t, e, fakeagent.Config{AgentVersion: "v1", Platform: "linux/arm"})
	if u := w.AgentUpdate; u == nil || u.SHA256 != sha("ARMV7 BUILD") || u.Path != "/agent/binary/linux-arm" {
		t.Fatalf("arm offer: %+v", u)
	}

	for name, cfg := range map[string]fakeagent.Config{
		"same version":           {AgentVersion: "v2", Platform: "linux/arm64"},
		"dev agent":              {AgentVersion: "dev", Platform: "linux/arm64"},
		"agent without platform": {AgentVersion: "v1"},
		"platform without build": {AgentVersion: "v1", Platform: "linux/riscv64"},
	} {
		if w := welcomeFor(t, e, cfg); w.AgentUpdate != nil {
			t.Errorf("%s: unexpected offer %+v", name, w.AgentUpdate)
		}
	}
}

func TestAgentUpdateDisabled(t *testing.T) {
	dir := agentDir(t, map[string]string{"birdcam-agent-linux-arm64": "BUILD"})
	for name, setup := range map[string]func(*testing.T) func(*config.Config){
		"dev server": func(t *testing.T) func(*config.Config) {
			withVersion(t, "dev")
			return func(c *config.Config) { c.AgentUpdates.Dir = dir }
		},
		"disabled in config": func(t *testing.T) func(*config.Config) {
			withVersion(t, "v2")
			return func(c *config.Config) { c.AgentUpdates.Dir = dir; c.AgentUpdates.Disabled = true }
		},
		"no binaries": func(t *testing.T) func(*config.Config) {
			withVersion(t, "v2")
			return func(c *config.Config) { c.AgentUpdates.Dir = t.TempDir() }
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnvWith(t, 1, false, setup(t))
			if w := welcomeFor(t, e, fakeagent.Config{AgentVersion: "v1", Platform: "linux/arm64"}); w.AgentUpdate != nil {
				t.Fatalf("offered %+v", w.AgentUpdate)
			}
			if code, _ := e.getBinary("linux-arm64", "dev1", "key1"); code != http.StatusNotFound {
				t.Fatalf("download answered %d, want 404", code)
			}
		})
	}
}

func (e *testEnv) getBinary(platform, device, key string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.hs.URL+"/agent/binary/"+platform, nil)
	if device != "" {
		req.Header.Set("X-Birdcam-Device", device)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAgentBinaryDownload(t *testing.T) {
	withVersion(t, "v2")
	dir := agentDir(t, map[string]string{"birdcam-agent-linux-arm64": "ARM64 BUILD"})
	e := newTestEnvWith(t, 1, false, func(c *config.Config) { c.AgentUpdates.Dir = dir })

	if code, body := e.getBinary("linux-arm64", "dev1", "key1"); code != 200 || body != "ARM64 BUILD" {
		t.Fatalf("download: %d %q", code, body)
	}
	if code, body := e.getBinary("linux-arm64", "dev2", "key2"); code != 200 || body != "ARM64 BUILD" {
		t.Fatalf("download as other device: %d %q", code, body)
	}
	for name, c := range map[string]struct{ device, key string }{
		"wrong key":      {"dev1", "key2"},
		"unknown device": {"nope", "key1"},
		"no credentials": {"", ""},
		"key as device":  {"key1", "key1"},
		"empty key":      {"dev1", ""},
	} {
		if code, body := e.getBinary("linux-arm64", c.device, c.key); code != http.StatusUnauthorized || body == "ARM64 BUILD" {
			t.Errorf("%s: %d %q", name, code, body)
		}
	}
	if code, _ := e.getBinary("linux-riscv64", "dev1", "key1"); code != http.StatusNotFound {
		t.Errorf("unknown platform: %d", code)
	}
	if code, _ := e.getBinary("linux-arm", "dev1", "key1"); code != http.StatusNotFound {
		t.Errorf("platform without build: %d", code)
	}

	// A build replaced on disk after startup would not match the announced hash.
	if err := os.WriteFile(filepath.Join(dir, "birdcam-agent-linux-arm64"), []byte("TAMPERED BUILD!"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, body := e.getBinary("linux-arm64", "dev1", "key1"); code != http.StatusInternalServerError || body == "TAMPERED BUILD!" {
		t.Errorf("changed on disk: %d %q", code, body)
	}
}
