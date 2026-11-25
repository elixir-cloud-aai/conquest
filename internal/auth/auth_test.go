package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/anuragxxd/conquest/internal/ownership"
)

const (
	webClient     = "web-frontend"
	serviceClient = "service-client"
)

// testIdP is an OpenID Connect issuer serving discovery and a key set.
type testIdP struct {
	srv       *httptest.Server
	rsaKey    *rsa.PrivateKey
	ecKey     *ecdsa.PrivateKey
	discovery string // issuer named by the discovery document; "" = own URL
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	idp := &testIdP{rsaKey: rsaKey, ecKey: ecKey}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := idp.discovery
		if iss == "" {
			iss = idp.issuer()
		}
		json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "jwks_uri": idp.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		pad := func(n *big.Int) string { return b64(n.FillBytes(make([]byte, 32))) }
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "kid": "rsa", "use": "sig", "alg": "RS256",
				"n": b64(rsaKey.N.Bytes()), "e": b64(big.NewInt(int64(rsaKey.E)).Bytes())},
			{"kty": "EC", "kid": "ec", "use": "sig", "alg": "ES256", "crv": "P-256",
				"x": pad(ecKey.X), "y": pad(ecKey.Y)},
		}})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

// issuer is the issuer identifier, with a trailing slash like LS Login's.
func (i *testIdP) issuer() string { return i.srv.URL + "/" }

// claimsFor returns valid user token claims.
func (i *testIdP) claimsFor(sub string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":       i.issuer(),
		"sub":       sub,
		"aud":       []string{webClient},
		"exp":       now.Add(time.Hour).Unix(),
		"iat":       now.Unix(),
		"client_id": webClient,
		"scope":     "openid",
	}
}

// sign signs claims with the IdP's RSA key as an at+jwt token.
func (i *testIdP) sign(t *testing.T, claims jwt.MapClaims, edit ...func(*jwt.Token)) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["typ"] = "at+jwt"
	tok.Header["kid"] = "rsa"
	var key any = i.rsaKey
	for _, e := range edit {
		e(tok)
	}
	switch tok.Method.(type) {
	case *jwt.SigningMethodECDSA:
		key = i.ecKey
	case *jwt.SigningMethodHMAC:
		key = []byte("shared-secret-shared-secret-1234")
	}
	if tok.Method == jwt.SigningMethodNone {
		key = jwt.UnsafeAllowNoneSignatureType
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestAuthenticator(t *testing.T, idp *testIdP, edit ...func(*Config)) *Authenticator {
	t.Helper()
	cfg := &Config{Issuers: []IssuerConfig{{
		URL:            idp.issuer(),
		Audiences:      []string{webClient, "cli"},
		ServiceClients: []ServiceClient{{ClientID: serviceClient}},
	}}}
	for _, e := range edit {
		e(cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, err := New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func request(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/ga4gh/wes/v1/runs", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAuthenticate_UserToken(t *testing.T) {
	idp := newTestIdP(t)
	a := newTestAuthenticator(t, idp)

	p, err := a.Authenticate(request(idp.sign(t, idp.claimsFor("alice@ls"))))
	if err != nil {
		t.Fatal(err)
	}
	if p.Owner() != idp.issuer()+"|alice@ls" || p.Service || p.ClientID != webClient || p.Scope != "openid" {
		t.Fatalf("principal = %+v", p)
	}
}

func TestAuthenticate_ES256AndSecondAudience(t *testing.T) {
	idp := newTestIdP(t)
	a := newTestAuthenticator(t, idp)
	c := idp.claimsFor("alice@ls")
	c["aud"] = "cli" // a single string audience
	token := idp.sign(t, c, func(tok *jwt.Token) {
		tok.Method = jwt.SigningMethodES256
		tok.Header["alg"] = "ES256"
		tok.Header["kid"] = "ec"
	})
	if _, err := a.Authenticate(request(token)); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticate_ServiceClient(t *testing.T) {
	idp := newTestIdP(t)
	a := newTestAuthenticator(t, idp)
	c := idp.claimsFor(serviceClient)
	c["client_id"] = serviceClient
	c["aud"] = serviceClient

	p, err := a.Authenticate(request(idp.sign(t, c)))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Service || p.Owner() != idp.issuer()+"|"+serviceClient {
		t.Fatalf("principal = %+v", p)
	}
}

func TestAuthenticate_Refused(t *testing.T) {
	idp := newTestIdP(t)
	other := newTestIdP(t)
	a := newTestAuthenticator(t, idp)
	now := time.Now()

	set := func(k string, v any) func(jwt.MapClaims) { return func(c jwt.MapClaims) { c[k] = v } }
	del := func(k string) func(jwt.MapClaims) { return func(c jwt.MapClaims) { delete(c, k) } }
	header := func(k string, v any) func(*jwt.Token) { return func(tok *jwt.Token) { tok.Header[k] = v } }
	method := func(m jwt.SigningMethod) func(*jwt.Token) {
		return func(tok *jwt.Token) { tok.Method = m; tok.Header["alg"] = m.Alg() }
	}
	service := func(c jwt.MapClaims) { c["sub"] = serviceClient; c["client_id"] = serviceClient }

	tests := []struct {
		name   string
		claims []func(jwt.MapClaims)
		header []func(*jwt.Token)
		signer *testIdP
		raw    string // used instead of a signed token when set
	}{
		{name: "wrong issuer", claims: []func(jwt.MapClaims){set("iss", "https://evil.example/")}},
		{name: "issuer without trailing slash", claims: []func(jwt.MapClaims){set("iss", strings.TrimSuffix(idp.issuer(), "/"))}},
		{name: "signed by another key", signer: other},
		{name: "alg none", header: []func(*jwt.Token){method(jwt.SigningMethodNone)}},
		{name: "alg HS256", header: []func(*jwt.Token){method(jwt.SigningMethodHS256)}},
		{name: "alg not allowed", header: []func(*jwt.Token){method(jwt.SigningMethodRS512)}},
		{name: "unknown kid", header: []func(*jwt.Token){header("kid", "nope")}},
		{name: "typ JWT (ID token)", header: []func(*jwt.Token){header("typ", "JWT")}},
		{name: "typ missing", header: []func(*jwt.Token){func(tok *jwt.Token) { delete(tok.Header, "typ") }}},
		{name: "expired", claims: []func(jwt.MapClaims){set("exp", now.Add(-2*time.Minute).Unix())}},
		{name: "no exp", claims: []func(jwt.MapClaims){del("exp")}},
		{name: "nbf in the future", claims: []func(jwt.MapClaims){set("nbf", now.Add(5*time.Minute).Unix())}},
		{name: "iat in the future", claims: []func(jwt.MapClaims){set("iat", now.Add(5*time.Minute).Unix())}},
		{name: "no sub", claims: []func(jwt.MapClaims){del("sub")}},
		{name: "user aud not configured", claims: []func(jwt.MapClaims){set("aud", []string{"other-frontend"})}},
		{name: "user without aud", claims: []func(jwt.MapClaims){del("aud")}},
		{name: "listed client, aud is a frontend", claims: []func(jwt.MapClaims){service, set("aud", webClient)}},
		{name: "listed client, aud has extra entries", claims: []func(jwt.MapClaims){service, set("aud", []string{serviceClient, webClient})}},
		{name: "user token whose sub is a listed client", claims: []func(jwt.MapClaims){set("sub", serviceClient)}},
		{name: "unlisted client with own aud", claims: []func(jwt.MapClaims){set("sub", "x"), set("client_id", "x"), set("aud", "x")}},
		{name: "not a JWS", raw: "not-a-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := tt.raw
			if token == "" {
				signer := tt.signer
				if signer == nil {
					signer = idp
				}
				c := idp.claimsFor("alice@ls")
				for _, e := range tt.claims {
					e(c)
				}
				token = signer.sign(t, c, tt.header...)
			}
			p, err := a.Authenticate(request(token))
			if err == nil {
				t.Fatalf("accepted as %+v", p)
			}
			t.Log(err)
		})
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name    string
		headers []string
		ok      bool
	}{
		{"none", nil, false},
		{"bearer", []string{"Bearer abc"}, true},
		{"lower-case scheme", []string{"bearer abc"}, true},
		{"basic", []string{"Basic abc"}, false},
		{"empty token", []string{"Bearer "}, false},
		{"two tokens", []string{"Bearer a b"}, false},
		{"two headers", []string{"Bearer a", "Bearer b"}, false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, h := range tt.headers {
			r.Header.Add("Authorization", h)
		}
		if _, err := bearerToken(r); (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok = %v", tt.name, err, tt.ok)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/?access_token=abc", nil)
	if _, err := bearerToken(r); err == nil {
		t.Error("token in query parameter accepted")
	}
}

func TestAuthenticate_ConfiguredJWKSURISkipsDiscovery(t *testing.T) {
	idp := newTestIdP(t)
	idp.discovery = "https://evil.example/" // would fail discovery
	a := newTestAuthenticator(t, idp, func(c *Config) { c.Issuers[0].JWKSURI = idp.srv.URL + "/jwks" })
	if _, err := a.Authenticate(request(idp.sign(t, idp.claimsFor("alice@ls")))); err != nil {
		t.Fatal(err)
	}
}

func TestMiddleware(t *testing.T) {
	idp := newTestIdP(t)
	a := newTestAuthenticator(t, idp)
	a.Public = func(r *http.Request) bool { return r.URL.Path == "/public" }

	var gotOwner ownership.Owner
	var gotScoped bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOwner, gotScoped = ownership.FromContext(r.Context())
		if p, ok := PrincipalFromContext(r.Context()); ok != gotScoped || (ok && p.Owner() != gotOwner.ID) {
			t.Errorf("principal %+v does not match owner %+v", p, gotOwner)
		}
		w.WriteHeader(http.StatusTeapot)
	})
	h := a.Middleware(next)

	serve := func(r *http.Request) *httptest.ResponseRecorder {
		gotOwner, gotScoped = ownership.Owner{}, false
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := serve(request(idp.sign(t, idp.claimsFor("alice@ls"))))
	if w.Code != http.StatusTeapot || !gotScoped || gotOwner.ID != idp.issuer()+"|alice@ls" {
		t.Fatalf("valid token: status %d, owner %+v", w.Code, gotOwner)
	}
	if string(gotOwner.Context) != `{"client_id":"web-frontend","scope":"openid"}` {
		t.Fatalf("auth context = %s", gotOwner.Context)
	}

	w = serve(request(""))
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("no token: status %d, WWW-Authenticate %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}

	w = serve(request("not-a-token"))
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != `Bearer error="invalid_token"` {
		t.Fatalf("bad token: status %d, WWW-Authenticate %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	if strings.Contains(w.Body.String(), "not-a-token") {
		t.Fatal("response echoes the token")
	}

	// Public paths pass without a token and ignore one that is sent.
	r := httptest.NewRequest(http.MethodGet, "/public", nil)
	r.Header.Set("Authorization", "Bearer not-a-token")
	if w = serve(r); w.Code != http.StatusTeapot || gotScoped {
		t.Fatalf("public path: status %d, scoped %v", w.Code, gotScoped)
	}
}

func TestMiddleware_DiscoveryFailureIs503(t *testing.T) {
	idp := newTestIdP(t)
	idp.discovery = "https://evil.example/" // names another issuer
	a := newTestAuthenticator(t, idp)
	w := httptest.NewRecorder()
	a.Middleware(http.NotFoundHandler()).ServeHTTP(w, request(idp.sign(t, idp.claimsFor("alice@ls"))))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}
