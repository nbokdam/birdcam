package agent

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"birdcam/internal/protocol"
)

type statusRec struct {
	mu sync.Mutex
	s  []protocol.StreamStatus
}

func (r *statusRec) add(s protocol.StreamStatus) { r.mu.Lock(); r.s = append(r.s, s); r.mu.Unlock() }
func (r *statusRec) states() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.s {
		out = append(out, s.State)
	}
	return out
}
func (r *statusRec) waitFor(t *testing.T, state string) protocol.StreamStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, s := range r.s {
			if s.State == state {
				r.mu.Unlock()
				return s
			}
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state %q not reached; got %v", state, r.states())
	return protocol.StreamStatus{}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-gst.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func listen(t *testing.T) (net.Listener, int) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, l.Addr().(*net.TCPAddr).Port
}

func testSup(gst string, rec *statusRec) *Supervisor {
	return NewSupervisor(SupervisorConfig{
		GstPath: gst, ServerHost: "127.0.0.1", Report: rec.add, Log: slog.New(slog.DiscardHandler),
		RetryDelay: 20 * time.Millisecond, Watchdog: 400 * time.Millisecond, KillGrace: 500 * time.Millisecond,
	})
}

func pidAlive(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

func TestStreamRunsAndStops(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	gst := writeScript(t, `echo $$ > `+pidFile+`
echo "hello from gst" >&2
while true; do printf 'MEDIA'; sleep 0.05; done
`)
	l, port := listen(t)
	rec := &statusRec{}
	sup := testSup(gst, rec)
	sup.Start(Plan{Start: protocol.Start{CameraID: "cam", SessionID: "sess", Port: port, Token: "tok"}, Mode: protocol.ModeH264Passthrough, Args: []string{"-q"}})

	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || line != protocol.IngestPreamble("sess", "tok") {
		t.Fatalf("preamble %q err %v", line, err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "MEDIA" {
		t.Fatalf("payload %q err %v", buf, err)
	}
	rec.waitFor(t, protocol.StateRunning)

	pidB, _ := os.ReadFile(pidFile)
	pid := 0
	for _, c := range strings.TrimSpace(string(pidB)) {
		pid = pid*10 + int(c-'0')
	}

	sup.Stop("cam", "other-session") // stale stop is ignored
	time.Sleep(100 * time.Millisecond)
	if got := rec.states(); got[len(got)-1] == protocol.StateStopped {
		t.Fatal("stale stop stopped the session")
	}
	sup.Stop("cam", "sess")
	rec.waitFor(t, protocol.StateStopped)
	sup.StopAll()
	if pidAlive(pid) {
		// zombie reaped by cmd.Wait; give the kernel a moment
		time.Sleep(200 * time.Millisecond)
		if pidAlive(pid) {
			t.Errorf("gst process %d still alive", pid)
		}
	}
	got := rec.states()
	if got[0] != protocol.StateStarting {
		t.Errorf("states %v", got)
	}
}

func TestStreamReplace(t *testing.T) {
	gst := writeScript(t, `while true; do printf 'X'; sleep 0.05; done`)
	l, port := listen(t)
	rec := &statusRec{}
	sup := testSup(gst, rec)
	defer sup.StopAll()
	sup.Start(Plan{Start: protocol.Start{CameraID: "cam", SessionID: "one", Port: port, Token: "a"}, Args: nil})
	c1, _ := l.Accept()
	defer c1.Close()
	sup.Start(Plan{Start: protocol.Start{CameraID: "cam", SessionID: "two", Port: port, Token: "b"}, Args: nil})
	c2, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := bufio.NewReader(c2).ReadString('\n')
	if line != protocol.IngestPreamble("two", "b") {
		t.Errorf("second preamble %q", line)
	}
	// old connection must have been closed
	c1.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.Copy(io.Discard, c1) // returns at EOF/reset
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, s := range rec.s {
		if s.SessionID == "one" && s.State == protocol.StateStopped {
			t.Error("replaced session reported stopped")
		}
	}
}

func TestStreamFailsAfterRetries(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	gst := writeScript(t, `echo run >> `+counter+`
echo "no such element v4l2src" >&2
exit 1
`)
	l, port := listen(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	rec := &statusRec{}
	sup := testSup(gst, rec)
	defer sup.StopAll()
	sup.Start(Plan{Start: protocol.Start{CameraID: "cam", SessionID: "s", Port: port, Token: "t"}, Mode: protocol.ModeMJPEGHWEnc})
	st := rec.waitFor(t, protocol.StateFailed)
	if !strings.Contains(st.Error, "no such element v4l2src") || st.Mode != protocol.ModeMJPEGHWEnc {
		t.Errorf("status %+v", st)
	}
	b, _ := os.ReadFile(counter)
	if n := strings.Count(string(b), "run"); n != 3 {
		t.Errorf("gst ran %d times, want 3", n)
	}
}

func TestStreamWatchdogRestarts(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	gst := writeScript(t, `echo run >> `+counter+`
printf 'A'
exec sleep 30
`)
	l, port := listen(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	rec := &statusRec{}
	sup := testSup(gst, rec)
	sup.Start(Plan{Start: protocol.Start{CameraID: "cam", SessionID: "s", Port: port, Token: "t"}})
	rec.waitFor(t, protocol.StateRunning)
	st := rec.waitFor(t, protocol.StateFailed)
	if !strings.Contains(st.Error, "watchdog") {
		t.Errorf("error %q", st.Error)
	}
	sup.StopAll()
	b, _ := os.ReadFile(counter)
	if n := strings.Count(string(b), "run"); n != 3 {
		t.Errorf("ran %d times", n)
	}
}

func TestStreamIngestUnreachable(t *testing.T) {
	l, port := listen(t)
	l.Close() // nothing listening
	rec := &statusRec{}
	sup := testSup("/bin/true", rec)
	sup.Start(Plan{Start: protocol.Start{CameraID: "cam", SessionID: "s", Port: port}})
	st := rec.waitFor(t, protocol.StateFailed)
	if !strings.Contains(st.Error, "connect ingest") {
		t.Errorf("error %q", st.Error)
	}
}

func TestStreamFallbackAfterStall(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	// "stall" mode: one byte then silence; "ok" mode: continuous data.
	gst := writeScript(t, `echo "$1" >> `+counter+`
if [ "$1" = stall ]; then printf 'A'; exec sleep 30; fi
while true; do printf 'MEDIA'; sleep 0.05; done
`)
	l, port := listen(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	rec := &statusRec{}
	sup := testSup(gst, rec)
	sup.Start(Plan{Start: protocol.Start{CameraID: "cam", SessionID: "s", Port: port, Token: "t"},
		Args: []string{"stall"}, FallbackArgs: []string{"ok"}})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(counter)
		if strings.Contains(string(b), "ok") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(600 * time.Millisecond) // longer than the watchdog: must keep running
	sup.StopAll()
	b, _ := os.ReadFile(counter)
	if got := strings.Fields(string(b)); !reflect.DeepEqual(got, []string{"stall", "ok"}) {
		t.Errorf("runs %v", got)
	}
	for _, s := range rec.states() {
		if s == protocol.StateFailed {
			t.Error("unexpected failed state")
		}
	}
}

func selfSigned(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "birdcam-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:])
}

func tlsListen(t *testing.T, cert tls.Certificate) (net.Listener, int) {
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, l.Addr().(*net.TCPAddr).Port
}

func readPreambleAndMedia(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || line != protocol.IngestPreamble("sess", "tok") {
		t.Fatalf("preamble %q err %v", line, err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "MEDIA" {
		t.Fatalf("payload %q err %v", buf, err)
	}
}

func ingestPlan(port int, fp string) Plan {
	return Plan{Start: protocol.Start{CameraID: "cam", SessionID: "sess", Port: port, Token: "tok"},
		Mode: protocol.ModeH264Passthrough, Args: []string{"-q"}, IngestCertSHA256: fp}
}

const mediaScript = `while true; do printf 'MEDIA'; sleep 0.05; done`

func TestStreamTLSCorrectFingerprint(t *testing.T) {
	cert, fp := selfSigned(t)
	l, port := tlsListen(t, cert)
	rec := &statusRec{}
	sup := testSup(writeScript(t, mediaScript), rec)
	defer sup.StopAll()
	sup.Start(ingestPlan(port, strings.ToUpper(fp))) // case-insensitive
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	readPreambleAndMedia(t, conn)
	rec.waitFor(t, protocol.StateRunning)
}

func TestStreamTLSWrongFingerprint(t *testing.T) {
	cert, _ := selfSigned(t)
	l, port := tlsListen(t, cert)
	go func() { // complete handshakes so the server side does not hang
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	started := filepath.Join(t.TempDir(), "started")
	gst := writeScript(t, "touch "+started+"\n"+mediaScript)
	rec := &statusRec{}
	sup := testSup(gst, rec)
	defer sup.StopAll()
	sup.Start(ingestPlan(port, strings.Repeat("0", 64)))
	st := rec.waitFor(t, protocol.StateFailed)
	if !strings.Contains(st.Error, "fingerprint mismatch") {
		t.Errorf("error not clear: %q", st.Error)
	}
	if _, err := os.Stat(started); err == nil {
		t.Error("gst was started despite failed pin")
	}
}

func TestStreamPlainWithoutFingerprint(t *testing.T) {
	l, port := listen(t)
	rec := &statusRec{}
	sup := testSup(writeScript(t, mediaScript), rec)
	defer sup.StopAll()
	sup.Start(ingestPlan(port, ""))
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	readPreambleAndMedia(t, conn)
}

func TestRequireIngestTLSWithoutFingerprint(t *testing.T) {
	var got []protocol.StreamStatus
	a := &Agent{cfg: &Config{RequireIngestTLS: true}, log: slog.New(slog.DiscardHandler)}
	a.sendFn = func(typ string, msg any) error {
		got = append(got, msg.(protocol.StreamStatus))
		return nil
	}
	// Must refuse before touching cameras/supervisor (sup is nil).
	a.OnStart(protocol.Start{CameraID: "cam", SessionID: "s"}, "")
	if len(got) != 1 || got[0].State != protocol.StateFailed || !strings.Contains(got[0].Error, "require_ingest_tls") {
		t.Errorf("status %+v", got)
	}
}
