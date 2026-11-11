// Package auth implements viewer login via OpenID Connect (authorization code
// flow with PKCE, state and nonce) and stateless session cookies encrypted
// with AES-GCM.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"birdcam/internal/server/config"
)

// Paths of the auth endpoints.
const (
	LoginPath    = "/auth/login"
	CallbackPath = "/auth/callback"
	LogoutPath   = "/auth/logout"
)

const (
	purposeSession = "birdcam-session-v1"
	purposeLogin   = "birdcam-login-v1"
	// loginTTL bounds the time between /auth/login and the callback.
	loginTTL = 10 * time.Minute
	// maxCookieValue keeps the session cookie below browser limits (4096
	// bytes including name and attributes). The ID token is dropped if the
	// cookie would be larger.
	maxCookieValue = 3800
)

// User is a logged-in viewer.
type User struct {
	Username string   `json:"username"`
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Groups   []string `json:"groups"`
}

// InAnyGroup reports whether the user is a member of one of groups.
func (u *User) InAnyGroup(groups []string) bool {
	if u == nil {
		return false
	}
	for _, g := range groups {
		if slices.Contains(u.Groups, g) {
			return true
		}
	}
	return false
}

// sessionData is the payload of the session cookie.
type sessionData struct {
	Username string   `json:"u"`
	Name     string   `json:"n,omitempty"`
	Email    string   `json:"e,omitempty"`
	Groups   []string `json:"g,omitempty"`
	Expiry   int64    `json:"x"`
	IDToken  string   `json:"t,omitempty"`
}

// loginData is the payload of the short-lived login state cookie.
type loginData struct {
	Redirect string `json:"d"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
	Expiry   int64  `json:"x"`
}

type provider struct {
	p          *oidc.Provider
	verifier   *oidc.IDTokenVerifier
	oauth      oauth2.Config
	endSession string
}

// Options configure Auth.
type Options struct {
	// HTTPClient is used for requests to the OIDC provider (default
	// http.DefaultClient).
	HTTPClient *http.Client
	// Now overrides the clock (tests).
	Now func() time.Time
	// RetryMin/RetryMax bound the discovery retry backoff.
	RetryMin, RetryMax time.Duration
}

// Auth handles OIDC login and sessions.
type Auth struct {
	cfg          config.OIDC
	full         *config.Config
	scheme       string // of the external URL
	ttl          time.Duration
	log          *slog.Logger
	sealer       *sealer
	secure       bool
	sessionName  string
	loginName    string
	clientSecret string
	client       *http.Client
	now          func() time.Time

	prov atomic.Pointer[provider]
}

// New creates the auth handler and starts OIDC discovery in the background
// (retried until it succeeds or ctx ends). Login answers 503 until then.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger, opts Options) (*Auth, error) {
	if !cfg.Auth.Enabled() {
		return nil, errors.New("auth: no oidc issuer configured")
	}
	a := &Auth{
		cfg:    cfg.Auth.OIDC,
		full:   cfg,
		ttl:    cfg.Auth.SessionTTL,
		log:    log.With("component", "auth"),
		client: opts.HTTPClient,
		now:    opts.Now,
	}
	if a.client == nil {
		a.client = http.DefaultClient
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.ttl <= 0 {
		a.ttl = 7 * 24 * time.Hour
	}

	key, err := loadSessionKey(cfg.Auth, a.log)
	if err != nil {
		return nil, err
	}
	if a.sealer, err = newSealer(key); err != nil {
		return nil, err
	}
	a.clientSecret = a.cfg.ClientSecret
	if a.cfg.ClientSecretFile != "" {
		b, err := os.ReadFile(a.cfg.ClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("auth: client_secret_file: %w", err)
		}
		a.clientSecret = strings.TrimSpace(string(b))
	}

	a.secure = strings.HasPrefix(a.defaultBase(), "https://")
	a.scheme = "http"
	if a.secure {
		a.scheme = "https"
	}
	a.sessionName, a.loginName = "birdcam_session", "birdcam_login"
	if a.secure {
		a.sessionName, a.loginName = "__Host-birdcam_session", "__Secure-birdcam_login"
	}

	rmin, rmax := opts.RetryMin, opts.RetryMax
	if rmin <= 0 {
		rmin = time.Second
	}
	if rmax <= 0 {
		rmax = time.Minute
	}
	go a.discover(ctx, rmin, rmax)
	return a, nil
}

func loadSessionKey(c config.Auth, log *slog.Logger) ([]byte, error) {
	switch {
	case c.SessionKey != "":
		return config.ParseKey(c.SessionKey)
	case c.SessionKeyFile != "":
		b, err := os.ReadFile(c.SessionKeyFile)
		if err != nil {
			return nil, fmt.Errorf("auth: session_key_file: %w", err)
		}
		if len(b) == 32 {
			return b, nil // raw key
		}
		k, err := config.ParseKey(string(b))
		if err != nil {
			return nil, fmt.Errorf("auth: session_key_file %s: %w", c.SessionKeyFile, err)
		}
		return k, nil
	}
	log.Warn("auth: no session_key/session_key_file configured; using a random key, logins are lost on restart")
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

func (a *Auth) clientCtx(ctx context.Context) context.Context {
	ctx = oidc.ClientContext(ctx, a.client)
	return context.WithValue(ctx, oauth2.HTTPClient, a.client)
}

func (a *Auth) discover(ctx context.Context, rmin, rmax time.Duration) {
	delay := rmin
	for {
		dctx, cancel := context.WithTimeout(a.clientCtx(ctx), 15*time.Second)
		p, err := oidc.NewProvider(dctx, a.cfg.Issuer)
		cancel()
		if err == nil {
			a.setProvider(p)
			a.log.Info("oidc provider discovered", "issuer", a.cfg.Issuer)
			return
		}
		a.log.Warn("oidc discovery failed, retrying", "issuer", a.cfg.Issuer, "err", err, "in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, rmax)
	}
}

func (a *Auth) setProvider(p *oidc.Provider) {
	ep := p.Endpoint()
	if a.cfg.TokenAuthMethod == config.ClientSecretPost {
		ep.AuthStyle = oauth2.AuthStyleInParams
	} else {
		ep.AuthStyle = oauth2.AuthStyleInHeader
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = p.Claims(&meta)
	a.prov.Store(&provider{
		p:        p,
		verifier: p.Verifier(&oidc.Config{ClientID: a.cfg.ClientID, Now: a.now}),
		oauth: oauth2.Config{
			ClientID:     a.cfg.ClientID,
			ClientSecret: a.clientSecret,
			Endpoint:     ep,
			Scopes:       a.cfg.Scopes,
		},
		endSession: meta.EndSession,
	})
}

// defaultBase is the configured external origin: external_url, else the
// origin of redirect_url.
func (a *Auth) defaultBase() string {
	if a.full.ExternalURL != "" {
		return a.full.ExternalURL
	}
	if u, err := url.Parse(a.cfg.RedirectURL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return ""
}

// baseFor returns the origin the viewer is using if it is a configured host
// (external_url's host or one of hosts), else the default origin.
func (a *Auth) baseFor(r *http.Request) string {
	if host := a.full.RequestHost(r); host != "" && a.full.KnownHost(host) {
		return a.scheme + "://" + host
	}
	return a.defaultBase()
}

// redirectFor returns the OIDC redirect URI for a login started by r.
func (a *Auth) redirectFor(r *http.Request) string {
	if a.cfg.RedirectURL != "" {
		return a.cfg.RedirectURL
	}
	return a.baseFor(r) + CallbackPath
}

// Ready reports whether OIDC discovery has succeeded.
func (a *Auth) Ready() bool { return a.prov.Load() != nil }

// Register adds the /auth endpoints to mux.
func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+LoginPath, a.handleLogin)
	mux.HandleFunc("GET "+CallbackPath, a.handleCallback)
	mux.HandleFunc("POST "+LogoutPath, a.handleLogout)
}

// User returns the logged-in user of the request, or nil.
func (a *Auth) User(r *http.Request) *User {
	if a == nil {
		return nil
	}
	ck, err := r.Cookie(a.sessionName)
	if err != nil || ck.Value == "" {
		return nil
	}
	var sd sessionData
	if err := a.sealer.open(purposeSession, ck.Value, &sd); err != nil {
		return nil
	}
	if sd.Username == "" || a.now().Unix() >= sd.Expiry {
		return nil
	}
	groups := sd.Groups
	if groups == nil {
		groups = []string{}
	}
	return &User{Username: sd.Username, Name: sd.Name, Email: sd.Email, Groups: groups}
}

// SafeReturnPath returns p if it is a local absolute path ("/x", not "//x"
// or "/\x"), else "/".
func SafeReturnPath(p string) string {
	if p == "" || p[0] != '/' || len(p) > 2048 {
		return "/"
	}
	if len(p) > 1 && (p[1] == '/' || p[1] == '\\') {
		return "/"
	}
	for _, c := range p {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return p
}

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *Auth) setCookie(w http.ResponseWriter, name, path, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true,
		Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	p := a.prov.Load()
	if p == nil {
		http.Error(w, "login provider unavailable, try again shortly", http.StatusServiceUnavailable)
		return
	}
	ld := loginData{
		Redirect: a.redirectFor(r),
		State:    randToken(),
		Nonce:    randToken(),
		Verifier: oauth2.GenerateVerifier(),
		Return:   SafeReturnPath(r.URL.Query().Get("return")),
		Expiry:   a.now().Add(loginTTL).Unix(),
	}
	v, err := a.sealer.seal(purposeLogin, ld)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.setCookie(w, a.loginName, "/auth/", v, loginTTL)
	oc := p.oauth
	oc.RedirectURL = ld.Redirect
	u := oc.AuthCodeURL(ld.State, oidc.Nonce(ld.Nonce), oauth2.S256ChallengeOption(ld.Verifier))
	http.Redirect(w, r, u, http.StatusFound)
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	p := a.prov.Load()
	if p == nil {
		http.Error(w, "login provider unavailable, try again shortly", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	var ld loginData
	ck, err := r.Cookie(a.loginName)
	if err != nil || a.sealer.open(purposeLogin, ck.Value, &ld) != nil || a.now().Unix() >= ld.Expiry {
		http.Error(w, "login session expired or missing; please start the login again", http.StatusBadRequest)
		return
	}
	a.clearCookie(w, a.loginName, "/auth/")
	if e := q.Get("error"); e != "" {
		a.log.Info("login failed at provider", "error", e, "description", q.Get("error_description"))
		http.Error(w, "login failed: "+e, http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(ld.State)) != 1 {
		http.Error(w, "login state mismatch; please start the login again", http.StatusBadRequest)
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(a.clientCtx(r.Context()), 20*time.Second)
	defer cancel()
	oc := p.oauth
	oc.RedirectURL = ld.Redirect
	tok, err := oc.Exchange(ctx, code, oauth2.VerifierOption(ld.Verifier))
	if err != nil {
		a.log.Warn("token exchange failed", "err", err)
		http.Error(w, "login failed: token exchange", http.StatusBadGateway)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		http.Error(w, "login failed: no id_token", http.StatusBadGateway)
		return
	}
	idt, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		a.log.Warn("id token verification failed", "err", err)
		http.Error(w, "login failed: invalid id_token", http.StatusBadGateway)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(ld.Nonce)) != 1 {
		http.Error(w, "login failed: nonce mismatch", http.StatusBadRequest)
		return
	}
	var claims map[string]json.RawMessage
	if err := idt.Claims(&claims); err != nil {
		http.Error(w, "login failed: bad claims", http.StatusBadGateway)
		return
	}
	// Authelia (and others) may only put profile/groups claims into the
	// userinfo response; fetch it when something is missing.
	if _, ok := claims[a.cfg.GroupsClaim]; !ok || claims["preferred_username"] == nil {
		ui, err := p.p.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		if err != nil {
			a.log.Warn("userinfo request failed", "err", err)
		} else if ui.Subject != idt.Subject {
			a.log.Warn("userinfo subject mismatch", "id_token", idt.Subject, "userinfo", ui.Subject)
		} else {
			var extra map[string]json.RawMessage
			if ui.Claims(&extra) == nil {
				for k, v := range extra {
					if _, ok := claims[k]; !ok {
						claims[k] = v
					}
				}
			}
		}
	}
	sd := sessionData{
		Username: firstString(claims, "preferred_username", "sub"),
		Name:     firstString(claims, "name", "preferred_username"),
		Email:    firstString(claims, "email"),
		Groups:   stringList(claims[a.cfg.GroupsClaim]),
		Expiry:   a.now().Add(a.ttl).Unix(),
		IDToken:  rawID,
	}
	if sd.Username == "" {
		sd.Username = idt.Subject
	}
	v, err := a.sealer.seal(purposeSession, sd)
	if err == nil && len(a.sessionName)+len(v) > maxCookieValue {
		sd.IDToken = ""
		v, err = a.sealer.seal(purposeSession, sd)
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.setCookie(w, a.sessionName, "/", v, a.ttl)
	a.log.Info("user logged in", "user", sd.Username, "groups", sd.Groups)
	http.Redirect(w, r, SafeReturnPath(ld.Return), http.StatusSeeOther)
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	redirect := "/"
	if ck, err := r.Cookie(a.sessionName); err == nil {
		var sd sessionData
		if a.sealer.open(purposeSession, ck.Value, &sd) == nil {
			if p := a.prov.Load(); p != nil && p.endSession != "" {
				if u, err := url.Parse(p.endSession); err == nil {
					q := u.Query()
					q.Set("client_id", a.cfg.ClientID)
					q.Set("post_logout_redirect_uri", a.baseFor(r)+"/")
					if sd.IDToken != "" {
						q.Set("id_token_hint", sd.IDToken)
					}
					u.RawQuery = q.Encode()
					redirect = u.String()
				}
			}
			a.log.Info("user logged out", "user", sd.Username)
		}
	}
	a.clearCookie(w, a.sessionName, "/")
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data" {
		http.Redirect(w, r, redirect, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": redirect})
}

func firstString(claims map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		var s string
		if raw, ok := claims[k]; ok && json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// stringList accepts a JSON array of strings or a single string.
func stringList(raw json.RawMessage) []string {
	if raw == nil {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return []string{s}
	}
	return nil
}
