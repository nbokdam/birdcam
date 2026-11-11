package server

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"birdcam/internal/server/auth"
	"birdcam/web"
)

// CameraInfo is one entry of GET /api/cameras.
type CameraInfo struct {
	DeviceID   string       `json:"device_id"`
	DeviceName string       `json:"device_name"`
	CameraID   string       `json:"camera_id"`
	Name       string       `json:"name"`
	Online     bool         `json:"online"`
	Streaming  bool         `json:"streaming"`
	Viewers    int          `json:"viewers"`
	Mode       string       `json:"mode,omitempty"`
	State      string       `json:"state"`
	Error      string       `json:"error,omitempty"`
	Audio      bool         `json:"audio"`
	Rotation   int          `json:"rotation"`
	Public     bool         `json:"public"`
	Stats      *StreamStats `json:"stats,omitempty"`
}

// StreamStats describes the current ingest session.
type StreamStats struct {
	SessionID    string   `json:"session_id"`
	Port         int      `json:"port"`
	Kbps         float64  `json:"kbps"`
	VideoPackets int64    `json:"video_packets"`
	AudioPackets int64    `json:"audio_packets"`
	UptimeS      float64  `json:"uptime_s"`
	AVDriftMS    *float64 `json:"av_drift_ms,omitempty"`
	AgentState   string   `json:"agent_state,omitempty"`
}

// States reported in CameraInfo.State.
const (
	StateOffline   = "offline"   // agent or camera not connected
	StateIdle      = "idle"      // online, nobody watching
	StateStarting  = "starting"  // session started, waiting for media
	StateStreaming = "streaming" // media flowing
	StateRetrying  = "retrying"  // waiting to restart after a failure
)

// Cameras returns all non-hidden cameras, ignoring access control. For
// internal use and tests; viewer-facing code must use CamerasFor.
func (s *Server) Cameras() []CameraInfo {
	return s.cameraList(func(c *camera) bool { return !s.hidden(c) })
}

// CamerasFor returns the cameras user (nil = anonymous) may see.
func (s *Server) CamerasFor(user *auth.User) []CameraInfo {
	return s.cameraList(func(c *camera) bool { return s.accessFor(c, user) == accessAllowed })
}

func (s *Server) cameraList(include func(*camera) bool) []CameraInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []CameraInfo{}
	for _, c := range s.sortedCameras() {
		if !include(c) {
			continue
		}
		online := c.dev.online() && c.present
		ci := CameraInfo{
			DeviceID:   c.dev.cfg.ID,
			DeviceName: c.dev.cfg.Name,
			CameraID:   c.id,
			Name:       s.cameraName(c),
			Online:     online,
			Viewers:    len(c.viewers),
			Mode:       c.mode,
			Error:      c.lastError,
			Audio:      c.info.AudioDevice != "",
			Rotation:   s.rotation(c),
			Public:     s.cfg.CameraAccess(c.dev.cfg.ID, c.id) == nil,
		}
		sess := c.session
		ci.Streaming = s.streamingLocked(sess)
		switch {
		case !online:
			ci.State = StateOffline
		case ci.Streaming:
			ci.State = StateStreaming
		case sess != nil:
			ci.State = StateStarting
		case c.retry != nil:
			ci.State = StateRetrying
		default:
			ci.State = StateIdle
		}
		if sess != nil {
			st := &StreamStats{
				SessionID:    sess.id,
				Port:         sess.port,
				Kbps:         math.Round(sess.kbps*10) / 10,
				VideoPackets: sess.videoPackets.Load(),
				AudioPackets: sess.audioPackets.Load(),
				AgentState:   c.agentState,
			}
			if first := sess.firstRTP.Load(); first != 0 {
				st.UptimeS = math.Round(time.Since(time.Unix(0, first)).Seconds())
			}
			if d, ok := sess.drift.Load().AV(); ok {
				ms := math.Round(d * 1000)
				st.AVDriftMS = &ms
			}
			ci.Stats = st
		}
		out = append(out, ci)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends {"error": msg}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleCameras(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Cookie")
	writeJSON(w, http.StatusOK, s.CamerasFor(s.auth.User(r)))
}

// SessionInfo is the response of GET /api/session.
type SessionInfo struct {
	AuthEnabled     bool       `json:"auth_enabled"`
	User            *auth.User `json:"user"`
	LoginURL        string     `json:"login_url"`
	LogoutURL       string     `json:"logout_url"`
	DefaultDeviceID string     `json:"default_device_id"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	info := SessionInfo{AuthEnabled: s.auth != nil, DefaultDeviceID: s.cfg.HostDefaultDevice(s.cfg.RequestHost(r))}
	if s.auth != nil {
		info.User = s.auth.User(r)
		info.LoginURL, info.LogoutURL = auth.LoginPath, auth.LogoutPath
	}
	w.Header().Add("Vary", "Cookie")
	writeJSON(w, http.StatusOK, info)
}

func staticHandler() http.Handler {
	fsrv := http.FileServerFS(web.Files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fsrv.ServeHTTP(w, r)
	})
}
