package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"birdcam/internal/server/config"
)

func testAuth(t *testing.T, mod func(*config.Config)) *Auth {
	t.Helper()
	cfg := &config.Config{ExternalURL: "https://birdcam.example.com"}
	cfg.Auth.OIDC = config.OIDC{Issuer: "http://127.0.0.1:1", ClientID: "c", ClientSecret: "s"}
	cfg.Auth.SessionKey = strings.Repeat("ab", 32)
	if mod != nil {
		mod(cfg)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, err := New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{RetryMin: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSafeReturnPath(t *testing.T) {
	for in, want := range map[string]string{
		"/":                             "/",
		"/?device=a&camera=b":           "/?device=a&camera=b",
		"/x/y#frag":                     "/x/y#frag",
		"":                              "/",
		"x":                             "/",
		"//evil.com":                    "/",
		"/\\evil.com":                   "/",
		"/a\\b":                         "/",
		"https://evil.com/":             "/",
		"/a\nSet-Cookie: x=y":           "/",
		"javascript:alert(1)":           "/",
		"/" + strings.Repeat("a", 3000): "/",
	} {
		if got := SafeReturnPath(in); got != want {
			t.Errorf("SafeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func sessionRequest(a *Auth, value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	r.AddCookie(&http.Cookie{Name: a.sessionName, Value: value})
	return r
}

func TestSessionCookie(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := testAuth(t, nil)
	a.now = func() time.Time { return now }
	if !a.secure || a.sessionName != "__Host-birdcam_session" {
		t.Fatalf("https external_url must give secure cookies: %v %s", a.secure, a.sessionName)
	}
	v, err := a.sealer.seal(purposeSession, sessionData{Username: "cat", Groups: []string{"g"}, Expiry: now.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if u := a.User(sessionRequest(a, v)); u == nil || u.Username != "cat" || !u.InAnyGroup([]string{"x", "g"}) {
		t.Fatalf("valid session rejected: %+v", u)
	}
	// Tampered.
	b := []byte(v)
	for i := range b {
		orig := b[i]
		b[i] = map[bool]byte{true: 'B', false: 'A'}[orig == 'A']
		if a.User(sessionRequest(a, string(b))) != nil {
			t.Fatalf("tampered cookie (byte %d) accepted", i)
		}
		b[i] = orig
	}
	if a.User(sessionRequest(a, "garbage")) != nil || a.User(sessionRequest(a, "")) != nil {
		t.Fatal("garbage accepted")
	}
	// Expired.
	now = now.Add(2 * time.Hour)
	if a.User(sessionRequest(a, v)) != nil {
		t.Fatal("expired session accepted")
	}
	// A login-state cookie cannot be used as a session cookie.
	lv, _ := a.sealer.seal(purposeLogin, sessionData{Username: "x", Expiry: now.Add(time.Hour).Unix()})
	if a.User(sessionRequest(a, lv)) != nil {
		t.Fatal("cookie sealed for another purpose accepted")
	}
	// A different key does not open it.
	other := testAuth(t, func(c *config.Config) { c.Auth.SessionKey = strings.Repeat("cd", 32) })
	other.now = a.now
	v2, _ := a.sealer.seal(purposeSession, sessionData{Username: "cat", Expiry: now.Add(time.Hour).Unix()})
	if other.User(sessionRequest(other, v2)) != nil {
		t.Fatal("cookie accepted with a different key")
	}
}

func TestLoginUnavailableUntilDiscovery(t *testing.T) {
	a := testAuth(t, nil) // issuer unreachable
	if a.Ready() {
		t.Fatal("ready without provider")
	}
	mux := http.NewServeMux()
	a.Register(mux)
	for _, p := range []string{"/auth/login?return=/", "/auth/callback?code=x&state=y"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d, want 503", p, w.Code)
		}
	}
	// Logout works without a provider and falls back to "/".
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"redirect":"/"`) {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	if sc := w.Header().Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") || !strings.Contains(sc, "Secure") ||
		!strings.Contains(sc, "HttpOnly") || !strings.Contains(sc, "SameSite=Lax") {
		t.Fatalf("logout cookie: %s", sc)
	}
}

func TestSessionKeyFile(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(strings.Repeat("k", 32))
	for name, content := range map[string][]byte{
		"raw": raw, "hex": []byte(strings.Repeat("6b", 32) + "\n"), "b64": []byte("a2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2s=\n"),
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, content, 0o600)
		k, err := loadSessionKey(config.Auth{SessionKeyFile: p}, slog.Default())
		if err != nil || string(k) != string(raw) {
			t.Errorf("%s: %q %v", name, k, err)
		}
	}
	os.WriteFile(filepath.Join(dir, "bad"), []byte("short"), 0o600)
	if _, err := loadSessionKey(config.Auth{SessionKeyFile: filepath.Join(dir, "bad")}, slog.Default()); err == nil {
		t.Error("bad key file accepted")
	}
}

func TestPlainHTTPCookies(t *testing.T) {
	a := testAuth(t, func(c *config.Config) { c.ExternalURL = "http://localhost:8080" })
	r := httptest.NewRequest(http.MethodGet, "http://localhost:8080/auth/login", nil)
	if a.secure || a.sessionName != "birdcam_session" || a.redirectFor(r) != "http://localhost:8080/auth/callback" {
		t.Fatalf("plain http: secure=%v name=%s redirect=%s", a.secure, a.sessionName, a.redirectFor(r))
	}
}

func TestRedirectPerHost(t *testing.T) {
	a := testAuth(t, func(c *config.Config) {
		c.Devices = []config.Device{{ID: "d", Key: "k"}}
		c.Hosts = []config.Host{{Host: "maci.example", DefaultDevice: "d"}}
	})
	for host, want := range map[string]string{
		"birdcam.example.com": "https://birdcam.example.com/auth/callback",
		"MACI.example":        "https://MACI.example/auth/callback",
		"evil.example":        "https://birdcam.example.com/auth/callback",
	} {
		r := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
		r.Host = host
		if got := a.redirectFor(r); got != want {
			t.Errorf("%s: %s, want %s", host, got, want)
		}
	}
	// An explicit redirect_url always wins.
	b := testAuth(t, func(c *config.Config) { c.Auth.OIDC.RedirectURL = "https://login.example/auth/callback" })
	r := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	r.Host = "birdcam.example.com"
	if got := b.redirectFor(r); got != "https://login.example/auth/callback" {
		t.Errorf("explicit: %s", got)
	}
}
