package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"birdcam/internal/protocol"
	"birdcam/internal/server/media"
)

const agentReadLimit = 1 << 20

// agentConn is one authenticated agent control connection.
type agentConn struct {
	dev *device
	ws  *websocket.Conn
	log *slog.Logger
	out chan []byte

	closeOnce sync.Once
	done      chan struct{}
	reason    string
}

// send queues a message without blocking. A full queue means the agent is
// not reading; the connection is dropped.
func (a *agentConn) send(msgType string, msg any) {
	b, err := protocol.Encode(msgType, msg)
	if err != nil {
		a.log.Error("encode message", "type", msgType, "err", err)
		return
	}
	select {
	case a.out <- b:
	case <-a.done:
	default:
		a.log.Warn("agent send queue full, dropping connection")
		go a.close("send queue full")
	}
}

func (a *agentConn) close(reason string) {
	a.closeOnce.Do(func() {
		a.reason = reason
		close(a.done)
		a.ws.Close(websocket.StatusNormalClosure, truncate(reason, 120))
	})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (a *agentConn) writer(ctx context.Context, pingEvery time.Duration) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		var msg []byte
		select {
		case <-a.done:
			return
		case <-ctx.Done():
			return
		case msg = <-a.out:
		case <-t.C:
			msg, _ = protocol.Encode(protocol.TypePing, protocol.Ping{T: time.Now().UnixMilli()})
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := a.ws.Write(wctx, websocket.MessageText, msg)
		cancel()
		if err != nil {
			a.close("write: " + err.Error())
			return
		}
	}
}

func keyEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.log.Warn("agent websocket accept", "remote", r.RemoteAddr, "err", err)
		return
	}
	ws.SetReadLimit(agentReadLimit)
	log := s.log.With("remote", r.RemoteAddr)

	reject := func(msg string) {
		log.Warn("agent rejected", "reason", msg)
		b, _ := protocol.Encode(protocol.TypeError, protocol.Error{Message: msg})
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
		_ = ws.Write(ctx, websocket.MessageText, b)
		cancel()
		ws.Close(websocket.StatusPolicyViolation, truncate(msg, 120))
	}

	hctx, cancel := context.WithTimeout(r.Context(), s.timing.HelloTimeout)
	typ, data, err := ws.Read(hctx)
	cancel()
	if err != nil {
		log.Warn("agent: no hello", "err", err)
		ws.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	var env protocol.Envelope
	var hello protocol.Hello
	if typ != websocket.MessageText || json.Unmarshal(data, &env) != nil || env.Type != protocol.TypeHello ||
		json.Unmarshal(data, &hello) != nil {
		reject("first message must be hello")
		return
	}
	if hello.ProtocolVersion != protocol.Version {
		reject(fmt.Sprintf("unsupported protocol_version %d (server speaks %d)", hello.ProtocolVersion, protocol.Version))
		return
	}
	s.mu.Lock()
	dev := s.devices[hello.DeviceID]
	s.mu.Unlock()
	// Always compare to avoid revealing whether the device exists via timing.
	want := "\x00no-such-device"
	if dev != nil {
		want = dev.cfg.Key
	}
	if !keyEqual(hello.Key, want) || dev == nil {
		reject("authentication failed")
		return
	}

	log = log.With("device", dev.cfg.ID)
	ac := &agentConn{dev: dev, ws: ws, log: log, out: make(chan []byte, 256), done: make(chan struct{})}
	ac.send(protocol.TypeWelcome, protocol.Welcome{ServerVersion: Version, IngestCertSHA256: s.ingestFingerprint})

	if !s.attachAgent(ac, hello) {
		ac.close("server shutting down")
		return
	}
	log.Info("agent connected", "agent_version", hello.AgentVersion, "model", hello.Hardware.Model,
		"cameras", len(hello.Cameras))

	ctx, cancelConn := context.WithCancel(s.ctx)
	defer cancelConn()
	go ac.writer(ctx, s.timing.PingInterval)

	err = s.readAgent(ctx, ac)
	ac.close("connection ended")
	s.detachAgent(ac)
	log.Info("agent disconnected", "err", err, "reason", ac.reason)
}

func (s *Server) readAgent(ctx context.Context, ac *agentConn) error {
	for {
		rctx, cancel := context.WithTimeout(ctx, s.timing.DeadAfter)
		typ, data, err := ac.ws.Read(rctx)
		cancel()
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			ac.log.Warn("agent: bad message", "err", err)
			continue
		}
		switch env.Type {
		case protocol.TypePing:
			var p protocol.Ping
			_ = json.Unmarshal(data, &p)
			ac.send(protocol.TypePong, protocol.Pong{T: p.T})
		case protocol.TypePong:
		case protocol.TypeCameras:
			var m protocol.Cameras
			if err := json.Unmarshal(data, &m); err != nil {
				ac.log.Warn("agent: bad cameras message", "err", err)
				continue
			}
			s.mu.Lock()
			if ac.dev.conn == ac {
				s.updateCamerasLocked(ac.dev, m.Cameras)
			}
			s.mu.Unlock()
		case protocol.TypeStreamStatus:
			var m protocol.StreamStatus
			if err := json.Unmarshal(data, &m); err != nil {
				ac.log.Warn("agent: bad stream_status message", "err", err)
				continue
			}
			s.handleStreamStatus(ac, m)
		case protocol.TypeHello:
			ac.log.Warn("agent: unexpected second hello")
		default:
			ac.log.Debug("agent: ignoring message", "type", env.Type)
		}
	}
}

// attachAgent registers ac as the device's connection, replacing (closing)
// any older connection.
func (s *Server) attachAgent(ac *agentConn, hello protocol.Hello) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	dev := ac.dev
	if old := dev.conn; old != nil {
		ac.log.Info("agent reconnected, replacing old connection")
		s.dropAgentLocked(dev)
		go old.close("replaced by new connection")
	}
	dev.conn = ac
	dev.hello = hello
	s.updateCamerasLocked(dev, hello.Cameras)
	return true
}

func (s *Server) detachAgent(ac *agentConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ac.dev.conn != ac {
		return // already replaced
	}
	s.dropAgentLocked(ac.dev)
}

// dropAgentLocked tears down the device's sessions. Viewers stay attached to
// the camera hubs and get a fresh session once the agent is back.
func (s *Server) dropAgentLocked(dev *device) {
	dev.conn = nil
	for _, c := range dev.cameras {
		c.present = false
		if c.retry != nil {
			c.retry.Stop()
			c.retry = nil
		}
		if c.linger != nil {
			c.linger.Stop()
			c.linger = nil
		}
		if c.session != nil {
			s.stopSessionLocked(c, false, "agent disconnected")
		}
		c.failures = 0
		c.agentState = ""
	}
}

func (s *Server) updateCamerasLocked(dev *device, cams []protocol.Camera) {
	seen := map[string]bool{}
	for _, info := range cams {
		if info.ID == "" {
			continue
		}
		seen[info.ID] = true
		c := dev.cameras[info.ID]
		if c == nil {
			c = &camera{dev: dev, id: info.ID, hub: media.NewHub(), viewers: map[*viewer]struct{}{}}
			dev.cameras[info.ID] = c
		}
		c.info = info
		c.present = true
	}
	for id, c := range dev.cameras {
		if seen[id] {
			continue
		}
		c.present = false
		if c.retry != nil {
			c.retry.Stop()
			c.retry = nil
		}
		if c.session != nil {
			s.stopSessionLocked(c, true, "camera removed by agent")
		}
	}
	for _, c := range dev.cameras {
		s.ensureStreamingLocked(c)
	}
}

func (s *Server) handleStreamStatus(ac *agentConn, m protocol.StreamStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ac.dev.conn != ac {
		return
	}
	c := ac.dev.cameras[m.CameraID]
	if c == nil || c.session == nil || c.session.id != m.SessionID {
		return // stale status for an old session
	}
	log := ac.log.With("camera", c.id, "session", m.SessionID)
	c.agentState = m.State
	if m.Mode != "" {
		c.mode = m.Mode
	}
	switch m.State {
	case protocol.StateStarting, protocol.StateRunning:
		log.Info("stream status", "state", m.State, "mode", m.Mode)
	case protocol.StateFailed:
		c.lastError = m.Error
		s.restartSessionLocked(c, "agent reported failure: "+m.Error)
	case protocol.StateStopped:
		s.restartSessionLocked(c, "agent stopped stream unexpectedly")
	default:
		log.Warn("unknown stream state", "state", m.State)
	}
}

var errNoPort = errors.New("no free ingest port")
