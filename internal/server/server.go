// Package server implements birdcam-server: agent control channel, ingest
// session management, WebRTC fan-out to browsers and the HTTP API.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"birdcam/internal/protocol"
	"birdcam/internal/server/auth"
	"birdcam/internal/server/config"
	"birdcam/internal/server/ingest"
	"birdcam/internal/server/media"
)

// Version is reported to agents in Welcome.
var Version = "dev" // set with -ldflags "-X birdcam/internal/server.Version=..."

// Timing holds all timeouts; tests shrink them.
type Timing struct {
	HelloTimeout   time.Duration // first message from an agent
	PingInterval   time.Duration
	DeadAfter      time.Duration
	StopLinger     time.Duration // keep streaming after the last viewer left
	StartTimeout   time.Duration // no ingest connection after start
	RTPTimeout     time.Duration // no RTP while running
	MaxDrift       time.Duration // max |video drift - audio drift|
	WatchdogTick   time.Duration
	ViewerGrace    time.Duration // PeerConnection disconnected for longer → viewer gone
	ViewerConnect  time.Duration // PeerConnection must connect within
	BackoffMin     time.Duration
	BackoffMax     time.Duration
	HealthyAfter   time.Duration // streaming this long resets the backoff
	NoPortRetry    time.Duration
	ICEGatherLimit time.Duration
}

// DefaultTiming returns the production timeouts.
func DefaultTiming() Timing {
	return Timing{
		HelloTimeout:   10 * time.Second,
		PingInterval:   protocol.PingInterval,
		DeadAfter:      protocol.DeadAfter,
		StopLinger:     protocol.StopLinger,
		StartTimeout:   20 * time.Second,
		RTPTimeout:     10 * time.Second,
		MaxDrift:       time.Second,
		WatchdogTick:   time.Second,
		ViewerGrace:    15 * time.Second,
		ViewerConnect:  30 * time.Second,
		BackoffMin:     time.Second,
		BackoffMax:     30 * time.Second,
		HealthyAfter:   30 * time.Second,
		NoPortRetry:    5 * time.Second,
		ICEGatherLimit: 10 * time.Second,
	}
}

// Options configure a Server.
type Options struct {
	Config *config.Config
	Logger *slog.Logger
	Timing *Timing // nil = DefaultTiming
	// IngestPorts overrides the configured port range (0 = pick free port).
	IngestPorts []int
	// DisableWebRTC skips the WebRTC engine (control/ingest tests).
	DisableWebRTC bool
	// Auth tunes the OIDC client (tests).
	Auth auth.Options
}

// Server is a running birdcam-server.
type Server struct {
	cfg    *config.Config
	log    *slog.Logger
	timing Timing

	ingest *ingest.Server
	pool   *ingest.PortPool
	rtc    *rtcEngine
	auth   *auth.Auth // nil when login is disabled
	// ingestFingerprint is the SHA-256 of the ingest TLS certificate ("" =
	// plain TCP ingest).
	ingestFingerprint string
	// agentBins are the agent builds offered for self-update, by platform
	// ("linux/arm64"); nil when updates are off.
	agentBins map[string]*agentBinary

	mu      sync.Mutex
	closed  bool
	devices map[string]*device
	byPort  map[int]*session
	viewers map[string]*viewer

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a server: it binds the ingest ports and the WebRTC UDP port.
// Serve HTTP with Handler().
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, errors.New("server: no config")
	}
	t := DefaultTiming()
	if opts.Timing != nil {
		t = *opts.Timing
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:     opts.Config,
		log:     log,
		timing:  t,
		devices: map[string]*device{},
		byPort:  map[int]*session{},
		viewers: map[string]*viewer{},
		ctx:     ctx,
		cancel:  cancel,
	}
	for _, d := range opts.Config.Devices {
		s.devices[d.ID] = &device{cfg: d, cameras: map[string]*camera{}}
	}
	cfg := opts.Config
	if cfg.Auth.Enabled() {
		a, err := auth.New(ctx, cfg, log, opts.Auth)
		if err != nil {
			cancel()
			return nil, err
		}
		s.auth = a
	}
	s.warnAccessConfig()
	s.agentBins = loadAgentBinaries(cfg.AgentUpdates, Version, log)
	var tlsCfg *tls.Config
	if cfg.Ingest.TLSEnabled() {
		cert, fp, err := ingest.LoadOrCreateCert(cfg.Ingest.CertFile, cfg.Ingest.KeyFile, cfg.StateDir)
		if err != nil {
			cancel()
			return nil, err
		}
		tlsCfg = ingest.ServerTLSConfig(cert)
		s.ingestFingerprint = fp
		log.Info("ingest TLS enabled; agents pin the certificate fingerprint received over the control "+
			"channel, so agents must connect with wss:// for the pin to be trustworthy", "sha256", fp)
	} else {
		log.Warn("ingest TLS disabled: media from agents travels unencrypted")
	}
	ports := opts.IngestPorts
	if ports == nil {
		ports = cfg.Ports()
	}
	ing, err := ingest.Listen(cfg.Ingest.Bind, ports, s, tlsCfg, log)
	if err != nil {
		cancel()
		return nil, err
	}
	s.ingest = ing
	s.pool = ingest.NewPortPool(ing.Ports())
	if !opts.DisableWebRTC {
		s.rtc, err = newRTCEngine(opts.Config.WebRTC, log)
		if err != nil {
			ing.Close()
			cancel()
			return nil, fmt.Errorf("webrtc: %w", err)
		}
	}
	s.wg.Add(1)
	go s.watchdog()
	return s, nil
}

// IngestPorts returns the ports actually listened on.
func (s *Server) IngestPorts() []int { return s.ingest.Ports() }

// Handler returns the HTTP handler (web page, API, /agent).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent", s.handleAgent)
	mux.HandleFunc("GET /agent/binary/{platform}", s.handleAgentBinary)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("GET /api/cameras", s.handleCameras)
	if s.auth != nil {
		s.auth.Register(mux)
	}
	mux.HandleFunc("POST /api/cameras/{device}/{camera}/whep", s.handleWHEP)
	mux.HandleFunc("DELETE /api/viewers/{id}", s.handleViewerDelete)
	mux.HandleFunc("PATCH /api/viewers/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "trickle ICE not supported")
	})
	mux.Handle("GET /", staticHandler())
	return mux
}

// Close tears everything down.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	var conns []*agentConn
	var vs []*viewer
	for _, d := range s.devices {
		if d.conn != nil {
			conns = append(conns, d.conn)
		}
		for _, c := range d.cameras {
			c.stopTimers()
			if c.session != nil {
				s.stopSessionLocked(c, false, "server shutdown")
			}
		}
	}
	for _, v := range s.viewers {
		vs = append(vs, v)
	}
	s.mu.Unlock()
	s.cancel()
	for _, c := range conns {
		c.close("server shutting down")
	}
	for _, v := range vs {
		v.close()
	}
	s.ingest.Close()
	s.wg.Wait()
	if s.rtc != nil {
		s.rtc.close()
	}
}

// ---- state ----

type device struct {
	cfg     config.Device
	conn    *agentConn // nil while offline
	hello   protocol.Hello
	cameras map[string]*camera
}

func (d *device) online() bool { return d.conn != nil }

type camera struct {
	dev     *device
	id      string
	info    protocol.Camera
	present bool // reported by the connected agent
	hub     *media.Hub

	viewers  map[*viewer]struct{}
	session  *session
	linger   *time.Timer
	retry    *time.Timer
	failures int

	agentState string // last stream_status state
	mode       string
	lastError  string
}

func (c *camera) stopTimers() {
	if c.linger != nil {
		c.linger.Stop()
		c.linger = nil
	}
	if c.retry != nil {
		c.retry.Stop()
		c.retry = nil
	}
}

func (s *Server) override(c *camera) (config.CameraOverride, bool) {
	return s.cfg.Camera(c.dev.cfg.ID, c.id)
}

func (s *Server) cameraName(c *camera) string {
	if o, ok := s.override(c); ok && o.Name != "" {
		return o.Name
	}
	if c.info.Name != "" {
		return c.info.Name
	}
	return c.id
}

func (s *Server) hidden(c *camera) bool {
	o, ok := s.override(c)
	return ok && o.Hidden
}

// rotation is the effective display rotation: server override, else the
// agent-reported value (if valid), else 0.
func (s *Server) rotation(c *camera) int {
	if o, ok := s.override(c); ok && o.Rotation != nil {
		return *o.Rotation
	}
	if config.ValidRotation(c.info.Rotation) {
		return c.info.Rotation
	}
	return 0
}

// access is the result of an access check for a camera.
type access int

const (
	accessAllowed access = iota
	// accessLogin: the camera is private and the viewer is anonymous.
	accessLogin
	// accessDenied: the camera is hidden, or private and the viewer is not
	// in a permitted group, or private while login is disabled.
	accessDenied
)

// accessFor decides whether user (nil = anonymous) may see/watch c.
func (s *Server) accessFor(c *camera, user *auth.User) access {
	if s.hidden(c) {
		return accessDenied
	}
	groups := s.cfg.CameraAccess(c.dev.cfg.ID, c.id)
	switch {
	case groups == nil:
		return accessAllowed
	case s.auth == nil:
		return accessDenied
	case user == nil:
		return accessLogin
	case user.InAnyGroup(groups):
		return accessAllowed
	}
	return accessDenied
}

// warnAccessConfig logs cameras that are unreachable because they are
// private while login is disabled.
func (s *Server) warnAccessConfig() {
	if s.auth != nil {
		return
	}
	for _, d := range s.cfg.Devices {
		if len(d.Groups) > 0 {
			s.log.Warn("device has groups but auth is disabled (no auth.oidc.issuer): its cameras are hidden unless public: true",
				"device", d.ID)
		}
	}
	for _, o := range s.cfg.Cameras {
		if len(o.Groups) > 0 && !o.Public {
			s.log.Warn("camera has groups but auth is disabled (no auth.oidc.issuer): it is hidden",
				"device", o.DeviceID, "camera", o.CameraID)
		}
	}
}

// profile builds the Start profile for a camera. The device budget is split
// evenly over the device's cameras that are streaming or starting, including
// c itself (sessions already running keep the bitrate they were started
// with). A per-camera max_kbps caps the result.
func (s *Server) profile(c *camera) protocol.Profile {
	dp := s.cfg.DefaultProfile
	p := protocol.Profile{Width: dp.Width, Height: dp.Height, FPS: dp.FPS, Audio: dp.Audio != nil && *dp.Audio}
	if c.dev.cfg.MaxKbps > 0 {
		n := 1 // c
		for _, other := range c.dev.cameras {
			if other != c && other.session != nil {
				n++
			}
		}
		p.MaxKbps = c.dev.cfg.MaxKbps / n
	}
	if o, ok := s.override(c); ok {
		if o.MaxKbps > 0 && (p.MaxKbps == 0 || o.MaxKbps < p.MaxKbps) {
			p.MaxKbps = o.MaxKbps
		}
		if o.Width > 0 && o.Height > 0 {
			p.Width, p.Height = o.Width, o.Height
		}
		if o.FPS > 0 {
			p.FPS = o.FPS
		}
		if o.Audio != nil {
			p.Audio = *o.Audio
		}
	}
	if c.info.AudioDevice == "" {
		p.Audio = false
	}
	return p
}

// sortedCameras returns all cameras ordered by device then camera name.
func (s *Server) sortedCameras() []*camera {
	var out []*camera
	for _, d := range s.devices {
		for _, c := range d.cameras {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.dev.cfg.Name != b.dev.cfg.Name {
			return a.dev.cfg.Name < b.dev.cfg.Name
		}
		if a.dev.cfg.ID != b.dev.cfg.ID {
			return a.dev.cfg.ID < b.dev.cfg.ID
		}
		return s.cameraName(a) < s.cameraName(b)
	})
	return out
}
