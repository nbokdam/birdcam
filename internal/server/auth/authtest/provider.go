// Package authtest provides a fake OpenID Connect provider for tests.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// User is the identity the provider logs in.
type User struct {
	Subject  string
	Username string
	Name     string
	Email    string
	Groups   []string
}

type grant struct {
	clientID, redirectURI, nonce, challenge string
	user                                    User
}

// Provider is a fake OIDC provider (authorization code flow with PKCE).
// /authorize logs in the current User without any UI.
type Provider struct {
	*httptest.Server
	ClientID     string
	ClientSecret string
	// RequireBasic enforces client_secret_basic (else client_secret_post).
	RequireBasic bool
	// GroupsInUserinfoOnly omits groups/preferred_username from the ID token
	// (like Authelia's default claims policy) so they come from /userinfo.
	GroupsInUserinfoOnly bool

	key *rsa.PrivateKey

	mu      sync.Mutex
	user    User
	grants  map[string]grant
	access  map[string]User
	Logouts []url.Values // end_session requests
}

// New starts a provider.
func New(clientID, clientSecret string, user User) *Provider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	p := &Provider{ClientID: clientID, ClientSecret: clientSecret, RequireBasic: true, key: key, user: user,
		grants: map[string]grant{}, access: map[string]User{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.Logouts = append(p.Logouts, r.URL.Query())
		p.mu.Unlock()
		w.Write([]byte("logged out"))
	})
	p.Server = httptest.NewServer(mux)
	return p
}

// SetUser changes the identity logged in by the next /authorize.
func (p *Provider) SetUser(u User) {
	p.mu.Lock()
	p.user = u
	p.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                p.URL,
		"authorization_endpoint":                p.URL + "/authorize",
		"token_endpoint":                        p.URL + "/token",
		"userinfo_endpoint":                     p.URL + "/userinfo",
		"jwks_uri":                              p.URL + "/jwks",
		"end_session_endpoint":                  p.URL + "/logout",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
}

func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != p.ClientID || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" {
		http.Error(w, "bad authorize request: "+q.Encode(), http.StatusBadRequest)
		return
	}
	code := randString()
	p.mu.Lock()
	p.grants[code] = grant{clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"), nonce: q.Get("nonce"),
		challenge: q.Get("code_challenge"), user: p.user}
	p.mu.Unlock()
	u, _ := url.Parse(q.Get("redirect_uri"))
	rq := u.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if basic != p.RequireBasic || id != p.ClientID || secret != p.ClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "invalid_client"})
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	g, ok := p.grants[code]
	delete(p.grants, code)
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != g.redirectURI ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": p.URL, "aud": p.ClientID, "sub": g.user.Subject, "nonce": g.nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"name": g.user.Name, "email": g.user.Email,
	}
	if !p.GroupsInUserinfoOnly {
		claims["preferred_username"] = g.user.Username
		claims["groups"] = g.user.Groups
	}
	access := randString()
	p.mu.Lock()
	p.access[access] = g.user
	p.mu.Unlock()
	writeJSON(w, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 3600, "id_token": p.sign(claims),
	})
}

func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	tok := r.Header.Get("Authorization")
	p.mu.Lock()
	u, ok := p.access[stripBearer(tok)]
	p.mu.Unlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{"sub": u.Subject, "preferred_username": u.Username, "name": u.Name,
		"email": u.Email, "groups": u.Groups})
}

func stripBearer(s string) string {
	if len(s) > 7 && s[:7] == "Bearer " {
		return s[7:]
	}
	return ""
}

func (p *Provider) sign(claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithHeader("kid", "k1").WithType("JWT"))
	if err != nil {
		panic(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := signer.Sign(payload)
	if err != nil {
		panic(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		panic(err)
	}
	return s
}

func randString() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
