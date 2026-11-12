// Package agent implements birdcam-agent, the Raspberry Pi side of BirdCam.
package agent

import (
	"bytes"
	"fmt"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultNativeKbps is the assumed bitrate of a camera's built-in H.264
// encoder (it cannot be controlled from the agent).
const DefaultNativeKbps = 7000

// Config is the agent's YAML configuration.
type Config struct {
	ServerURL     string `yaml:"server_url"`
	DeviceID      string `yaml:"device_id"`
	Key           string `yaml:"key"`
	MaxUploadKbps int    `yaml:"max_upload_kbps"` // 0 = unlimited
	GstLaunch     string `yaml:"gst_launch"`
	// RequireIngestTLS refuses to stream unless the server announced an
	// ingest certificate fingerprint (i.e. never fall back to plain TCP).
	RequireIngestTLS bool `yaml:"require_ingest_tls"`
	// StateDir holds downloaded agent updates (StateDir/update). It must be
	// writable by the agent user. Default /var/lib/birdcam-agent.
	StateDir string `yaml:"state_dir"`
	// AutoUpdate (default true) lets the server replace this agent with the
	// build it ships: the agent downloads, verifies and restarts into it once
	// no stream is running. Set false to only update via install-agent.sh.
	AutoUpdate *bool                   `yaml:"auto_update"`
	Cameras    map[string]CameraConfig `yaml:"cameras"` // keyed by camera id
}

// DefaultStateDir is where the agent keeps downloaded updates.
const DefaultStateDir = "/var/lib/birdcam-agent"

// AutoUpdateEnabled reports whether server-pushed updates are applied.
func (c *Config) AutoUpdateEnabled() bool { return c.AutoUpdate == nil || *c.AutoUpdate }

// CameraConfig holds optional per-camera overrides.
type CameraConfig struct {
	Name          string `yaml:"name"`
	Disabled      bool   `yaml:"disabled"`
	AudioDevice   string `yaml:"audio_device"`   // override ALSA device ("none" disables audio)
	PreferredMode string `yaml:"preferred_mode"` // "h264-passthrough" | "mjpeg-hwenc"
	NativeKbps    int    `yaml:"native_kbps"`    // bitrate of the camera's own H.264 encoder
	// NativeBitrateControl set to false disables the use of the camera's
	// native H.264 bitrate control (UVC extension unit); default: use it if detected.
	NativeBitrateControl *bool `yaml:"native_bitrate_control"`
	Rotation             int   `yaml:"rotation"` // clockwise display rotation: 0, 90, 180, 270 (metadata only)
}

// LoadConfig reads and validates a config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(data)
}

// ParseConfig parses and validates YAML config bytes.
func ParseConfig(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.GstLaunch == "" {
		c.GstLaunch = "gst-launch-1.0"
	}
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks required fields.
func (c *Config) Validate() error {
	u, err := url.Parse(c.ServerURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Hostname() == "" {
		return fmt.Errorf("config: server_url must be ws:// or wss:// URL, got %q", c.ServerURL)
	}
	if c.DeviceID == "" {
		return fmt.Errorf("config: device_id is required")
	}
	if c.Key == "" {
		return fmt.Errorf("config: key is required")
	}
	if c.MaxUploadKbps < 0 {
		return fmt.Errorf("config: max_upload_kbps must be >= 0")
	}
	for id, cc := range c.Cameras {
		switch cc.Rotation {
		case 0, 90, 180, 270:
		default:
			return fmt.Errorf("config: camera %q: rotation must be 0, 90, 180 or 270, got %d", id, cc.Rotation)
		}
		switch cc.PreferredMode {
		case "", "h264-passthrough", "mjpeg-hwenc":
		default:
			return fmt.Errorf("config: camera %q: unknown preferred_mode %q", id, cc.PreferredMode)
		}
	}
	return nil
}

// ServerHost returns the hostname of server_url (used for the ingest TCP connection).
func (c *Config) ServerHost() string {
	u, err := url.Parse(c.ServerURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// NativeKbps returns the configured native H.264 bitrate for a camera.
func (c *Config) NativeKbps(cameraID string) int {
	if cc, ok := c.Cameras[cameraID]; ok && cc.NativeKbps > 0 {
		return cc.NativeKbps
	}
	return DefaultNativeKbps
}

// ConfiguredNativeKbps returns native_kbps for a camera, 0 if not configured.
func (c *Config) ConfiguredNativeKbps(cameraID string) int {
	if cc, ok := c.Cameras[cameraID]; ok {
		return max(cc.NativeKbps, 0)
	}
	return 0
}
