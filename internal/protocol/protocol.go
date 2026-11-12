// Package protocol defines the wire contract between birdcam-agent (on a Pi)
// and birdcam-server. See docs/ARCHITECTURE.md for the full description.
//
// Control channel: one WebSocket per agent (Pi) at ws(s)://SERVER/agent.
// Every message is a JSON object with a "type" field (Envelope). Both sides
// send a Ping every PingInterval; any received message counts as liveness.
// If nothing is received for DeadAfter, the side closes the connection; the
// agent then reconnects with backoff.
//
// Media channel: one TCP connection per streaming camera, to a port from the
// server's ingest pool, on the host of the agent's server_url or the host
// the server announces in Start.IngestHost. The agent sends IngestPreamble, then RFC 4571 framed
// RTP packets (2-byte big-endian length + RTP packet).
package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

const Version = 1

const (
	PingInterval = 10 * time.Second
	DeadAfter    = 30 * time.Second
	// MaxReconnectBackoff caps the agent's reconnect delay.
	MaxReconnectBackoff = 15 * time.Second
	// StopLinger is how long the server keeps a stream running after the
	// last viewer left, before sending Stop.
	StopLinger = 30 * time.Second
)

// RTP payload types and clock rates used on the ingest connection.
const (
	PayloadTypeH264 = 96
	PayloadTypeOpus = 111
	ClockRateH264   = 90000
	ClockRateOpus   = 48000
	// MaxRTPPacketSize: packets are forwarded unchanged into WebRTC, so the
	// agent must payload with mtu <= this (rtph264pay mtu=1200).
	MaxRTPPacketSize = 1200
)

// IngestPreambleFormat is the first line the agent sends on an ingest TCP
// connection: "BIRDCAM1 <session_id> <token>\n".
const IngestPreambleFormat = "BIRDCAM1 %s %s\n"

func IngestPreamble(sessionID, token string) string {
	return fmt.Sprintf(IngestPreambleFormat, sessionID, token)
}

// Message types.
const (
	// agent -> server
	TypeHello        = "hello"
	TypeCameras      = "cameras"
	TypeStreamStatus = "stream_status"
	// server -> agent
	TypeWelcome = "welcome"
	TypeStart   = "start"
	TypeStop    = "stop"
	TypeError   = "error"
	// both directions
	TypePing = "ping"
	TypePong = "pong"
)

// Envelope is used to peek at the type before decoding the full message.
type Envelope struct {
	Type string `json:"type"`
}

// Encode marshals msg with its "type" field set.
func Encode(msgType string, msg any) ([]byte, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	t, _ := json.Marshal(msgType)
	m["type"] = t
	return json.Marshal(m)
}

// ---- agent -> server ----

type Hello struct {
	ProtocolVersion int    `json:"protocol_version"`
	DeviceID        string `json:"device_id"`
	Key             string `json:"key"`
	AgentVersion    string `json:"agent_version"`
	// Platform is "<GOOS>/<GOARCH>" of the agent binary (e.g. "linux/arm64"),
	// so the server can offer a matching update. Empty for old agents.
	Platform string   `json:"platform,omitempty"`
	Hardware Hardware `json:"hardware"`
	Cameras  []Camera `json:"cameras"`
}

type Hardware struct {
	Model          string `json:"model"`            // /proc/device-tree/model
	HWH264Encoder  bool   `json:"hw_h264_encoder"`  // bcm2835-codec-encode found
	HWMJPEGDecoder bool   `json:"hw_mjpeg_decoder"` // bcm2835-codec-decode found
	MaxUploadKbps  int    `json:"max_upload_kbps"`  // agent-side configured budget, 0 = unlimited
}

// Camera is one capture device on the agent. ID is stable across reboots and
// replugs (derived from /dev/v4l/by-id).
type Camera struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	VideoDevice string        `json:"video_device"`
	AudioDevice string        `json:"audio_device,omitempty"` // ALSA "hw:CARD=X,DEV=0", empty if none
	Formats     []VideoFormat `json:"formats"`
	// Rotation is the clockwise display rotation in degrees (0, 90, 180,
	// 270) configured on the agent. Viewers apply it; the video itself is
	// not rotated. The server may override it.
	Rotation int `json:"rotation,omitempty"`
}

type VideoFormat struct {
	Codec  string `json:"codec"` // "h264" | "mjpeg"
	Width  int    `json:"width"`
	Height int    `json:"height"`
	FPS    []int  `json:"fps"`
}

type Cameras struct {
	Cameras []Camera `json:"cameras"`
}

// Stream states reported in StreamStatus.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopped  = "stopped"
	StateFailed   = "failed"
)

// Stream modes.
const (
	ModeH264Passthrough = "h264-passthrough"
	ModeMJPEGHWEnc      = "mjpeg-hwenc"
)

type StreamStatus struct {
	CameraID  string `json:"camera_id"`
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Mode      string `json:"mode,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ---- server -> agent ----

type Welcome struct {
	ServerVersion string `json:"server_version"`
	// IngestCertSHA256, if set, is the lowercase hex SHA-256 of the server's
	// ingest TLS certificate (DER). The agent must then connect to ingest
	// ports with TLS and accept only a leaf certificate with this hash.
	// Trust is anchored in the control connection (use wss://).
	IngestCertSHA256 string `json:"ingest_cert_sha256,omitempty"`
	// AgentUpdate, if set, is a newer (really: different) agent build for the
	// platform the agent announced in Hello. See AgentUpdate.
	AgentUpdate *AgentUpdate `json:"agent_update,omitempty"`
}

// AgentUpdate announces the agent binary the server wants the agent to run.
// The agent downloads it from Path (relative to the server's HTTP base URL,
// authenticated with the device key as bearer token), verifies Size and SHA256
// (announced over the control channel, so trust is anchored there: use
// wss://), checks that the binary reports Version, and restarts into it once
// no stream is running.
type AgentUpdate struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"` // lowercase hex
	Size    int64  `json:"size"`
	Path    string `json:"path"` // e.g. "/agent/binary/linux-arm64"
}

type Start struct {
	CameraID  string  `json:"camera_id"`
	SessionID string  `json:"session_id"`
	Port      int     `json:"port"`
	Token     string  `json:"token"`
	Profile   Profile `json:"profile"`
	// IngestHost, if set, is the host (name or IP) the agent must open the
	// ingest connection to, instead of the host of its server_url. It lets
	// the control channel use a stable load-balanced name while the media
	// goes straight to the node that runs this server. Empty = server_url host.
	IngestHost string `json:"ingest_host,omitempty"`
}

// Profile is the server's request; the agent picks the closest supported
// mode and must not exceed MaxKbps (0 = no limit).
type Profile struct {
	Width   int  `json:"width"`
	Height  int  `json:"height"`
	FPS     int  `json:"fps"`
	MaxKbps int  `json:"max_kbps"`
	Audio   bool `json:"audio"`
}

type Stop struct {
	CameraID  string `json:"camera_id"`
	SessionID string `json:"session_id"`
}

type Error struct {
	Message string `json:"message"`
}

type Ping struct {
	T int64 `json:"t"` // sender unix millis
}

type Pong struct {
	T int64 `json:"t"` // echoed from Ping
}
