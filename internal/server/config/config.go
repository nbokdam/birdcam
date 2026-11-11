// Package config loads the birdcam-server YAML configuration.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level server configuration.
type Config struct {
	// Listen is the HTTP listen address (web page, /agent, /api).
	Listen string `yaml:"listen"`
	// ExternalURL is the public base URL of the web page (e.g.
	// https://birdcam.example.com). It is the default OIDC redirect base and
	// decides whether cookies are marked Secure.
	ExternalURL string `yaml:"external_url"`
	// StateDir holds generated state (the self-signed ingest certificate).
	StateDir string `yaml:"state_dir"`
	// TrustProxyHeaders honours X-Forwarded-Host for the hosts mapping. Only
	// enable it when the server is reachable exclusively through a proxy.
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
	// Hosts maps request host names to the device shown by default.
	Hosts []Host `yaml:"hosts"`
	// Auth configures viewer login (OIDC). Disabled unless auth.oidc.issuer
	// is set.
	Auth Auth `yaml:"auth"`
	// Ingest configures the TCP ingest port pool.
	Ingest Ingest `yaml:"ingest"`
	// WebRTC configures the media transport to browsers.
	WebRTC WebRTC `yaml:"webrtc"`
	// DefaultProfile is requested from agents unless overridden per camera.
	DefaultProfile Profile `yaml:"default_profile"`
	// Devices are the agents (Pis) allowed to connect.
	Devices []Device `yaml:"devices"`
	// Cameras are optional per-camera overrides.
	Cameras []CameraOverride `yaml:"cameras"`
}

// Host maps a request host name to a default device.
type Host struct {
	Host          string `yaml:"host"`
	DefaultDevice string `yaml:"default_device"`
}

// Auth configures viewer authentication.
type Auth struct {
	OIDC OIDC `yaml:"oidc"`
	// SessionKey is a 32-byte key (hex or base64) encrypting session
	// cookies. SessionKeyFile reads it from a file instead. If both are
	// empty a random key is generated at startup (sessions do not survive
	// restarts).
	SessionKey     string `yaml:"session_key"`
	SessionKeyFile string `yaml:"session_key_file"`
	// SessionTTL is the login session lifetime (default 168h).
	SessionTTL time.Duration `yaml:"session_ttl"`
}

// OIDC configures the OpenID Connect provider (e.g. Authelia).
type OIDC struct {
	Issuer           string `yaml:"issuer"`
	ClientID         string `yaml:"client_id"`
	ClientSecret     string `yaml:"client_secret"`
	ClientSecretFile string `yaml:"client_secret_file"`
	// RedirectURL, if set, is used for every login. If empty, it is
	// "<scheme of ExternalURL>://<request host>/auth/callback" when the
	// request host is ExternalURL's host or listed in Hosts, else
	// ExternalURL + "/auth/callback". (Login cookies are per host, so the
	// callback must reach the host the login started on.)
	RedirectURL string   `yaml:"redirect_url"`
	Scopes      []string `yaml:"scopes"`
	// GroupsClaim is the ID token / userinfo claim holding group names.
	GroupsClaim string `yaml:"groups_claim"`
	// TokenAuthMethod is client_secret_basic (default) or client_secret_post.
	TokenAuthMethod string `yaml:"token_auth_method"`
}

// Token endpoint client authentication methods.
const (
	ClientSecretBasic = "client_secret_basic"
	ClientSecretPost  = "client_secret_post"
)

// Enabled reports whether viewer authentication is configured.
func (a Auth) Enabled() bool { return a.OIDC.Issuer != "" }

// Ingest is the TCP ingest port pool.
type Ingest struct {
	// Bind is an optional address to bind ingest listeners to (default all).
	Bind    string `yaml:"bind"`
	PortMin int    `yaml:"port_min"`
	PortMax int    `yaml:"port_max"`
	// TLS wraps ingest connections in TLS (default true). The certificate's
	// SHA-256 is sent to agents in Welcome, so a self-signed one is fine.
	TLS *bool `yaml:"tls"`
	// CertFile/KeyFile select the ingest certificate. If unset, a
	// self-signed ECDSA certificate is generated and kept in StateDir.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// TLSEnabled reports whether ingest connections use TLS.
func (i Ingest) TLSEnabled() bool { return i.TLS == nil || *i.TLS }

// WebRTC configures ICE.
type WebRTC struct {
	// UDPPort is the single UDP port used for all WebRTC media.
	UDPPort int `yaml:"udp_port"`
	// PublicIPs are advertised as additional host candidates (1:1 NAT), e.g.
	// the router's public IP when UDPPort is port-forwarded to this host.
	PublicIPs []string `yaml:"public_ips"`
	// STUNServers (e.g. "stun:stun.l.google.com:19302") are used to discover
	// the public address automatically. Usually PublicIPs is the better choice.
	STUNServers []string `yaml:"stun_servers"`
	// IncludeLoopback gathers 127.0.0.1 candidates (useful for tests only).
	IncludeLoopback bool `yaml:"include_loopback"`
}

// Profile is the requested stream format.
type Profile struct {
	Width  int   `yaml:"width"`
	Height int   `yaml:"height"`
	FPS    int   `yaml:"fps"`
	Audio  *bool `yaml:"audio"`
}

// Device is one agent allowed to connect.
type Device struct {
	ID  string `yaml:"id"`
	Key string `yaml:"key"`
	// KeyFile reads the key from a file instead (trailing whitespace is
	// ignored). Exactly one of Key and KeyFile must be set.
	KeyFile string `yaml:"key_file"`
	Name    string `yaml:"name"`
	// MaxKbps is the upload budget of the device, shared by its streaming
	// cameras (0 = unlimited).
	MaxKbps int `yaml:"max_kbps"`
	// Groups restricts the device's cameras to members of any of these
	// groups (empty = public). Cameras can override it.
	Groups []string `yaml:"groups"`
	// NightGrayscale is the default night grayscale mode of the device's
	// cameras (off, auto, on); cameras can override it.
	NightGrayscale string `yaml:"night_grayscale"`
}

// CameraOverride overrides settings for a single camera.
type CameraOverride struct {
	DeviceID string `yaml:"device_id"`
	CameraID string `yaml:"camera_id"`
	Name     string `yaml:"name"`
	MaxKbps  int    `yaml:"max_kbps"`
	Width    int    `yaml:"width"`
	Height   int    `yaml:"height"`
	FPS      int    `yaml:"fps"`
	Audio    *bool  `yaml:"audio"`
	Hidden   bool   `yaml:"hidden"`
	// Groups overrides the device's groups for this camera.
	Groups []string `yaml:"groups"`
	// Public makes the camera visible to everyone regardless of groups.
	Public bool `yaml:"public"`
	// Rotation overrides the agent-reported display rotation (0, 90, 180,
	// 270 degrees clockwise).
	Rotation *int `yaml:"rotation"`
	// NightGrayscale overrides the device's night grayscale mode: the
	// viewer shows the video in grayscale always ("on"), never ("off") or
	// when the image looks like an IR image ("auto"). Display-only.
	NightGrayscale string `yaml:"night_grayscale"`
}

// Night grayscale modes.
const (
	NightOff  = "off"
	NightAuto = "auto"
	NightOn   = "on"
)

// ValidNightGrayscale reports whether m is "" (unset) or a known mode.
func ValidNightGrayscale(m string) bool {
	return m == "" || m == NightOff || m == NightAuto || m == NightOn
}

// NightGrayscaleFor returns the effective night grayscale mode of a camera:
// camera override, else device default, else "off".
func (c *Config) NightGrayscaleFor(deviceID, cameraID string) string {
	if o, ok := c.Camera(deviceID, cameraID); ok && o.NightGrayscale != "" {
		return o.NightGrayscale
	}
	if d, ok := c.Device(deviceID); ok && d.NightGrayscale != "" {
		return d.NightGrayscale
	}
	return NightOff
}

// Load reads and validates the config file at path. Relative file paths in
// the config (state_dir, *_file, ingest cert/key) are resolved against the
// directory of the config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(raw, filepath.Dir(path))
}

// Parse parses and validates YAML config bytes, applying defaults. Relative
// paths are resolved against the current directory.
func Parse(raw []byte) (*Config, error) { return parse(raw, "") }

func parse(raw []byte, baseDir string) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.resolvePaths(baseDir)
	if err := c.readKeyFiles(); err != nil {
		return nil, err
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// resolvePaths makes relative file paths relative to baseDir.
func (c *Config) resolvePaths(baseDir string) {
	if baseDir == "" {
		return
	}
	for _, p := range []*string{&c.StateDir, &c.Auth.SessionKeyFile, &c.Auth.OIDC.ClientSecretFile, &c.Ingest.CertFile, &c.Ingest.KeyFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(baseDir, *p)
		}
	}
	for i := range c.Devices {
		if p := &c.Devices[i].KeyFile; *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(baseDir, *p)
		}
	}
}

// readKeyFiles loads devices[].key_file into Key.
func (c *Config) readKeyFiles() error {
	for i := range c.Devices {
		d := &c.Devices[i]
		if d.KeyFile == "" {
			continue
		}
		if d.Key != "" {
			return fmt.Errorf("device %q: set either key or key_file, not both", d.ID)
		}
		b, err := os.ReadFile(d.KeyFile)
		if err != nil {
			return fmt.Errorf("device %q: %w", d.ID, err)
		}
		d.Key = strings.TrimSpace(string(b))
	}
	return nil
}

// ApplyDefaults fills in unset fields.
func (c *Config) ApplyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Ingest.PortMin == 0 && c.Ingest.PortMax == 0 {
		c.Ingest.PortMin, c.Ingest.PortMax = 9001, 9010
	}
	if c.WebRTC.UDPPort == 0 {
		c.WebRTC.UDPPort = 8189
	}
	if c.DefaultProfile.Width == 0 || c.DefaultProfile.Height == 0 {
		c.DefaultProfile.Width, c.DefaultProfile.Height = 1280, 720
	}
	if c.DefaultProfile.FPS == 0 {
		c.DefaultProfile.FPS = 30
	}
	if c.DefaultProfile.Audio == nil {
		t := true
		c.DefaultProfile.Audio = &t
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/birdcam"
	}
	c.ExternalURL = strings.TrimSuffix(c.ExternalURL, "/")
	o := &c.Auth.OIDC
	if len(o.Scopes) == 0 {
		o.Scopes = []string{"openid", "profile", "email", "groups"}
	}
	if o.GroupsClaim == "" {
		o.GroupsClaim = "groups"
	}
	if o.TokenAuthMethod == "" {
		o.TokenAuthMethod = ClientSecretBasic
	}
	if c.Auth.SessionTTL == 0 {
		c.Auth.SessionTTL = 7 * 24 * time.Hour
	}
	for i := range c.Devices {
		if c.Devices[i].Name == "" {
			c.Devices[i].Name = c.Devices[i].ID
		}
	}
}

// Validate checks the config for errors.
func (c *Config) Validate() error {
	var errs []error
	if c.Ingest.PortMin <= 0 || c.Ingest.PortMax > 65535 || c.Ingest.PortMin > c.Ingest.PortMax {
		errs = append(errs, fmt.Errorf("ingest: invalid port range %d-%d", c.Ingest.PortMin, c.Ingest.PortMax))
	} else if c.Ingest.PortMax-c.Ingest.PortMin >= 1000 {
		errs = append(errs, errors.New("ingest: port range larger than 1000 ports"))
	}
	if c.WebRTC.UDPPort < 0 || c.WebRTC.UDPPort > 65535 {
		errs = append(errs, fmt.Errorf("webrtc: invalid udp_port %d", c.WebRTC.UDPPort))
	}
	for _, ip := range c.WebRTC.PublicIPs {
		if net.ParseIP(ip) == nil {
			errs = append(errs, fmt.Errorf("webrtc: invalid public ip %q", ip))
		}
	}
	seen := map[string]bool{}
	for i, d := range c.Devices {
		switch {
		case d.ID == "":
			errs = append(errs, fmt.Errorf("devices[%d]: missing id", i))
		case seen[d.ID]:
			errs = append(errs, fmt.Errorf("devices[%d]: duplicate id %q", i, d.ID))
		case d.Key == "":
			errs = append(errs, fmt.Errorf("device %q: missing key", d.ID))
		}
		seen[d.ID] = true
		if d.MaxKbps < 0 {
			errs = append(errs, fmt.Errorf("device %q: negative max_kbps", d.ID))
		}
		if !ValidNightGrayscale(d.NightGrayscale) {
			errs = append(errs, fmt.Errorf("device %q: night_grayscale must be off, auto or on", d.ID))
		}
	}
	for i, o := range c.Cameras {
		if o.DeviceID == "" || o.CameraID == "" {
			errs = append(errs, fmt.Errorf("cameras[%d]: device_id and camera_id are required", i))
		} else if !seen[o.DeviceID] {
			errs = append(errs, fmt.Errorf("cameras[%d]: unknown device %q", i, o.DeviceID))
		}
		if o.Rotation != nil && !ValidRotation(*o.Rotation) {
			errs = append(errs, fmt.Errorf("cameras[%d]: rotation must be 0, 90, 180 or 270", i))
		}
		if !ValidNightGrayscale(o.NightGrayscale) {
			errs = append(errs, fmt.Errorf("cameras[%d]: night_grayscale must be off, auto or on", i))
		}
	}
	for i, h := range c.Hosts {
		if h.Host == "" {
			errs = append(errs, fmt.Errorf("hosts[%d]: missing host", i))
		}
		if h.DefaultDevice != "" && !seen[h.DefaultDevice] {
			errs = append(errs, fmt.Errorf("hosts[%d]: unknown device %q", i, h.DefaultDevice))
		}
	}
	if c.ExternalURL != "" {
		if u, err := url.Parse(c.ExternalURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("external_url: must be an absolute http(s) URL, got %q", c.ExternalURL))
		}
	}
	if c.Ingest.TLSEnabled() && (c.Ingest.CertFile == "") != (c.Ingest.KeyFile == "") {
		errs = append(errs, errors.New("ingest: cert_file and key_file must be set together"))
	}
	if c.Auth.SessionTTL < 0 {
		errs = append(errs, errors.New("auth: negative session_ttl"))
	}
	if c.Auth.SessionKey != "" && c.Auth.SessionKeyFile != "" {
		errs = append(errs, errors.New("auth: set only one of session_key and session_key_file"))
	}
	if c.Auth.SessionKey != "" {
		if _, err := ParseKey(c.Auth.SessionKey); err != nil {
			errs = append(errs, fmt.Errorf("auth: session_key: %w", err))
		}
	}
	if o := c.Auth.OIDC; c.Auth.Enabled() {
		if o.ClientID == "" {
			errs = append(errs, errors.New("auth.oidc: missing client_id"))
		}
		if o.ClientSecret == "" && o.ClientSecretFile == "" {
			errs = append(errs, errors.New("auth.oidc: missing client_secret or client_secret_file"))
		}
		if o.ClientSecret != "" && o.ClientSecretFile != "" {
			errs = append(errs, errors.New("auth.oidc: set only one of client_secret and client_secret_file"))
		}
		if o.RedirectURL == "" && c.ExternalURL == "" {
			errs = append(errs, errors.New("auth.oidc: set external_url (or auth.oidc.redirect_url)"))
		} else if u, err := url.Parse(o.RedirectURL); o.RedirectURL != "" && (err != nil || !u.IsAbs()) {
			errs = append(errs, fmt.Errorf("auth.oidc: redirect_url must be absolute, got %q", o.RedirectURL))
		}
		if o.TokenAuthMethod != ClientSecretBasic && o.TokenAuthMethod != ClientSecretPost {
			errs = append(errs, fmt.Errorf("auth.oidc: token_auth_method must be %s or %s", ClientSecretBasic, ClientSecretPost))
		}
		if !slices.Contains(o.Scopes, "openid") {
			errs = append(errs, errors.New("auth.oidc: scopes must include openid"))
		}
	}
	return errors.Join(errs...)
}

// Device returns the device with the given id.
func (c *Config) Device(id string) (Device, bool) {
	for _, d := range c.Devices {
		if d.ID == id {
			return d, true
		}
	}
	return Device{}, false
}

// Camera returns the override for a camera, if any.
func (c *Config) Camera(deviceID, cameraID string) (CameraOverride, bool) {
	for _, o := range c.Cameras {
		if o.DeviceID == deviceID && o.CameraID == cameraID {
			return o, true
		}
	}
	return CameraOverride{}, false
}

// Ports returns the ingest pool ports.
func (c *Config) Ports() []int {
	ports := make([]int, 0, c.Ingest.PortMax-c.Ingest.PortMin+1)
	for p := c.Ingest.PortMin; p <= c.Ingest.PortMax; p++ {
		ports = append(ports, p)
	}
	return ports
}

// ValidRotation reports whether r is a supported display rotation.
func ValidRotation(r int) bool { return r == 0 || r == 90 || r == 180 || r == 270 }

// ParseKey decodes a 32-byte key given as hex or (std/url, padded or not)
// base64. Surrounding whitespace is ignored.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("must be 32 bytes, hex or base64 encoded")
}

// RequestHost is the host the client used: the first X-Forwarded-Host entry
// if TrustProxyHeaders is set and the header is present, else Host.
func (c *Config) RequestHost(r *http.Request) string {
	if c.TrustProxyHeaders {
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
			first, _, _ := strings.Cut(fh, ",")
			return strings.TrimSpace(first)
		}
	}
	return r.Host
}

// KnownHost reports whether host (port optional) is ExternalURL's host or
// one of Hosts.
func (c *Config) KnownHost(host string) bool {
	if u, err := url.Parse(c.ExternalURL); err == nil && c.ExternalURL != "" && strings.EqualFold(u.Host, host) {
		return true
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	for _, e := range c.Hosts {
		if strings.EqualFold(strings.TrimSuffix(e.Host, "."), strings.TrimSuffix(h, ".")) {
			return true
		}
	}
	return false
}

// HostDefaultDevice returns the default device for a request host (port
// stripped, case-insensitive), or "".
func (c *Config) HostDefaultDevice(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	for _, h := range c.Hosts {
		if strings.EqualFold(strings.TrimSuffix(h.Host, "."), host) {
			return h.DefaultDevice
		}
	}
	return ""
}

// CameraAccess returns the groups allowed to view a camera; nil means public.
func (c *Config) CameraAccess(deviceID, cameraID string) []string {
	o, ok := c.Camera(deviceID, cameraID)
	if ok && o.Public {
		return nil
	}
	if ok && len(o.Groups) > 0 {
		return o.Groups
	}
	if d, ok := c.Device(deviceID); ok && len(d.Groups) > 0 {
		return d.Groups
	}
	return nil
}
