package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"birdcam/internal/protocol"
	"birdcam/internal/server/config"
)

// agentFiles maps the platform an agent reports in Hello to the file name of
// the matching agent build in the agent_updates directory (see the Makefile
// and Dockerfile, which build them with the same version as the server).
var agentFiles = map[string]string{
	"linux/arm64": "birdcam-agent-linux-arm64",
	"linux/arm":   "birdcam-agent-linux-armv7",
}

const agentBinaryPrefix = "/agent/binary/"

// agentBinary is one agent build the server can hand out.
type agentBinary struct {
	path   string
	sha256 string
	size   int64
	url    string // path of the download endpoint
}

// loadAgentBinaries indexes the agent builds in cfg.Dir. The builds are
// assumed to have the server's own version (they come from the same build).
// It returns nil when updates are disabled or unavailable.
func loadAgentBinaries(cfg config.AgentUpdates, version string, log *slog.Logger) map[string]*agentBinary {
	if cfg.Disabled {
		return nil
	}
	if version == "" || version == "dev" {
		log.Info("agent updates disabled: development build of the server")
		return nil
	}
	out := map[string]*agentBinary{}
	for platform, name := range agentFiles {
		p := filepath.Join(cfg.Dir, name)
		sum, size, err := hashFile(p)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Warn("agent update binary unreadable", "path", p, "err", err)
			}
			continue
		}
		out[platform] = &agentBinary{
			path: p, sha256: sum, size: size,
			url: agentBinaryPrefix + strings.ReplaceAll(platform, "/", "-"),
		}
		log.Info("agent update available", "platform", platform, "version", version, "sha256", sum, "size", size)
	}
	if len(out) == 0 {
		log.Info("agent updates disabled: no agent binaries found", "dir", cfg.Dir)
		return nil
	}
	return out
}

func hashFile(p string) (sum string, size int64, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err = io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// updateFor returns the update to offer an agent that just said hello, or nil
// if it already runs the server's version, is a development build (never
// replaced), is too old to report its platform, or no matching build exists.
func (s *Server) updateFor(h protocol.Hello) *protocol.AgentUpdate {
	if h.Platform == "" || h.AgentVersion == "" || h.AgentVersion == "dev" || h.AgentVersion == Version {
		return nil
	}
	b := s.agentBins[h.Platform]
	if b == nil {
		return nil
	}
	return &protocol.AgentUpdate{Version: Version, SHA256: b.sha256, Size: b.size, Path: b.url}
}

// handleAgentBinary serves an agent build to an authenticated device:
// GET /agent/binary/{platform} with X-Birdcam-Device and
// "Authorization: Bearer <device key>".
func (s *Server) handleAgentBinary(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	dev := s.devices[r.Header.Get("X-Birdcam-Device")]
	s.mu.Unlock()
	want := "\x00no-such-device"
	if dev != nil {
		want = dev.cfg.Key
	}
	got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !keyEqual(got, want) || dev == nil {
		writeError(w, http.StatusUnauthorized, "authentication failed")
		return
	}
	var bin *agentBinary
	for _, b := range s.agentBins {
		if b.url == agentBinaryPrefix+r.PathValue("platform") {
			bin = b
		}
	}
	if bin == nil {
		writeError(w, http.StatusNotFound, "no agent binary for this platform")
		return
	}
	f, err := os.Open(bin.path)
	if err != nil {
		s.log.Error("agent binary", "path", bin.path, "err", err)
		writeError(w, http.StatusInternalServerError, "agent binary unavailable")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() != bin.size {
		// Replaced on disk after startup: the announced hash would not match.
		writeError(w, http.StatusInternalServerError, "agent binary changed on disk")
		return
	}
	s.log.Info("serving agent binary", "device", dev.cfg.ID, "platform", r.PathValue("platform"), "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filepath.Base(bin.path), st.ModTime(), f)
}
