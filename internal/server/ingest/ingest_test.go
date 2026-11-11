package ingest

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"

	"birdcam/internal/protocol"
)

func TestParsePreamble(t *testing.T) {
	sid, tok, err := ParsePreamble(protocol.IngestPreamble("abc", "def"))
	if err != nil || sid != "abc" || tok != "def" {
		t.Fatalf("got %q %q %v", sid, tok, err)
	}
	for _, bad := range []string{
		"BIRDCAM1 abc def",     // no newline
		"BIRDCAM2 abc def\n",   // wrong magic
		"BIRDCAM1 abc\n",       // missing token
		"BIRDCAM1 abc def x\n", // extra field
		"BIRDCAM1  def\n",      // empty session
		"BIRDCAM1 abc def\r\n", // CR is part of token -> still parses, checked below
		"\n",
	} {
		_, tok, err := ParsePreamble(bad)
		if bad == "BIRDCAM1 abc def\r\n" {
			if err == nil && tok == "def" {
				t.Errorf("CRLF should not yield clean token")
			}
			continue
		}
		if !errors.Is(err, ErrBadPreamble) {
			t.Errorf("%q: want ErrBadPreamble, got %v", bad, err)
		}
	}
}

func TestReadPreamble(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("BIRDCAM1 s t\n\x00\x02ab"))
	sid, tok, err := ReadPreamble(r)
	if err != nil || sid != "s" || tok != "t" {
		t.Fatalf("got %q %q %v", sid, tok, err)
	}
	// The reader must be positioned right after the newline.
	f, err := ReadFrame(r, make([]byte, 100))
	if err != nil || string(f) != "ab" {
		t.Fatalf("frame after preamble: %q %v", f, err)
	}

	long := "BIRDCAM1 " + strings.Repeat("x", 1000) + " t\n"
	if _, _, err := ReadPreamble(bufio.NewReader(strings.NewReader(long))); !errors.Is(err, ErrPreambleTooLong) {
		t.Fatalf("want ErrPreambleTooLong, got %v", err)
	}
	if _, _, err := ReadPreamble(bufio.NewReader(strings.NewReader("BIRDCAM1 s"))); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestReadFrame(t *testing.T) {
	var stream []byte
	stream = AppendFrame(stream, []byte("hello"))
	stream = AppendFrame(stream, nil)
	stream = AppendFrame(stream, bytes.Repeat([]byte{1}, 50)) // too large for buf
	stream = AppendFrame(stream, []byte("after"))
	stream = append(stream, 0x00, 0x05, 'x') // truncated
	r := bytes.NewReader(stream)
	buf := make([]byte, 10)

	f, err := ReadFrame(r, buf)
	if err != nil || string(f) != "hello" {
		t.Fatalf("frame 1: %q %v", f, err)
	}
	f, err = ReadFrame(r, buf)
	if err != nil || len(f) != 0 {
		t.Fatalf("empty frame: %q %v", f, err)
	}
	if _, err = ReadFrame(r, buf); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
	f, err = ReadFrame(r, buf)
	if err != nil || string(f) != "after" {
		t.Fatalf("stream out of sync after oversized frame: %q %v", f, err)
	}
	if _, err = ReadFrame(r, buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated: want ErrUnexpectedEOF, got %v", err)
	}
	if _, err = ReadFrame(r, buf); !errors.Is(err, io.EOF) {
		t.Fatalf("end: want EOF, got %v", err)
	}
}

func TestPortPool(t *testing.T) {
	p := NewPortPool([]int{9001, 9002, 9003})
	got := map[int]bool{}
	for i := 0; i < 3; i++ {
		port, ok := p.Acquire()
		if !ok || got[port] {
			t.Fatalf("acquire %d: %d %v", i, port, ok)
		}
		got[port] = true
	}
	if _, ok := p.Acquire(); ok {
		t.Fatal("pool should be exhausted")
	}
	if p.InUse() != 3 {
		t.Fatalf("InUse = %d", p.InUse())
	}
	p.Release(9002)
	p.Release(9002) // double release is harmless
	port, ok := p.Acquire()
	if !ok || port != 9002 {
		t.Fatalf("reacquire: %d %v", port, ok)
	}
	p.Release(9001)
	p.Release(9003)
	// Round-robin: after 9002 the next free port is 9003.
	if port, _ := p.Acquire(); port != 9003 {
		t.Fatalf("round robin: got %d", port)
	}
}

type testSink struct {
	mu     sync.Mutex
	pkts   []*rtp.Packet
	closed chan error
}

func (s *testSink) HandlePacket(p *rtp.Packet, _ int, _ time.Time) {
	s.mu.Lock()
	s.pkts = append(s.pkts, p)
	s.mu.Unlock()
}
func (s *testSink) Closed(err error) { s.closed <- err }

type testHandler struct{ sink *testSink }

func (h *testHandler) Claim(port int, sid, tok string, _ net.Conn) (Sink, error) {
	if sid != "sess" || tok != "secret" {
		return nil, errors.New("bad token")
	}
	return h.sink, nil
}

func TestListener(t *testing.T) {
	sink := &testSink{closed: make(chan error, 1)}
	srv, err := Listen("127.0.0.1", []int{0}, &testHandler{sink: sink}, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.Ports()[0]))

	// Wrong token: closed immediately.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte(protocol.IngestPreamble("sess", "wrong")))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("expected server to close bad connection, got %v", err)
	}
	c.Close()

	// Good token: packets are delivered.
	c, err = net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	var stream []byte
	stream = append(stream, protocol.IngestPreamble("sess", "secret")...)
	for i := 0; i < 5; i++ {
		p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(i)}, Payload: []byte{0x41, byte(i)}}
		b, _ := p.Marshal()
		stream = AppendFrame(stream, b)
	}
	stream = AppendFrame(stream, []byte{1, 2, 3}) // not RTP: dropped
	c.Write(stream)
	c.Close()
	select {
	case <-sink.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("sink not closed")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.pkts) != 5 {
		t.Fatalf("got %d packets, want 5", len(sink.pkts))
	}
	for i, p := range sink.pkts {
		if p.SequenceNumber != uint16(i) || p.Payload[1] != byte(i) {
			t.Fatalf("packet %d mismatch: %+v", i, p.Header)
		}
	}
}

func TestTLSListenerAndCert(t *testing.T) {
	dir := t.TempDir()
	cert, fp, err := LoadOrCreateCert("", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, GeneratedKeyName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	if _, fp2, err := LoadOrCreateCert("", "", dir); err != nil || fp2 != fp {
		t.Fatalf("fingerprint not stable: %s vs %s (%v)", fp2, fp, err)
	}
	// Explicit cert/key files.
	if _, fp3, err := LoadOrCreateCert(filepath.Join(dir, GeneratedCertName), filepath.Join(dir, GeneratedKeyName), ""); err != nil || fp3 != fp {
		t.Fatalf("explicit files: %s %v", fp3, err)
	}

	sink := &testSink{closed: make(chan error, 1)}
	srv, err := Listen("127.0.0.1", []int{0}, &testHandler{sink: sink}, ServerTLSConfig(cert), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.Ports()[0]))

	// Plain TCP with a valid preamble is dropped.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte(protocol.IngestPreamble("sess", "secret")))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.Copy(io.Discard, c)
	c.Close()
	// TLS 1.2 is refused.
	tc12 := PinnedClientConfig(fp)
	tc12.MinVersion, tc12.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if c, err := tls.Dial("tcp", addr, tc12); err == nil {
		c.Close()
		t.Fatal("TLS 1.2 accepted")
	}
	// Silent client: handshake times out and the server closes.
	// (Covered by PreambleTimeout; not waited for here.)

	tc, err := tls.Dial("tcp", addr, PinnedClientConfig(fp))
	if err != nil {
		t.Fatal(err)
	}
	stream := []byte(protocol.IngestPreamble("sess", "secret"))
	p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96}, Payload: []byte{0x41}}
	b, _ := p.Marshal()
	stream = AppendFrame(stream, b)
	tc.Write(stream)
	tc.Close()
	select {
	case <-sink.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("sink not closed")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.pkts) != 1 {
		t.Fatalf("got %d packets over TLS, want 1", len(sink.pkts))
	}
}
