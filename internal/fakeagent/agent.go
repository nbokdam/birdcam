// Package fakeagent is a minimal birdcam agent for tests and end-to-end
// experiments: it speaks the control protocol and, on Start, streams H.264
// (from a file or synthetic) plus silent Opus over the ingest connection.
package fakeagent

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"birdcam/internal/protocol"
	"birdcam/internal/server/ingest"
)

// opusSilence is a 20 ms Opus (CELT) silence frame.
var opusSilence = []byte{0xf8, 0xff, 0xfe}

// Config configures a fake agent.
type Config struct {
	// ServerURL is the control WebSocket, e.g. ws://localhost:8080/agent.
	ServerURL string
	DeviceID  string
	Key       string
	// Cameras reported in hello. Default: one camera "fakecam".
	Cameras []protocol.Camera
	// IngestHost to connect to; default the ingest_host of Start, else the
	// host of ServerURL.
	IngestHost string
	// AgentVersion and Platform are reported in hello (defaults "fakeagent"
	// and none), to exercise the server's update offer.
	AgentVersion string
	Platform     string
	// NewSource creates the video source for a session. Default synthetic.
	NewSource func() (FrameSource, error)
	FPS       int  // default 30
	NoAudio   bool // do not send Opus even if the profile asks for audio
	// AudioRate scales the audio RTP clock (1 = correct). Values != 1 make
	// audio drift against video, to exercise the server's drift watchdog.
	AudioRate float64
	// NoStream makes the agent acknowledge Start without streaming.
	NoStream bool
	// ProtocolVersion overrides protocol.Version (tests).
	ProtocolVersion int
	// IngestFingerprint, if set, overrides the ingest certificate
	// fingerprint received in Welcome (tests: simulate a wrong pin).
	IngestFingerprint string
	// PlainIngest connects without TLS even if Welcome carries a
	// fingerprint (tests).
	PlainIngest bool
	Log         *slog.Logger
}

// Event is something the agent received from the server.
type Event struct {
	Type    string
	Welcome protocol.Welcome
	Start   protocol.Start
	Stop    protocol.Stop
	Error   string
}

// Agent is a fake birdcam agent.
type Agent struct {
	cfg    Config
	log    *slog.Logger
	Events chan Event

	ws      *websocket.Conn
	writeMu sync.Mutex

	mu          sync.Mutex
	sessions    map[string]context.CancelFunc // by camera id
	fingerprint string                        // from Welcome

	// PacketsSent counts RTP packets written to ingest connections.
	PacketsSent atomic.Int64
}

// New creates an agent.
func New(cfg Config) *Agent {
	if cfg.FPS == 0 {
		cfg.FPS = 30
	}
	if cfg.AudioRate == 0 {
		cfg.AudioRate = 1
	}
	if len(cfg.Cameras) == 0 {
		cfg.Cameras = []protocol.Camera{{
			ID: "fakecam", Name: "Fake camera", VideoDevice: "/dev/null", AudioDevice: "hw:CARD=Fake,DEV=0",
			Formats: []protocol.VideoFormat{{Codec: "h264", Width: 1920, Height: 1080, FPS: []int{30}}},
		}}
	}
	if cfg.NewSource == nil {
		cfg.NewSource = func() (FrameSource, error) { return SyntheticSource(30, 3000), nil }
	}
	if cfg.ProtocolVersion == 0 {
		cfg.ProtocolVersion = protocol.Version
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Agent{cfg: cfg, log: log.With("fakeagent", cfg.DeviceID), Events: make(chan Event, 64),
		sessions: map[string]context.CancelFunc{}}
}

// ErrRejected is returned by Run when the server answers hello with error.
var ErrRejected = errors.New("rejected by server")

// Run connects once and serves until the connection or ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	ws, _, err := websocket.Dial(ctx, a.cfg.ServerURL, nil)
	if err != nil {
		return err
	}
	a.ws = ws
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		a.stopAll()
		ws.CloseNow()
	}()
	if err := a.send(ctx, protocol.TypeHello, protocol.Hello{
		ProtocolVersion: a.cfg.ProtocolVersion,
		DeviceID:        a.cfg.DeviceID,
		Key:             a.cfg.Key,
		AgentVersion:    cmp.Or(a.cfg.AgentVersion, "fakeagent"),
		Platform:        a.cfg.Platform,
		Hardware:        protocol.Hardware{Model: "fake"},
		Cameras:         a.cfg.Cameras,
	}); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(protocol.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = a.send(ctx, protocol.TypePing, protocol.Ping{T: time.Now().UnixMilli()})
			}
		}
	}()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		var env protocol.Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeWelcome:
			var wm protocol.Welcome
			_ = json.Unmarshal(data, &wm)
			a.mu.Lock()
			a.fingerprint = wm.IngestCertSHA256
			a.mu.Unlock()
			a.emit(Event{Type: env.Type, Welcome: wm})
		case protocol.TypeError:
			var e protocol.Error
			_ = json.Unmarshal(data, &e)
			a.emit(Event{Type: env.Type, Error: e.Message})
			return fmt.Errorf("%w: %s", ErrRejected, e.Message)
		case protocol.TypePing:
			var p protocol.Ping
			_ = json.Unmarshal(data, &p)
			_ = a.send(ctx, protocol.TypePong, protocol.Pong{T: p.T})
		case protocol.TypeStart:
			var s protocol.Start
			_ = json.Unmarshal(data, &s)
			a.emit(Event{Type: env.Type, Start: s})
			a.start(ctx, s)
		case protocol.TypeStop:
			var s protocol.Stop
			_ = json.Unmarshal(data, &s)
			a.emit(Event{Type: env.Type, Stop: s})
			a.stop(s.CameraID, s.SessionID)
		}
	}
}

func (a *Agent) emit(e Event) {
	select {
	case a.Events <- e:
	default:
	}
}

func (a *Agent) send(ctx context.Context, typ string, msg any) error {
	b, err := protocol.Encode(typ, msg)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return a.ws.Write(wctx, websocket.MessageText, b)
}

// SendCameras sends a cameras update.
func (a *Agent) SendCameras(ctx context.Context, cams []protocol.Camera) error {
	return a.send(ctx, protocol.TypeCameras, protocol.Cameras{Cameras: cams})
}

// SendStatus sends a stream_status message.
func (a *Agent) SendStatus(ctx context.Context, st protocol.StreamStatus) error {
	return a.send(ctx, protocol.TypeStreamStatus, st)
}

func (a *Agent) stopAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, c := range a.sessions {
		c()
		delete(a.sessions, id)
	}
}

func (a *Agent) stop(cameraID, sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.sessions[cameraID]; ok {
		c()
		delete(a.sessions, cameraID)
	}
}

func (a *Agent) start(ctx context.Context, s protocol.Start) {
	a.mu.Lock()
	if c, ok := a.sessions[s.CameraID]; ok {
		c()
	}
	sctx, cancel := context.WithCancel(ctx)
	a.sessions[s.CameraID] = cancel
	a.mu.Unlock()
	if a.cfg.NoStream {
		return
	}
	go func() {
		status := func(state, errMsg string) {
			_ = a.send(ctx, protocol.TypeStreamStatus, protocol.StreamStatus{
				CameraID: s.CameraID, SessionID: s.SessionID, State: state,
				Mode: protocol.ModeH264Passthrough, Error: errMsg,
			})
		}
		status(protocol.StateStarting, "")
		err := a.stream(sctx, s, func() { status(protocol.StateRunning, "") })
		if sctx.Err() != nil {
			return // stopped
		}
		a.log.Warn("stream ended", "camera", s.CameraID, "err", err)
		msg := "stream ended"
		if err != nil {
			msg = err.Error()
		}
		status(protocol.StateFailed, msg)
	}()
}

func (a *Agent) ingestAddr(s protocol.Start) (string, error) {
	port := s.Port
	host := cmp.Or(a.cfg.IngestHost, s.IngestHost)
	if host == "" {
		u, err := url.Parse(a.cfg.ServerURL)
		if err != nil {
			return "", err
		}
		host = u.Hostname()
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// stream connects to the ingest port and sends RTP until ctx ends.
func (a *Agent) stream(ctx context.Context, s protocol.Start, running func()) error {
	src, err := a.cfg.NewSource()
	if err != nil {
		return err
	}
	addr, err := a.ingestAddr(s)
	if err != nil {
		return err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	a.mu.Lock()
	fp := a.fingerprint
	a.mu.Unlock()
	if a.cfg.IngestFingerprint != "" {
		fp = a.cfg.IngestFingerprint
	}
	if fp != "" && !a.cfg.PlainIngest {
		tc := tls.Client(conn, ingest.PinnedClientConfig(fp))
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			return fmt.Errorf("ingest tls: %w", err)
		}
		conn = tc
	}
	go func() { <-ctx.Done(); conn.Close() }()
	if _, err := conn.Write([]byte(protocol.IngestPreamble(s.SessionID, s.Token))); err != nil {
		return err
	}
	running()
	a.log.Info("streaming", "camera", s.CameraID, "session", s.SessionID, "addr", addr)

	w := &frameWriter{w: bufio.NewWriterSize(conn, 64*1024)}
	ssrcV, ssrcA := uint32(time.Now().UnixNano()), uint32(time.Now().UnixNano()>>8)
	vp := rtp.NewPacketizer(protocol.MaxRTPPacketSize, protocol.PayloadTypeH264, ssrcV,
		&codecs.H264Payloader{}, rtp.NewRandomSequencer(), protocol.ClockRateH264)
	ap := rtp.NewPacketizer(protocol.MaxRTPPacketSize, protocol.PayloadTypeOpus, ssrcA,
		&codecs.OpusPayloader{}, rtp.NewRandomSequencer(), protocol.ClockRateOpus)
	audio := s.Profile.Audio && !a.cfg.NoAudio

	frameDur := time.Second / time.Duration(a.cfg.FPS)
	vt := time.NewTicker(frameDur)
	defer vt.Stop()
	at := time.NewTicker(20 * time.Millisecond)
	defer at.Stop()
	if !audio {
		at.Stop()
	}
	audioSamples := uint32(960 * a.cfg.AudioRate)
	for {
		var pkts []*rtp.Packet
		select {
		case <-ctx.Done():
			return nil
		case <-vt.C:
			pkts = vp.Packetize(src.Next(), uint32(protocol.ClockRateH264/a.cfg.FPS))
		case <-at.C:
			pkts = ap.Packetize(opusSilence, audioSamples)
		}
		for _, p := range pkts {
			if err := w.write(p); err != nil {
				return err
			}
			a.PacketsSent.Add(1)
		}
		if err := w.w.Flush(); err != nil {
			return err
		}
	}
}

type frameWriter struct {
	w   *bufio.Writer
	buf []byte
}

func (f *frameWriter) write(p *rtp.Packet) error {
	b, err := p.Marshal()
	if err != nil {
		return err
	}
	f.buf = binary.BigEndian.AppendUint16(f.buf[:0], uint16(len(b)))
	f.buf = append(f.buf, b...)
	_, err = f.w.Write(f.buf)
	return err
}
