package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadResolvesRelativePathsAndKeyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dev.key"), []byte("s3cret-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `state_dir: state
ingest: {cert_file: tls/ingest.crt, key_file: /abs/ingest.key}
devices:
  - id: pi
    key_file: dev.key
`
	path := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != filepath.Join(dir, "state") {
		t.Errorf("state_dir = %q", c.StateDir)
	}
	if c.Ingest.CertFile != filepath.Join(dir, "tls/ingest.crt") || c.Ingest.KeyFile != "/abs/ingest.key" {
		t.Errorf("ingest files = %q, %q", c.Ingest.CertFile, c.Ingest.KeyFile)
	}
	if c.Devices[0].Key != "s3cret-key" {
		t.Errorf("key = %q", c.Devices[0].Key)
	}
}

func TestKeyAndKeyFileExclusive(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "k"), []byte("x"), 0o600)
	path := filepath.Join(dir, "s.yaml")
	os.WriteFile(path, []byte("devices:\n  - id: pi\n    key: a\n    key_file: k\n"), 0o600)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("err = %v", err)
	}
	os.WriteFile(path, []byte("devices:\n  - id: pi\n    key_file: missing\n"), 0o600)
	if _, err := Load(path); err == nil {
		t.Error("missing key file accepted")
	}
}
