package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"birdcam/internal/protocol"
)

// Self-update.
//
// The server announces the build it wants in Welcome.AgentUpdate. The agent
// downloads it into <state_dir>/update/birdcam-agent (the "override"), checks
// size, SHA-256 (announced over the control channel) and that the binary
// reports the announced version, and then exits once no stream is running.
// systemd restarts /usr/local/bin/birdcam-agent (the installed "baseline",
// which sits on a read-only path and is never modified); at startup that
// binary execs the override when it is a different version (ExecOverride).
//
// Safety net: a freshly installed override is "pending" until it has run for
// StableAfter. If it was started MaxStartAttempts times without ever getting
// there, it is deleted and its version is remembered as bad, so the baseline
// runs again and the same build is not downloaded in a loop.
// install-agent.sh removes the override, so a manual install always wins.

const (
	updateDirName = "update"
	overrideBin   = "birdcam-agent"
	pendingFile   = "pending"
	badFile       = "bad"
	// overrideEnv is set when the override was exec'ed by the baseline.
	overrideEnv = "BIRDCAM_AGENT_OVERRIDE"

	// MaxStartAttempts is how often an unconfirmed override is started.
	MaxStartAttempts = 3
	// StableAfter is how long a new build must run to be confirmed.
	StableAfter = 60 * time.Second

	maxUpdateSize = 64 << 20
)

// idlePollInterval is how often a staged update checks for an idle moment.
var idlePollInterval = 5 * time.Second

// updateStore is the on-disk state of the override.
type updateStore struct{ dir string }

func newUpdateStore(stateDir string) *updateStore {
	return &updateStore{dir: filepath.Join(stateDir, updateDirName)}
}

func (u *updateStore) bin() string          { return filepath.Join(u.dir, overrideBin) }
func (u *updateStore) path(n string) string { return filepath.Join(u.dir, n) }

func (u *updateStore) readBad() string {
	b, _ := os.ReadFile(u.path(badFile))
	return strings.TrimSpace(string(b))
}

func (u *updateStore) writeBad(v string) { _ = os.WriteFile(u.path(badFile), []byte(v+"\n"), 0o644) }

// pending returns the start attempts of an unconfirmed override.
func (u *updateStore) pending() (attempts int, ok bool) {
	b, err := os.ReadFile(u.path(pendingFile))
	if err != nil {
		return 0, false
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n, true
}

func (u *updateStore) setPending(n int) error {
	return os.WriteFile(u.path(pendingFile), []byte(strconv.Itoa(n)+"\n"), 0o644)
}

func (u *updateStore) confirm() { _ = os.Remove(u.path(pendingFile)) }

// removeOverride deletes the override and its pending marker.
func (u *updateStore) removeOverride() {
	_ = os.Remove(u.bin())
	u.confirm()
}

// execSelf replaces the process image; a variable so tests never exec.
var execSelf = func(path string, env []string) error { return syscall.Exec(path, os.Args, env) }

// binaryVersion runs "<path> -version".
func binaryVersion(path string) (string, error) {
	var out []byte
	var err error
	for try := 0; try < 5; try++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err = exec.CommandContext(ctx, path, "-version").Output()
		cancel()
		// A just-written file can briefly be "busy" if another thread forked
		// while it was open for writing.
		if err == nil || !errors.Is(err, syscall.ETXTBSY) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// overrideAction is what the starting agent should do about the override.
type overrideAction int

const (
	actNone     overrideAction = iota // run this binary
	actExec                           // exec the override
	actRollback                       // override failed repeatedly: delete it, run this binary
)

// decideOverride inspects the override. own is this binary's version and
// overrideVersion the output of the override's -version ("" if it cannot run).
func decideOverride(st *updateStore, own, overrideVersion string) overrideAction {
	if _, err := os.Stat(st.bin()); err != nil {
		return actNone
	}
	if overrideVersion == "" || overrideVersion == own {
		return actNone // broken or redundant; caller cleans up
	}
	if n, ok := st.pending(); ok && n >= MaxStartAttempts {
		return actRollback
	}
	return actExec
}

// ExecOverride is called by main early in a normal start. If a downloaded
// update is present it replaces this process with it; it only returns when
// this binary should keep running.
func ExecOverride(stateDir, own string, log *slog.Logger) {
	if os.Getenv(overrideEnv) != "" {
		return // we are the override
	}
	st := newUpdateStore(stateDir)
	if _, err := os.Stat(st.bin()); err != nil {
		return
	}
	v, err := binaryVersion(st.bin())
	switch act := decideOverride(st, own, v); act {
	case actNone:
		if err != nil {
			log.Warn("ignoring unusable downloaded agent", "err", err)
		}
		st.removeOverride() // broken, or the installed binary is the same version
	case actRollback:
		log.Error("downloaded agent never started stably, rolling back to the installed version",
			"bad_version", v, "attempts", MaxStartAttempts)
		st.writeBad(v)
		st.removeOverride()
	case actExec:
		if n, ok := st.pending(); ok {
			if err := st.setPending(n + 1); err != nil {
				log.Warn("cannot record start attempt", "err", err)
			}
		}
		log.Info("starting downloaded agent", "version", v, "installed_version", own)
		err := execSelf(st.bin(), append(os.Environ(), overrideEnv+"=1"))
		log.Error("cannot exec downloaded agent; running the installed version", "err", err)
	}
}

// updater downloads and stages updates for an Agent.
type updater struct {
	serverURL string
	deviceID  string
	key       string
	store     *updateStore
	version   string
	log       *slog.Logger
	http      *http.Client

	mu       sync.Mutex
	inflight bool
}

func newUpdater(cfg *Config, version string, log *slog.Logger) *updater {
	return &updater{
		serverURL: cfg.ServerURL, deviceID: cfg.DeviceID, key: cfg.Key,
		store: newUpdateStore(cfg.StateDir), version: version, log: log,
		http: &http.Client{
			Timeout:       5 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// downloadURL resolves the server's relative path against server_url
// (ws→http, wss→https).
func downloadURL(serverURL, path string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	default:
		return "", fmt.Errorf("unsupported server_url scheme %q", u.Scheme)
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("update path %q is not absolute", path)
	}
	u.Path, u.RawQuery, u.Fragment = path, "", ""
	return u.String(), nil
}

// stage makes the override equal to the offered build. It returns true if the
// override is ready to be run (newly downloaded, or already there).
func (u *updater) stage(ctx context.Context, upd protocol.AgentUpdate) (bool, error) {
	if upd.Version == "" || upd.Version == u.version {
		return false, nil
	}
	if upd.Size <= 0 || upd.Size > maxUpdateSize {
		return false, fmt.Errorf("offered update has implausible size %d", upd.Size)
	}
	want := strings.ToLower(upd.SHA256)
	if len(want) != 64 {
		return false, errors.New("offered update has no valid sha256")
	}
	if bad := u.store.readBad(); bad == upd.Version {
		return false, fmt.Errorf("version %s was rolled back before; not installing it again", bad)
	}
	if err := os.MkdirAll(u.store.dir, 0o755); err != nil {
		return false, fmt.Errorf("state dir not writable (set state_dir or auto_update: false): %w", err)
	}
	if old, _ := filepath.Glob(u.store.path("download-*")); len(old) > 0 {
		for _, f := range old { // leftovers of an interrupted download
			_ = os.Remove(f)
		}
	}
	if got, err := fileSHA256(u.store.bin()); err == nil && got == want {
		return true, nil // staged earlier, waiting for an idle moment
	}

	link, err := downloadURL(u.serverURL, upd.Path)
	if err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(u.store.dir, "download-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	defer tmp.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Birdcam-Device", u.deviceID)
	req.Header.Set("Authorization", "Bearer "+u.key)
	resp, err := u.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("download: server answered %s", resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, upd.Size+1))
	if err != nil {
		return false, fmt.Errorf("download: %w", err)
	}
	if n != upd.Size {
		return false, fmt.Errorf("download: got %d bytes, announced %d", n, upd.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return false, fmt.Errorf("download: sha256 %s does not match announced %s", got, want)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	// The build must run on this machine and be the version that was announced
	// (catches a wrong CPU architecture or a noexec state dir before we commit).
	got, err := binaryVersion(tmp.Name())
	if err != nil {
		return false, fmt.Errorf("downloaded agent does not run here: %w", err)
	}
	if got != upd.Version {
		return false, fmt.Errorf("downloaded agent reports version %q, announced %q", got, upd.Version)
	}
	// Order matters: with the marker in place first, a crash between the two
	// steps can only leave a marker without an override, which is harmless.
	if err := u.store.setPending(0); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), u.store.bin()); err != nil {
		u.store.confirm()
		return false, err
	}
	u.log.Info("agent update downloaded", "version", upd.Version, "size", n)
	return true, nil
}

// run stages the update and then waits for idle() before calling restart.
// Only one run is active at a time.
func (u *updater) run(ctx context.Context, upd protocol.AgentUpdate, idle func() bool, restart func()) {
	u.mu.Lock()
	if u.inflight {
		u.mu.Unlock()
		return
	}
	u.inflight = true
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.inflight = false; u.mu.Unlock() }()

	ready, err := u.stage(ctx, upd)
	if err != nil {
		u.log.Error("agent update failed", "version", upd.Version, "err", err)
		return
	}
	if !ready {
		return
	}
	t := time.NewTicker(idlePollInterval)
	defer t.Stop()
	for !idle() {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
	u.log.Info("restarting to run the updated agent", "from", u.version, "to", upd.Version)
	restart()
}

// confirmAfter marks a freshly started override as good once it has been
// running for d.
func (u *updater) confirmAfter(ctx context.Context, d time.Duration) {
	if os.Getenv(overrideEnv) == "" {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(d):
			u.store.confirm()
		}
	}()
}
