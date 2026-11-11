package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"birdcam/internal/server/config"
	"birdcam/internal/server/media"
)

// The camera sends H.264 Main profile (4d401f). Chrome/Safari offer a Main
// profile entry; Firefox only offers Constrained Baseline, but its decoder
// handles Main streams fine, so we fall back to whatever H.264 the browser
// offers (pion falls back to a mime-type match when binding the track).
var h264Variants = []struct {
	pt   webrtc.PayloadType
	fmtp string
}{
	{102, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f"},
	{106, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
	{108, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"},
	{112, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f"},
}

var (
	videoCap = webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: h264Variants[0].fmtp,
	}
	audioCap = webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeOpus,
		ClockRate:   48000,
		Channels:    2,
		SDPFmtpLine: "minptime=10;useinbandfec=1",
	}
)

type rtcEngine struct {
	api        *webrtc.API
	mux        io.Closer
	iceServers []webrtc.ICEServer
}

func newRTCEngine(cfg config.WebRTC, log *slog.Logger) (*rtcEngine, error) {
	m := &webrtc.MediaEngine{}
	for _, v := range h264Variants {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: v.fmtp},
			PayloadType:        v.pt,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: audioCap, PayloadType: 111},
		webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}

	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetIncludeLoopbackCandidate(cfg.IncludeLoopback)

	var iceServers []webrtc.ICEServer
	var mux io.Closer
	if len(cfg.STUNServers) > 0 {
		// Server-reflexive candidates through a mux need the universal mux,
		// which works on a single socket bound to all addresses.
		conn, err := listenUDP(cfg.UDPPort)
		if err != nil {
			return nil, err
		}
		um := ice.NewUniversalUDPMuxDefault(ice.UniversalUDPMuxParams{UDPConn: conn})
		se.SetICEUDPMux(um)
		mux = um
		iceServers = []webrtc.ICEServer{{URLs: cfg.STUNServers}}
	} else {
		var opts []ice.UDPMuxFromPortOption
		if cfg.IncludeLoopback {
			opts = append(opts, ice.UDPMuxFromPortWithLoopback())
		}
		um, err := ice.NewMultiUDPMuxFromPort(cfg.UDPPort, opts...)
		if err != nil {
			return nil, fmt.Errorf("udp mux on port %d: %w", cfg.UDPPort, err)
		}
		se.SetICEUDPMux(um)
		mux = um
	}
	if len(cfg.PublicIPs) > 0 {
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        cfg.PublicIPs,
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteAppend,
		}); err != nil {
			mux.Close()
			return nil, err
		}
	}
	log.Info("webrtc ready", "udp_port", cfg.UDPPort, "public_ips", cfg.PublicIPs, "stun", cfg.STUNServers)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	return &rtcEngine{api: api, mux: mux, iceServers: iceServers}, nil
}

func (e *rtcEngine) close() { e.mux.Close() }

// viewer is one browser PeerConnection watching a camera.
type viewer struct {
	id  string
	s   *Server
	cam *camera // set by addViewerLocked, guarded by Server.mu
	pc  *webrtc.PeerConnection
	mv  *media.Viewer
	log *slog.Logger

	mu          sync.Mutex
	discTimer   *time.Timer
	connectT    *time.Timer
	closeOnce   sync.Once
	closed      bool
	attachedHub *media.Hub
}

// close removes the viewer: detaches from the hub, drops demand and closes the
// PeerConnection. Safe to call multiple times and from pion callbacks.
func (v *viewer) close() {
	v.closeOnce.Do(func() {
		v.mu.Lock()
		v.closed = true
		if v.discTimer != nil {
			v.discTimer.Stop()
		}
		if v.connectT != nil {
			v.connectT.Stop()
		}
		if v.attachedHub != nil {
			v.attachedHub.Remove(v.mv)
		}
		v.mu.Unlock()
		v.s.mu.Lock()
		v.s.removeViewerLocked(v)
		delete(v.s.viewers, v.id)
		v.s.mu.Unlock()
		v.log.Info("viewer left")
		if v.pc != nil {
			go v.pc.Close()
		}
	})
}

func (v *viewer) onState(st webrtc.PeerConnectionState) {
	v.log.Debug("viewer connection state", "state", st.String())
	switch st {
	case webrtc.PeerConnectionStateConnected:
		v.mu.Lock()
		if v.discTimer != nil {
			v.discTimer.Stop()
			v.discTimer = nil
		}
		if v.connectT != nil {
			v.connectT.Stop()
			v.connectT = nil
		}
		if v.attachedHub == nil && !v.closed {
			v.s.mu.Lock()
			hub := v.cam.hub
			v.s.mu.Unlock()
			hub.Add(v.mv)
			v.attachedHub = hub
			v.log.Info("viewer connected")
		}
		v.mu.Unlock()
	case webrtc.PeerConnectionStateDisconnected:
		v.mu.Lock()
		if v.discTimer == nil {
			v.discTimer = time.AfterFunc(v.s.timing.ViewerGrace, v.close)
		}
		v.mu.Unlock()
	case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
		v.close()
	}
}

func (s *Server) handleWHEP(w http.ResponseWriter, r *http.Request) {
	if s.rtc == nil {
		writeError(w, http.StatusServiceUnavailable, "webrtc disabled")
		return
	}
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/sdp" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/sdp")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		return
	}
	devID, camID := r.PathValue("device"), r.PathValue("camera")
	v := &viewer{id: randHex(16), s: s}
	v.log = s.log.With("viewer", v.id, "device", devID, "camera", camID, "remote", r.RemoteAddr)

	user := s.auth.User(r)
	s.mu.Lock()
	acc := accessDenied
	if c := s.lookupCameraLocked(devID, camID); c != nil {
		acc = s.accessFor(c, user)
	}
	if acc == accessAllowed {
		_, err = s.addViewerLocked(v, devID, camID)
		if err == nil {
			s.viewers[v.id] = v
		}
	}
	s.mu.Unlock()
	switch {
	case acc == accessLogin:
		writeError(w, http.StatusUnauthorized, "login required")
		return
	case acc == accessDenied, errors.Is(err, errUnknownCamera):
		writeError(w, http.StatusNotFound, "unknown camera")
		return
	case errors.Is(err, errNoPort):
		writeError(w, http.StatusServiceUnavailable, "all ingest ports are busy, try again later")
		return
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if user != nil {
		v.log = v.log.With("user", user.Username)
	}

	answer, err := s.negotiate(r.Context(), v, string(body))
	if err != nil {
		v.log.Warn("whep negotiation failed", "err", err)
		v.close()
		writeError(w, http.StatusBadRequest, "negotiation failed: "+err.Error())
		return
	}
	v.log.Info("viewer joined")
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/api/viewers/"+v.id)
	w.WriteHeader(http.StatusCreated)
	io.WriteString(w, answer)
}

func (s *Server) negotiate(ctx context.Context, v *viewer, offer string) (string, error) {
	pc, err := s.rtc.api.NewPeerConnection(webrtc.Configuration{ICEServers: s.rtc.iceServers})
	if err != nil {
		return "", err
	}
	v.pc = pc
	streamID := "birdcam"
	vt, err := webrtc.NewTrackLocalStaticRTP(videoCap, "video", streamID)
	if err != nil {
		return "", err
	}
	at, err := webrtc.NewTrackLocalStaticRTP(audioCap, "audio", streamID)
	if err != nil {
		return "", err
	}
	v.mv = media.NewViewer(vt, at)
	pc.OnConnectionStateChange(v.onState)

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", err
	}
	for _, t := range []webrtc.TrackLocal{vt, at} {
		sender, err := pc.AddTrack(t)
		if err != nil {
			return "", err
		}
		go func() {
			buf := make([]byte, 1500)
			for {
				// Drain RTCP so interceptors (NACK, reports) work. PLIs are
				// ignored: the camera sends an IDR about every second.
				if _, _, err := sender.Read(buf); err != nil {
					return
				}
			}
		}()
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	select {
	case <-gathered:
	case <-time.After(s.timing.ICEGatherLimit):
		return "", errors.New("ICE gathering timed out")
	case <-ctx.Done():
		return "", ctx.Err()
	}
	v.mu.Lock()
	v.connectT = time.AfterFunc(s.timing.ViewerConnect, func() {
		v.log.Info("viewer never connected")
		v.close()
	})
	v.mu.Unlock()
	return pc.LocalDescription().SDP, nil
}

func (s *Server) handleViewerDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	v := s.viewers[r.PathValue("id")]
	s.mu.Unlock()
	if v == nil {
		writeError(w, http.StatusNotFound, "no such viewer")
		return
	}
	v.close()
	w.WriteHeader(http.StatusOK)
}

func listenUDP(port int) (*net.UDPConn, error) {
	return net.ListenUDP("udp", &net.UDPAddr{Port: port})
}
