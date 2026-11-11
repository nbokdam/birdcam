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
// server's ingest pool. The agent sends IngestPreamble, then RFC 4571 framed
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
	ProtocolVersion int      `json:"protocol_version"`
	DeviceID        string   `json:"device_id"`
	Key             string   `json:"key"`
	AgentVersion    string   `json:"agent_version"`
	Hardware        Hardware `json:"hardware"`
	Cameras         []Camera `json:"cameras"`
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
}

type Start struct {
	CameraID  string  `json:"camera_id"`
	SessionID string  `json:"session_id"`
	Port      int     `json:"port"`
	Token     string  `json:"token"`
	Profile   Profile `json:"profile"`
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
