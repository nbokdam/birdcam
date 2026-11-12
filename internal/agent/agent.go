package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"runtime"
	"sync"
	"time"

	"birdcam/internal/protocol"
)

// agentPlatform is reported in Hello so the server can offer a matching build.
var agentPlatform = runtime.GOOS + "/" + runtime.GOARCH

// RescanInterval is how often the camera set is re-discovered.
const RescanInterval = 10 * time.Second

// Agent ties config, discovery, control connection and stream supervisor together.
type Agent struct {
	cfg     *Config
	version string
	log     *slog.Logger
	disc    *Discoverer
	hw      protocol.Hardware

	client *Client
	sendFn func(string, any) error // test hook; defaults to client.Send
	sup    *Supervisor
	// applyBitrate sets the camera's native H.264 bitrate (test hook).
	applyBitrate func(node string, unit uint8, bps int) (int, error)

	mu      sync.Mutex
	cameras []Discovered

	updater *updater
	// runCtx/restart are set by Run; restart ends Run so that systemd starts
	// the agent again (used to switch to a downloaded update).
	runCtx  context.Context
	restart context.CancelFunc
}

func New(cfg *Config, version string, log *slog.Logger) *Agent {
	a := &Agent{
		cfg: cfg, version: version, log: log,
		disc: NewDiscoverer(),
		hw:   DetectHardware("/sys", "/proc/device-tree/model", cfg.MaxUploadKbps),

		applyBitrate: ApplyBitrate,
	}
	a.updater = newUpdater(cfg, version, log)
	a.client = &Client{
		URL:     cfg.ServerURL,
		Hello:   a.hello,
		Handler: a,
		Log:     log,
	}
	a.sup = NewSupervisor(SupervisorConfig{
		GstPath:    cfg.GstLaunch,
		ServerHost: cfg.ServerHost(),
		Log:        log,
		Report: func(s protocol.StreamStatus) {
			if err := a.client.Send(protocol.TypeStreamStatus, s); err != nil {
				log.Debug("drop stream_status", "state", s.State, "err", err)
			}
		},
	})
	return a
}

// Scan discovers cameras with overrides applied.
func Scan(d *Discoverer, cfg *Config) []Discovered {
	return ApplyOverrides(d.Discover(), cfg.Cameras)
}

func (a *Agent) hello() protocol.Hello {
	a.mu.Lock()
	defer a.mu.Unlock()
	return protocol.Hello{
		ProtocolVersion: protocol.Version,
		DeviceID:        a.cfg.DeviceID,
		Key:             a.cfg.Key,
		AgentVersion:    a.version,
		Platform:        agentPlatform,
		Hardware:        a.hw,
		Cameras:         ProtocolCameras(a.cameras),
	}
}

// Run blocks until ctx is cancelled or an update asks for a restart.
func (a *Agent) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.runCtx, a.restart = ctx, cancel
	a.mu.Unlock()
	a.updater.confirmAfter(ctx, StableAfter)
	a.cameras = Scan(a.disc, a.cfg)
	a.log.Info("agent starting", "device", a.cfg.DeviceID, "model", a.hw.Model,
		"hw_encoder", a.hw.HWH264Encoder, "hw_jpeg_decoder", a.hw.HWMJPEGDecoder, "cameras", len(a.cameras))

	if u, err := url.Parse(a.cfg.ServerURL); err == nil && u.Scheme == "ws" {
		a.log.Warn("server_url uses ws://; ingest TLS trust is anchored in the control channel, use wss:// to be protected against a man in the middle")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.rescanLoop(ctx)
	}()
	a.client.Run(ctx)
	wg.Wait()
	a.sup.StopAll()
}

func (a *Agent) rescanLoop(ctx context.Context) {
	t := time.NewTicker(RescanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		next := Scan(a.disc, a.cfg)
		a.mu.Lock()
		changed := !camerasEqual(a.cameras, next)
		if changed {
			a.cameras = next
		}
		a.mu.Unlock()
		if changed {
			a.log.Info("camera set changed", "cameras", len(next))
			if err := a.client.Send(protocol.TypeCameras, protocol.Cameras{Cameras: ProtocolCameras(next)}); err != nil {
				a.log.Debug("cameras update not sent", "err", err)
			}
		}
	}
}

func (a *Agent) findCamera(id string) (Discovered, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.cameras {
		if c.ID == id {
			return c, true
		}
	}
	return Discovered{}, false
}

func (a *Agent) send(msgType string, msg any) error {
	if a.sendFn != nil {
		return a.sendFn(msgType, msg)
	}
	return a.client.Send(msgType, msg)
}

// OnStart implements Handler.
func (a *Agent) OnStart(s protocol.Start, ingestFP string) {
	fail := func(msg string) {
		a.log.Error("cannot start stream", "camera", s.CameraID, "err", msg)
		_ = a.send(protocol.TypeStreamStatus, protocol.StreamStatus{
			CameraID: s.CameraID, SessionID: s.SessionID, State: protocol.StateFailed, Error: msg,
		})
	}
	if a.cfg.RequireIngestTLS && ingestFP == "" {
		fail("require_ingest_tls is set but the server did not provide an ingest certificate fingerprint; refusing to stream unencrypted")
		return
	}
	cam, ok := a.findCamera(s.CameraID)
	if !ok {
		fail("unknown camera " + s.CameraID)
		return
	}
	hints := Hints{MaxUploadKbps: a.cfg.MaxUploadKbps, NativeKbps: a.cfg.NativeKbps(cam.ID)}
	if a.cfg.ConfiguredNativeKbps(cam.ID) == 0 && cam.XUBitrateBps > 0 {
		hints.NativeKbps = cam.XUBitrateBps / 1000 // measured default of the camera
	}
	hints.NativeBitrateControl = cam.NativeBitrateControl
	hints.PreferredMode = a.cfg.Cameras[cam.ID].PreferredMode
	sel, err := SelectMode(cam, s.Profile, a.hw, hints)
	if err != nil {
		fail(err.Error())
		return
	}
	var prepare func() error
	if sel.CameraBitrateKbps > 0 {
		apply := func() error { return a.applyCameraBitrate(cam, sel.CameraBitrateKbps) }
		if err := apply(); err != nil {
			// Cannot control the camera: fall back to selection without it
			// (mjpeg-hwenc if the budget needs it, else the native rate).
			a.log.Warn("native bitrate control failed; falling back", "camera", cam.ID, "err", err)
			hints.NativeBitrateControl = false
			sel, err = SelectMode(cam, s.Profile, a.hw, hints)
			if err != nil {
				fail(err.Error())
				return
			}
		} else {
			prepare = apply
		}
	}
	pp := PipelineParams{
		Mode: sel.Mode, H264Node: cam.Nodes["h264"], MJPEGNode: cam.Nodes["mjpeg"],
		Width: sel.Width, Height: sel.Height, FPS: sel.FPS, BitrateKbps: sel.BitrateKbps,
		SWDecode:    UseSWDecode(sel.Width, sel.Height),
		AudioDevice: sel.AudioDevice,
	}
	if pp.AudioDevice != "" {
		UnmuteMic(pp.AudioDevice)
		pp.AudioRate = CaptureRate(a.disc.ProcAsound, pp.AudioDevice)
	}
	args, err := BuildGstArgs(pp)
	if err != nil {
		fail(err.Error())
		return
	}
	plan := Plan{Start: s, Mode: sel.Mode, Args: args, IngestCertSHA256: ingestFP, Prepare: prepare}
	if sel.Mode == protocol.ModeMJPEGHWEnc && !pp.SWDecode {
		// The HW JPEG decoder can stall silently on corrupt frames; the
		// supervisor switches to software decoding if that happens.
		pp.SWDecode = true
		plan.FallbackArgs, _ = BuildGstArgs(pp)
	}
	a.log.Info("starting stream", "camera", cam.ID, "session", s.SessionID, "mode", sel.Mode,
		"size", fmt.Sprintf("%dx%d", sel.Width, sel.Height), "fps", sel.FPS, "kbps", sel.BitrateKbps, "camera_kbps", sel.CameraBitrateKbps,
		"swdecode", pp.SWDecode && plan.FallbackArgs == nil, "audio", pp.AudioDevice, "audio_rate", pp.AudioRate)
	a.sup.Start(plan)
}

// OnWelcome implements WelcomeHandler: act on an update offer of the server.
func (a *Agent) OnWelcome(w protocol.Welcome) {
	if w.AgentUpdate == nil {
		return
	}
	if !a.cfg.AutoUpdateEnabled() {
		a.log.Info("server offers an agent update but auto_update is off", "version", w.AgentUpdate.Version)
		return
	}
	if a.version == "dev" || a.version == "" {
		return // development builds are never replaced
	}
	a.mu.Lock()
	ctx, restart := a.runCtx, a.restart
	a.mu.Unlock()
	if ctx == nil {
		return
	}
	a.log.Info("server offers an agent update", "from", a.version, "to", w.AgentUpdate.Version)
	go a.updater.run(ctx, *w.AgentUpdate, func() bool { return a.sup.Active() == 0 }, restart)
}

// OnStop implements Handler.
func (a *Agent) OnStop(s protocol.Stop) { a.sup.Stop(s.CameraID, s.SessionID) }

// OnDisconnect implements Handler: stop all pipelines.
func (a *Agent) OnDisconnect() { a.sup.StopAll() }

// applyCameraBitrate sets the camera's encoder bitrate via the UVC extension
// unit and logs the read-back value.
func (a *Agent) applyCameraBitrate(cam Discovered, kbps int) error {
	if !cam.NativeBitrateControl {
		return fmt.Errorf("camera %s has no native bitrate control", cam.ID)
	}
	got, err := a.applyBitrate(cam.XUNode, cam.XUUnit, kbps*1000)
	if err != nil {
		return fmt.Errorf("set camera bitrate to %d kbps: %w", kbps, err)
	}
	a.log.Info("camera bitrate applied", "camera", cam.ID, "kbps", kbps, "readback_bps", got)
	return nil
}
