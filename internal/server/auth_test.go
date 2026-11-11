package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"birdcam/internal/fakeagent"
	"birdcam/internal/protocol"
	"birdcam/internal/server/auth/authtest"
	"birdcam/internal/server/config"
)

const testSessionKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

// authEnv: dev1 is restricted to group "family" except its camera "pubcam"
// (public); dev2's "secretcam" is restricted to "admins".
func newAuthEnv(t *testing.T, prov *authtest.Provider, mod func(*config.Config)) *testEnv {
	t.Helper()
	e := newTestEnvWith(t, 4, true, func(c *config.Config) {
		c.Auth.OIDC = config.OIDC{Issuer: prov.URL, ClientID: prov.ClientID, ClientSecret: prov.ClientSecret}
		c.Auth.SessionKey = testSessionKey
		c.Devices[0].Groups = []string{"family"}
		c.Cameras = append(c.Cameras,
			config.CameraOverride{DeviceID: "dev1", CameraID: "pubcam", Public: true},
			config.CameraOverride{DeviceID: "dev2", CameraID: "secretcam", Groups: []string{"admins"}})
		if mod != nil {
			mod(c)
		}
	})
	waitFor(t, "oidc discovery", 5*time.Second, e.s.auth.Ready)
	a, _, _ := e.runAgent(fakeagent.Config{NoStream: true, Cameras: []protocol.Camera{
		{ID: "fakecam", Name: "Private"}, {ID: "pubcam", Name: "Public"}}})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	b, _, _ := e.runAgent(fakeagent.Config{NoStream: true, DeviceID: "dev2", Key: "key2",
		Cameras: []protocol.Camera{{ID: "secretcam"}}})
	waitEvent(t, b, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "cameras", 2*time.Second, func() bool { return len(e.s.Cameras()) == 3 })
	return e
}

// browser is an HTTP client with a cookie jar that follows redirects except
// those back to the server's own non-/auth/ pages.
func newBrowser(t *testing.T, base string) *http.Client {
	jar, _ := cookiejar.New(nil)
	bu, _ := url.Parse(base)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Host == bu.Host && !strings.HasPrefix(req.URL.Path, "/auth/") {
				return http.ErrUseLastResponse
			}
			return nil
		}}
}

func getJSON(t *testing.T, c *http.Client, u string, v any) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func cameraIDs(cams []CameraInfo) []string {
	var ids []string
	for _, c := range cams {
		ids = append(ids, c.DeviceID+"/"+c.CameraID)
	}
	slices.Sort(ids)
	return ids
}

func whepStatus(t *testing.T, c *http.Client, base, dev, cam string) int {
	t.Helper()
	resp, err := c.Post(base+"/api/cameras/"+dev+"/"+cam+"/whep", "application/sdp", strings.NewReader("garbage"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest && !strings.Contains(string(body), `"error"`) {
		t.Errorf("error body is not JSON: %s", body)
	}
	return resp.StatusCode
}

func login(t *testing.T, c *http.Client, base, ret string) *http.Response {
	t.Helper()
	resp, err := c.Get(base + "/auth/login?return=" + url.QueryEscape(ret))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestOIDCLoginFlow(t *testing.T) {
	for _, tc := range []struct {
		name string
		post bool // client_secret_post + claims only from userinfo
	}{{"basic", false}, {"post+userinfo", true}} {
		t.Run(tc.name, func(t *testing.T) {
			prov := authtest.New("birdcam", "s3cret", authtest.User{Subject: "uid-1", Username: "cat", Name: "Cat",
				Email: "cat@example.com", Groups: []string{"family", "users"}})
			defer prov.Close()
			if tc.post {
				prov.RequireBasic = false
				prov.GroupsInUserinfoOnly = true
			}
			e := newAuthEnv(t, prov, func(c *config.Config) {
				if tc.post {
					c.Auth.OIDC.TokenAuthMethod = config.ClientSecretPost
				}
			})
			base := e.hs.URL
			br := newBrowser(t, base)

			// Anonymous.
			var si SessionInfo
			getJSON(t, br, base+"/api/session", &si)
			if !si.AuthEnabled || si.User != nil || si.LoginURL != "/auth/login" || si.LogoutURL != "/auth/logout" {
				t.Fatalf("anonymous session: %+v", si)
			}
			var cams []CameraInfo
			getJSON(t, br, base+"/api/cameras", &cams)
			if got := cameraIDs(cams); !slices.Equal(got, []string{"dev1/pubcam"}) || !cams[0].Public {
				t.Fatalf("anonymous cameras: %v %+v", got, cams)
			}
			for _, w := range []struct {
				dev, cam string
				want     int
			}{
				{"dev1", "fakecam", 401},
				{"dev2", "secretcam", 401},
				{"dev1", "nope", 404},
				{"nodev", "x", 404},
				{"dev1", "hiddencam", 404},
				{"dev1", "pubcam", 400}, // allowed; garbage SDP fails negotiation
			} {
				if got := whepStatus(t, br, base, w.dev, w.cam); got != w.want {
					t.Errorf("anonymous WHEP %s/%s: %d, want %d", w.dev, w.cam, got, w.want)
				}
			}

			// Login.
			resp := login(t, br, base, "/?device=dev1&camera=fakecam")
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?device=dev1&camera=fakecam" {
				t.Fatalf("callback: %d Location=%q", resp.StatusCode, resp.Header.Get("Location"))
			}
			getJSON(t, br, base+"/api/session", &si)
			if si.User == nil || si.User.Username != "cat" || si.User.Name != "Cat" || si.User.Email != "cat@example.com" ||
				!slices.Equal(si.User.Groups, []string{"family", "users"}) {
				t.Fatalf("logged-in session: %+v", si.User)
			}
			getJSON(t, br, base+"/api/cameras", &cams)
			if got := cameraIDs(cams); !slices.Equal(got, []string{"dev1/fakecam", "dev1/pubcam"}) {
				t.Fatalf("family cameras: %v", got)
			}
			for _, c := range cams {
				if c.CameraID == "fakecam" && c.Public {
					t.Error("fakecam must not be public")
				}
			}
			if got := whepStatus(t, br, base, "dev1", "fakecam"); got != 400 {
				t.Errorf("member WHEP fakecam: %d, want 400 (access granted)", got)
			}
			if got := whepStatus(t, br, base, "dev2", "secretcam"); got != 404 {
				t.Errorf("non-member WHEP secretcam: %d, want 404", got)
			}

			// Logout via fetch: JSON with the end-session URL.
			req, _ := http.NewRequest(http.MethodPost, base+"/auth/logout", nil)
			resp, err := br.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var lo struct{ Redirect string }
			json.NewDecoder(resp.Body).Decode(&lo)
			resp.Body.Close()
			ru, _ := url.Parse(lo.Redirect)
			if !strings.HasPrefix(lo.Redirect, prov.URL+"/logout?") || ru.Query().Get("id_token_hint") == "" ||
				ru.Query().Get("post_logout_redirect_uri") != base+"/" || ru.Query().Get("client_id") != "birdcam" {
				t.Fatalf("logout redirect: %q", lo.Redirect)
			}
			getJSON(t, br, base+"/api/session", &si)
			if si.User != nil {
				t.Fatalf("still logged in after logout: %+v", si.User)
			}
			getJSON(t, br, base+"/api/cameras", &cams)
			if len(cams) != 1 {
				t.Fatalf("cameras after logout: %v", cameraIDs(cams))
			}

			// Log in as an admin; form-post logout redirects.
			prov.SetUser(authtest.User{Subject: "uid-2", Username: "admin", Groups: []string{"admins"}})
			login(t, br, base, "/")
			getJSON(t, br, base+"/api/cameras", &cams)
			if got := cameraIDs(cams); !slices.Equal(got, []string{"dev1/pubcam", "dev2/secretcam"}) {
				t.Fatalf("admin cameras: %v", got)
			}
			if got := whepStatus(t, br, base, "dev1", "fakecam"); got != 404 {
				t.Errorf("admin WHEP fakecam: %d, want 404", got)
			}
			resp, err = br.PostForm(base+"/auth/logout", url.Values{})
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			// The browser followed the 303 to the provider's end-session endpoint.
			if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Request.URL.String(), prov.URL+"/logout?") {
				t.Fatalf("form logout: %d ended at %s", resp.StatusCode, resp.Request.URL)
			}
			getJSON(t, br, base+"/api/session", &si)
			if si.User != nil {
				t.Fatal("still logged in after form logout")
			}
		})
	}
}

func TestLoginReturnPathAndState(t *testing.T) {
	prov := authtest.New("birdcam", "s3cret", authtest.User{Subject: "u", Username: "u", Groups: []string{"family"}})
	defer prov.Close()
	e := newAuthEnv(t, prov, nil)
	base := e.hs.URL
	for ret, want := range map[string]string{
		"//evil.example/x":      "/",
		"https://evil.example/": "/",
		"/\\evil.example":       "/",
		"evil":                  "/",
		"":                      "/",
		"/ok?camera=a":          "/ok?camera=a",
	} {
		br := newBrowser(t, base)
		resp := login(t, br, base, ret)
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != want {
			t.Errorf("return %q: %d %q, want %q", ret, resp.StatusCode, loc, want)
		}
	}

	// A callback with a forged state is rejected.
	br := newBrowser(t, base)
	br.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := br.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	authURL, _ := url.Parse(resp.Header.Get("Location"))
	if authURL.Query().Get("code_challenge_method") != "S256" || authURL.Query().Get("nonce") == "" {
		t.Fatalf("authorize URL lacks PKCE/nonce: %s", authURL)
	}
	resp, err = br.Get(base + "/auth/callback?code=x&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state: %d", resp.StatusCode)
	}
	// Without the login cookie the callback fails too.
	resp, err = http.Get(base + "/auth/callback?code=x&state=y")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without cookie: %d", resp.StatusCode)
	}

	// A tampered session cookie is ignored.
	br = newBrowser(t, base)
	login(t, br, base, "/")
	bu, _ := url.Parse(base)
	cks := br.Jar.Cookies(bu)
	if len(cks) != 1 {
		t.Fatalf("cookies: %v", cks)
	}
	v := []byte(cks[0].Value)
	if v[len(v)/2] == 'A' {
		v[len(v)/2] = 'B'
	} else {
		v[len(v)/2] = 'A'
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/api/session", nil)
	req.AddCookie(&http.Cookie{Name: cks[0].Name, Value: string(v)})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var si SessionInfo
	json.NewDecoder(resp.Body).Decode(&si)
	resp.Body.Close()
	if si.User != nil {
		t.Fatal("tampered cookie accepted")
	}
}

func TestAuthDisabledHidesPrivateCameras(t *testing.T) {
	e := newTestEnvWith(t, 2, true, func(c *config.Config) {
		c.Devices[0].Groups = []string{"family"}
		c.Cameras = append(c.Cameras, config.CameraOverride{DeviceID: "dev1", CameraID: "pubcam", Public: true})
	})
	a, _, _ := e.runAgent(fakeagent.Config{NoStream: true, Cameras: []protocol.Camera{{ID: "fakecam"}, {ID: "pubcam"}}})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "cameras", 2*time.Second, func() bool { return len(e.s.Cameras()) == 2 })
	var cams []CameraInfo
	getJSON(t, http.DefaultClient, e.hs.URL+"/api/cameras", &cams)
	if got := cameraIDs(cams); !slices.Equal(got, []string{"dev1/pubcam"}) {
		t.Fatalf("cameras: %v", got)
	}
	if got := whepStatus(t, http.DefaultClient, e.hs.URL, "dev1", "fakecam"); got != 404 {
		t.Fatalf("WHEP private camera without auth: %d", got)
	}
	var si SessionInfo
	getJSON(t, http.DefaultClient, e.hs.URL+"/api/session", &si)
	if si.AuthEnabled || si.User != nil || si.LoginURL != "" {
		t.Fatalf("session: %+v", si)
	}
	if resp, _ := http.Get(e.hs.URL + "/auth/login"); resp != nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusFound {
			t.Fatal("login must not be available when auth is disabled")
		}
	}
}
