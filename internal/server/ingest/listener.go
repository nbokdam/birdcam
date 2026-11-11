package ingest

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pion/rtp"
)

const (
	// PreambleTimeout is how long a new connection has to send its preamble.
	PreambleTimeout = 5 * time.Second
	// ReadTimeout closes an ingest connection that sends nothing.
	ReadTimeout = 10 * time.Second
)

// Handler decides what to do with an incoming ingest connection.
type Handler interface {
	// Claim validates the preamble received on port. It returns a Sink for
	// the connection's packets or an error to reject (close) the connection.
	Claim(port int, sessionID, token string, conn net.Conn) (Sink, error)
}

// Sink receives packets of one accepted ingest connection.
type Sink interface {
	// HandlePacket is called for every parsed RTP packet. pkt is owned by the
	// callee. size is the frame size on the wire.
	HandlePacket(pkt *rtp.Packet, size int, arrival time.Time)
	// Closed is called once when the connection ends.
	Closed(err error)
}

// Server runs TCP listeners on the ingest ports.
type Server struct {
	handler Handler
	log     *slog.Logger
	tls     *tls.Config // nil = plain TCP

	mu        sync.Mutex
	listeners []net.Listener
	ports     []int
	conns     map[net.Conn]struct{}
	closed    bool
	wg        sync.WaitGroup
}

// Listen opens a listener on bind:port for each port. A port of 0 picks a
// free port (tests); Ports reports the actual ports. If tlsCfg is non-nil,
// every connection must complete a TLS handshake (within PreambleTimeout)
// before the preamble is read.
func Listen(bind string, ports []int, h Handler, tlsCfg *tls.Config, log *slog.Logger) (*Server, error) {
	s := &Server{handler: h, log: log, tls: tlsCfg, conns: map[net.Conn]struct{}{}}
	for _, p := range ports {
		ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(p)))
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("ingest listen on port %d: %w", p, err)
		}
		s.listeners = append(s.listeners, ln)
		s.ports = append(s.ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for i, ln := range s.listeners {
		s.wg.Add(1)
		go s.serve(s.ports[i], ln)
	}
	return s, nil
}

// Ports returns the actual listening ports.
func (s *Server) Ports() []int { return append([]int(nil), s.ports...) }

// Close stops all listeners and closes open connections.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	for _, ln := range s.listeners {
		ln.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) serve(port int, ln net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("ingest accept", "port", port, "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.handleConn(port, conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		}()
	}
}

func (s *Server) handleConn(port int, conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	log := s.log.With("port", port, "remote", conn.RemoteAddr().String())
	if s.tls != nil {
		tc := tls.Server(conn, s.tls)
		ctx, cancel := context.WithTimeout(context.Background(), PreambleTimeout)
		err := tc.HandshakeContext(ctx)
		cancel()
		if err != nil {
			log.Warn("ingest: tls handshake failed", "err", err)
			return
		}
		conn = tc
		defer conn.Close()
	}
	conn.SetReadDeadline(time.Now().Add(PreambleTimeout))
	br := bufio.NewReaderSize(conn, 64*1024)
	sid, token, err := ReadPreamble(br)
	if err != nil {
		log.Warn("ingest: rejected connection", "err", err)
		return
	}
	sink, err := s.handler.Claim(port, sid, token, conn)
	if err != nil {
		log.Warn("ingest: rejected connection", "session", sid, "err", err)
		return
	}
	log.Info("ingest: connection accepted", "session", sid)
	err = readPackets(conn, br, sink)
	sink.Closed(err)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		log.Info("ingest: connection ended", "session", sid, "err", err)
	} else {
		log.Info("ingest: connection closed", "session", sid)
	}
}

func readPackets(conn net.Conn, r io.Reader, sink Sink) error {
	buf := make([]byte, MaxFrameSize)
	for {
		conn.SetReadDeadline(time.Now().Add(ReadTimeout))
		frame, err := ReadFrame(r, buf)
		if errors.Is(err, ErrFrameTooLarge) {
			continue
		}
		if err != nil {
			return err
		}
		if len(frame) == 0 {
			continue
		}
		data := make([]byte, len(frame))
		copy(data, frame)
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(data); err != nil {
			continue
		}
		sink.HandlePacket(pkt, len(frame)+2, time.Now())
	}
}
