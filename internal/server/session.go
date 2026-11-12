package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"

	"birdcam/internal/protocol"
	"birdcam/internal/server/ingest"
	"birdcam/internal/server/media"
)

// session is one start..stop streaming session of a camera.
type session struct {
	id        string
	token     string
	port      int
	cam       *camera
	startedAt time.Time

	// guarded by Server.mu
	conn        net.Conn // active ingest connection
	connGen     uint64
	connectedAt time.Time // first ingest connection
	lastBytes   int64
	lastRateAt  time.Time
	kbps        float64

	// updated by the ingest goroutine
	activeGen     atomic.Uint64
	bytes         atomic.Int64
	videoPackets  atomic.Int64
	audioPackets  atomic.Int64
	droppedPacket atomic.Int64
	lastRTP       atomic.Int64 // unix nanos
	firstRTP      atomic.Int64
	drift         atomic.Pointer[media.DriftTracker]
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// ensureStreamingLocked starts a session if the camera has viewers, the agent
// is online and nothing is running or scheduled.
func (s *Server) ensureStreamingLocked(c *camera) {
	if s.closed || len(c.viewers) == 0 || c.session != nil || c.retry != nil {
		return
	}
	if !c.dev.online() || !c.present || s.hidden(c) {
		return
	}
	if err := s.startSessionLocked(c); err != nil {
		c.lastError = err.Error()
		s.log.Warn("cannot start stream", "device", c.dev.cfg.ID, "camera", c.id, "err", err)
		s.scheduleRetryLocked(c, s.timing.NoPortRetry)
	}
}

func (s *Server) startSessionLocked(c *camera) error {
	port, ok := s.pool.Acquire()
	if !ok {
		return errNoPort
	}
	sess := &session{
		id:        randHex(12),
		token:     randHex(24),
		port:      port,
		cam:       c,
		startedAt: time.Now(),
	}
	sess.drift.Store(media.NewDriftTracker(protocol.ClockRateH264, protocol.ClockRateOpus))
	c.session = sess
	c.agentState = ""
	s.byPort[port] = sess
	prof := s.profile(c)
	s.log.Info("starting stream", "device", c.dev.cfg.ID, "camera", c.id, "session", sess.id, "port", port,
		"profile", fmt.Sprintf("%dx%d@%d audio=%v max_kbps=%d", prof.Width, prof.Height, prof.FPS, prof.Audio, prof.MaxKbps))
	c.dev.conn.send(protocol.TypeStart, protocol.Start{
		CameraID:  c.id,
		SessionID: sess.id,
		Port:      port,
		Token:     sess.token,
		Profile:   prof,

		IngestHost: s.cfg.Ingest.PublicHost,
	})
	return nil
}

// stopSessionLocked ends the camera's session, optionally telling the agent.
func (s *Server) stopSessionLocked(c *camera, sendStop bool, reason string) {
	sess := c.session
	if sess == nil {
		return
	}
	s.log.Info("stopping stream", "device", c.dev.cfg.ID, "camera", c.id, "session", sess.id, "reason", reason)
	c.session = nil
	c.agentState = ""
	if s.byPort[sess.port] == sess {
		delete(s.byPort, sess.port)
	}
	s.pool.Release(sess.port)
	sess.activeGen.Store(0)
	if sess.conn != nil {
		sess.conn.Close()
		sess.conn = nil
	}
	if sendStop && c.dev.conn != nil {
		c.dev.conn.send(protocol.TypeStop, protocol.Stop{CameraID: c.id, SessionID: sess.id})
	}
}

// restartSessionLocked stops the current session and schedules a new one with
// exponential backoff.
func (s *Server) restartSessionLocked(c *camera, reason string) {
	s.log.Warn("restarting stream", "device", c.dev.cfg.ID, "camera", c.id, "reason", reason)
	c.lastError = reason
	s.stopSessionLocked(c, true, reason)
	c.failures++
	d := s.timing.BackoffMin * time.Duration(math.Pow(2, float64(min(c.failures-1, 10))))
	s.scheduleRetryLocked(c, min(d, s.timing.BackoffMax))
}

func (s *Server) scheduleRetryLocked(c *camera, d time.Duration) {
	if c.retry != nil {
		c.retry.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(d, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c.retry != t {
			return
		}
		c.retry = nil
		s.ensureStreamingLocked(c)
	})
	c.retry = t
}

// ---- viewers (demand) ----

var (
	errUnknownCamera = errors.New("unknown camera")
	errShuttingDown  = errors.New("server shutting down")
)

// addViewerLocked registers demand for a camera. It fails if the camera is
// unknown or if a new session would be needed but no ingest port is free.
func (s *Server) addViewerLocked(v *viewer, deviceID, cameraID string) (*camera, error) {
	if s.closed {
		return nil, errShuttingDown
	}
	c := s.lookupCameraLocked(deviceID, cameraID)
	if c == nil || s.hidden(c) {
		return nil, errUnknownCamera
	}
	d := c.dev
	needsSession := c.session == nil && c.retry == nil && d.online() && c.present
	if needsSession && s.pool.InUse() >= s.pool.Size() {
		return nil, errNoPort
	}
	c.viewers[v] = struct{}{}
	v.cam = c
	if c.linger != nil {
		c.linger.Stop()
		c.linger = nil
	}
	s.ensureStreamingLocked(c)
	return c, nil
}

// lookupCameraLocked returns a known camera or nil.
func (s *Server) lookupCameraLocked(deviceID, cameraID string) *camera {
	if d := s.devices[deviceID]; d != nil {
		return d.cameras[cameraID]
	}
	return nil
}

func (s *Server) removeViewerLocked(v *viewer) {
	c := v.cam
	if c == nil {
		return
	}
	if _, ok := c.viewers[v]; !ok {
		return
	}
	delete(c.viewers, v)
	if len(c.viewers) > 0 || s.closed {
		return
	}
	if c.retry != nil {
		c.retry.Stop()
		c.retry = nil
	}
	if c.session == nil {
		return
	}
	if c.linger != nil {
		c.linger.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(s.timing.StopLinger, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c.linger != t {
			return
		}
		c.linger = nil
		if len(c.viewers) == 0 {
			s.stopSessionLocked(c, true, "no viewers")
			c.failures = 0
		}
	})
	c.linger = t
}

// ---- ingest ----

// Claim implements ingest.Handler.
func (s *Server) Claim(port int, sessionID, token string, conn net.Conn) (ingest.Sink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.byPort[port]
	if sess == nil {
		return nil, errors.New("no pending session on this port")
	}
	idOK := subtle.ConstantTimeCompare([]byte(sessionID), []byte(sess.id))
	tokOK := subtle.ConstantTimeCompare([]byte(token), []byte(sess.token))
	if idOK&tokOK != 1 {
		return nil, errors.New("session/token mismatch")
	}
	if sess.conn != nil {
		s.log.Warn("ingest: new connection replaces existing one", "session", sess.id)
		sess.conn.Close()
	}
	sess.connGen++
	sess.conn = conn
	if sess.connectedAt.IsZero() {
		sess.connectedAt = time.Now()
	}
	sess.lastRTP.Store(time.Now().UnixNano())
	sess.firstRTP.Store(0)
	sess.drift.Store(media.NewDriftTracker(protocol.ClockRateH264, protocol.ClockRateOpus))
	sess.activeGen.Store(sess.connGen)
	sess.cam.hub.NewSource()
	return &sessionSink{s: s, sess: sess, gen: sess.connGen, conn: conn}, nil
}

type sessionSink struct {
	s    *Server
	sess *session
	gen  uint64
	conn net.Conn
}

func (k *sessionSink) HandlePacket(p *rtp.Packet, size int, arrival time.Time) {
	sess := k.sess
	if sess.activeGen.Load() != k.gen {
		return // replaced or stopped
	}
	sess.bytes.Add(int64(size))
	now := arrival.UnixNano()
	sess.lastRTP.Store(now)
	sess.firstRTP.CompareAndSwap(0, now)
	drift := sess.drift.Load()
	switch p.PayloadType {
	case protocol.PayloadTypeH264:
		sess.videoPackets.Add(1)
		drift.Video(p.Timestamp, arrival)
		sess.cam.hub.WriteVideo(p)
	case protocol.PayloadTypeOpus:
		sess.audioPackets.Add(1)
		drift.Audio(p.Timestamp, arrival)
		sess.cam.hub.WriteAudio(p)
	default:
		sess.droppedPacket.Add(1)
	}
}

func (k *sessionSink) Closed(error) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if k.sess.conn == k.conn {
		k.sess.conn = nil
	}
}

// ---- watchdog ----

func (s *Server) watchdog() {
	defer s.wg.Done()
	t := time.NewTicker(s.timing.WatchdogTick)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			s.mu.Lock()
			for _, d := range s.devices {
				for _, c := range d.cameras {
					if c.session != nil {
						s.checkSessionLocked(c, now)
					}
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) checkSessionLocked(c *camera, now time.Time) {
	sess := c.session
	// bitrate
	b := sess.bytes.Load()
	if !sess.lastRateAt.IsZero() {
		if dt := now.Sub(sess.lastRateAt).Seconds(); dt > 0 {
			sess.kbps = float64(b-sess.lastBytes) * 8 / 1000 / dt
		}
	}
	sess.lastBytes, sess.lastRateAt = b, now

	if sess.connectedAt.IsZero() {
		if now.Sub(sess.startedAt) > s.timing.StartTimeout {
			s.restartSessionLocked(c, fmt.Sprintf("no ingest connection within %s", s.timing.StartTimeout))
		}
		return
	}
	last := time.Unix(0, sess.lastRTP.Load())
	if now.Sub(last) > s.timing.RTPTimeout {
		s.restartSessionLocked(c, fmt.Sprintf("no RTP for %s", s.timing.RTPTimeout))
		return
	}
	if diff, ok := sess.drift.Load().AV(); ok && math.Abs(diff) > s.timing.MaxDrift.Seconds() {
		s.restartSessionLocked(c, fmt.Sprintf("A/V drift %.2fs exceeds %s", diff, s.timing.MaxDrift))
		return
	}
	if first := sess.firstRTP.Load(); first != 0 && c.failures > 0 && now.Sub(time.Unix(0, first)) > s.timing.HealthyAfter {
		c.failures = 0
		c.lastError = ""
	}
}

// streaming reports whether RTP is currently flowing for the session.
func (s *Server) streamingLocked(sess *session) bool {
	if sess == nil || sess.conn == nil || sess.firstRTP.Load() == 0 {
		return false
	}
	return time.Since(time.Unix(0, sess.lastRTP.Load())) < 3*time.Second
}
