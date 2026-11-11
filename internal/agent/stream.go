package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"birdcam/internal/protocol"
)

// Plan is everything the supervisor needs to run one stream session.
type Plan struct {
	Start protocol.Start
	Mode  string
	Args  []string // gst-launch argv
	// FallbackArgs, if set, replace Args after a watchdog stall (e.g. switch
	// from HW to SW JPEG decoding).
	FallbackArgs []string
	// IngestCertSHA256, if set, makes the ingest connection TLS, pinned to
	// this lowercase hex SHA-256 of the server's leaf certificate (DER).
	IngestCertSHA256 string
}

// SupervisorConfig configures a Supervisor. Zero values get defaults.
type SupervisorConfig struct {
	GstPath       string
	ServerHost    string
	Report        func(protocol.StreamStatus)
	Log           *slog.Logger
	MaxAttempts   int           // consecutive failed attempts before "failed" (3)
	RetryDelay    time.Duration // between attempts (1s)
	Watchdog      time.Duration // no stdout bytes for this long after the first bytes -> restart (3s)
	StartWatchdog time.Duration // no stdout bytes for this long after launch -> restart (5s, or Watchdog if that is set)
	DialTimeout   time.Duration // (5s)
	KillGrace     time.Duration // SIGINT -> SIGKILL (2s)
	StableAfter   time.Duration // a run this long resets the attempt counter (30s)
}

// Supervisor runs one stream session per camera.
type Supervisor struct {
	cfg SupervisorConfig

	mu       sync.Mutex
	sessions map[string]*session
	wg       sync.WaitGroup
}

func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.GstPath == "" {
		cfg.GstPath = "gst-launch-1.0"
	}
	if cfg.Report == nil {
		cfg.Report = func(protocol.StreamStatus) {}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = time.Second
	}
	if cfg.StartWatchdog == 0 {
		if cfg.Watchdog == 0 {
			cfg.StartWatchdog = 5 * time.Second
		} else {
			cfg.StartWatchdog = cfg.Watchdog
		}
	}
	if cfg.Watchdog == 0 {
		cfg.Watchdog = 3 * time.Second
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.KillGrace == 0 {
		cfg.KillGrace = 2 * time.Second
	}
	if cfg.StableAfter == 0 {
		cfg.StableAfter = 30 * time.Second
	}
	return &Supervisor{cfg: cfg, sessions: map[string]*session{}}
}

type session struct {
	sup      *Supervisor
	plan     Plan
	log      *slog.Logger
	cancel   context.CancelFunc
	done     chan struct{}
	replaced atomic.Bool

	tailMu sync.Mutex
	tail   []string
}

const tailLines = 50

// Start begins a session, replacing any existing one for the same camera.
// It never blocks: the new session waits for the old one to die.
func (s *Supervisor) Start(plan Plan) {
	cam := plan.Start.CameraID
	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{
		sup: s, plan: plan, cancel: cancel, done: make(chan struct{}),
		log: s.cfg.Log.With("camera", cam, "session", plan.Start.SessionID),
	}
	s.mu.Lock()
	prev := s.sessions[cam]
	if prev != nil {
		prev.replaced.Store(true)
		prev.cancel()
	}
	s.sessions[cam] = sess
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		defer close(sess.done)
		if prev != nil {
			<-prev.done
		}
		sess.run(ctx)
		s.mu.Lock()
		if s.sessions[cam] == sess {
			delete(s.sessions, cam)
		}
		s.mu.Unlock()
	}()
}

// Stop stops the session if it is still the current one for the camera.
func (s *Supervisor) Stop(cameraID, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[cameraID]; sess != nil && sess.plan.Start.SessionID == sessionID {
		sess.cancel()
	}
}

// StopAll stops every session and waits until all processes are gone.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	for _, sess := range s.sessions {
		sess.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (se *session) report(state, errMsg string) {
	se.sup.cfg.Report(protocol.StreamStatus{
		CameraID: se.plan.Start.CameraID, SessionID: se.plan.Start.SessionID,
		State: state, Mode: se.plan.Mode, Error: errMsg,
	})
}

func (se *session) addTail(line string) {
	se.tailMu.Lock()
	defer se.tailMu.Unlock()
	se.tail = append(se.tail, line)
	if len(se.tail) > tailLines {
		se.tail = se.tail[len(se.tail)-tailLines:]
	}
}

func (se *session) lastLines(n int) string {
	se.tailMu.Lock()
	defer se.tailMu.Unlock()
	t := se.tail
	if len(t) > n {
		t = t[len(t)-n:]
	}
	return strings.Join(t, "\n")
}

func (se *session) run(ctx context.Context) {
	cfg := se.sup.cfg
	se.report(protocol.StateStarting, "")
	attempts := 0
	for {
		ran, err := se.runOnce(ctx)
		if errors.Is(err, errWatchdog) && se.plan.FallbackArgs != nil && ctx.Err() == nil {
			se.log.Warn("pipeline stalled; switching to fallback pipeline")
			se.plan.Args, se.plan.FallbackArgs = se.plan.FallbackArgs, nil
		}
		if ctx.Err() != nil {
			if !se.replaced.Load() {
				se.report(protocol.StateStopped, "")
			}
			return
		}
		if ran >= cfg.StableAfter {
			attempts = 0
		}
		attempts++
		se.log.Warn("stream attempt ended", "attempt", attempts, "err", err)
		if attempts >= cfg.MaxAttempts {
			msg := err.Error()
			if tail := se.lastLines(10); tail != "" {
				msg += "\n" + tail
			}
			if len(msg) > 2000 {
				msg = msg[len(msg)-2000:]
			}
			se.report(protocol.StateFailed, msg)
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(cfg.RetryDelay):
		}
	}
}

// dialIngest connects to the server's ingest port. With a fingerprint the
// connection is TLS 1.3 and only a leaf certificate whose SHA-256 matches is
// accepted (the chain is not validated: the server cert is self-signed and
// trust is anchored in the control channel). The handshake completes before
// this returns. Go's TLS client prefers ChaCha20-Poly1305 over AES-GCM
// automatically when the CPU lacks AES instructions (Pi 3), so no cipher
// configuration is needed.
func dialIngest(ctx context.Context, host string, port int, timeout time.Duration, fingerprint string) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := net.Dialer{Timeout: timeout}
	if fingerprint == "" {
		return d.DialContext(ctx, "tcp", addr)
	}
	want := strings.ToLower(strings.TrimSpace(fingerprint))
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         host,
		InsecureSkipVerify: true, // replaced by the pin check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server presented no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			got := hex.EncodeToString(sum[:])
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return fmt.Errorf("ingest certificate fingerprint mismatch (server presented sha256 %s, control channel announced %s)", got, want)
			}
			return nil
		},
	}
	td := &tls.Dialer{NetDialer: &d, Config: tc}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return td.DialContext(hctx, "tcp", addr)
}

var errWatchdog = errors.New("no media data from gst-launch (watchdog)")

// runOnce runs one TCP connection + gst-launch process until either ends.
func (se *session) runOnce(ctx context.Context) (time.Duration, error) {
	cfg := se.sup.cfg
	t0 := time.Now()
	st := se.plan.Start

	conn, err := dialIngest(ctx, cfg.ServerHost, st.Port, cfg.DialTimeout, se.plan.IngestCertSHA256)
	if err != nil {
		return time.Since(t0), fmt.Errorf("connect ingest: %w", err)
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, protocol.IngestPreamble(st.SessionID, st.Token)); err != nil {
		return time.Since(t0), fmt.Errorf("write preamble: %w", err)
	}

	cmd := exec.Command(cfg.GstPath, se.plan.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return time.Since(t0), err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return time.Since(t0), err
	}
	if err := cmd.Start(); err != nil {
		return time.Since(t0), fmt.Errorf("start %s: %w", cfg.GstPath, err)
	}
	pgid := cmd.Process.Pid
	killGroup := func(sig syscall.Signal) { _ = syscall.Kill(-pgid, sig) }
	se.log.Info("gst-launch started", "pid", pgid, "args", strings.Join(se.plan.Args, " "))

	var errWG sync.WaitGroup
	errWG.Add(1)
	go func() {
		defer errWG.Done()
		se.scanStderr(stderr)
	}()

	var lastBytes, total atomic.Int64
	var gotBytes atomic.Bool
	lastBytes.Store(time.Now().UnixNano())
	copyDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 32*1024)
		first := true
		for {
			n, rerr := stdout.Read(buf)
			if n > 0 {
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if _, werr := conn.Write(buf[:n]); werr != nil {
					copyDone <- fmt.Errorf("ingest connection: %w", werr)
					return
				}
				lastBytes.Store(time.Now().UnixNano())
				gotBytes.Store(true)
				total.Add(int64(n))
				if first {
					first = false
					se.log.Info("first media bytes")
					se.report(protocol.StateRunning, "")
				}
			}
			if rerr != nil {
				if rerr == io.EOF {
					rerr = nil
				}
				copyDone <- rerr
				return
			}
		}
	}()

	tick := time.NewTicker(max(min(cfg.Watchdog, cfg.StartWatchdog)/10, 10*time.Millisecond))
	defer tick.Stop()
	ctxDone := ctx.Done()
	var killC <-chan time.Time
	var reason error
	var copyErr error
loop:
	for {
		select {
		case copyErr = <-copyDone:
			break loop
		case <-ctxDone:
			ctxDone = nil
			killGroup(syscall.SIGINT) // exits in ~0.1s (no -e, so no EOS drain)
			killC = time.After(cfg.KillGrace)
		case <-killC:
			killC = nil
			killGroup(syscall.SIGKILL)
			conn.Close()
		case <-tick.C:
			limit := cfg.StartWatchdog
			if gotBytes.Load() {
				limit = cfg.Watchdog
			}
			if reason == nil && ctxDone != nil && time.Since(time.Unix(0, lastBytes.Load())) > limit {
				reason = errWatchdog
				killGroup(syscall.SIGKILL)
				conn.Close()
			}
		}
	}
	killGroup(syscall.SIGKILL) // no leftovers
	errWG.Wait()
	waitErr := cmd.Wait()
	ran := time.Since(t0)
	se.log.Info("gst-launch ended", "bytes", total.Load(), "ran", ran.Round(time.Millisecond))

	switch {
	case reason != nil:
		return ran, reason
	case copyErr != nil:
		return ran, copyErr
	case waitErr != nil:
		return ran, fmt.Errorf("gst-launch exited: %w", waitErr)
	}
	return ran, errors.New("gst-launch exited")
}

func (se *session) scanStderr(r io.Reader) {
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	flush := func(line string) {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			return
		}
		se.addTail(line)
		se.log.Info("gst", "line", line)
	}
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			i := strings.IndexByte(string(buf), '\n')
			if i < 0 {
				break
			}
			flush(string(buf[:i]))
			buf = buf[i+1:]
		}
		if len(buf) > 64*1024 {
			flush(string(buf))
			buf = buf[:0]
		}
		if err != nil {
			flush(string(buf))
			return
		}
	}
}
