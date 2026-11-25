package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
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
	mu        sync.Mutex    // guards rsaKey, rsaKid and delay
	rsaKid    string        // kid of rsaKey; "" = "rsa"
	delay     time.Duration // latency of the key set endpoint
	rsaKey    *rsa.PrivateKey
	ecKey     *ecdsa.PrivateKey
	discovery string // issuer named by the discovery document; "" = own URL
	down      bool   // discovery answers 500
	rsaUse    string // "use" of the RSA key; "" = sig

	// userinfo endpoint (create.go)
	userinfo    userinfoStub
	noUserinfo  bool   // discovery names no userinfo_endpoint
	userinfoURL string // userinfo_endpoint named by discovery; "" = own
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
		if idp.down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		iss := idp.discovery
		if iss == "" {
			iss = idp.issuer()
		}
		doc := map[string]string{"issuer": iss, "jwks_uri": idp.srv.URL + "/jwks"}
		switch {
		case idp.userinfoURL != "":
			doc["userinfo_endpoint"] = idp.userinfoURL
		case !idp.noUserinfo:
			doc["userinfo_endpoint"] = idp.srv.URL + "/userinfo"
		}
		json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/userinfo", idp.userinfo.serve)
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		idp.mu.Lock()
		rsaKey, kid, delay := idp.rsaKey, idp.kid(), idp.delay
		idp.mu.Unlock()
		time.Sleep(delay)
		rsaUse := idp.rsaUse
		if rsaUse == "" {
			rsaUse = "sig"
		}
		b64 := base64.RawURLEncoding.EncodeToString
		pad := func(n *big.Int) string { return b64(n.FillBytes(make([]byte, 32))) }
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "kid": kid, "use": rsaUse, "alg": "RS256",
				"n": b64(rsaKey.N.Bytes()), "e": b64(big.NewInt(int64(rsaKey.E)).Bytes())},
			{"kty": "EC", "kid": "ec", "use": "sig", "alg": "ES256", "crv": "P-256",
				"x": pad(ecKey.X), "y": pad(ecKey.Y)},
		}})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

// kid is the RSA key's kid; callers hold i.mu.
func (i *testIdP) kid() string {
	if i.rsaKid == "" {
		return "rsa"
	}
	return i.rsaKid
}

// rotate replaces the RSA signing key by a new one with a new kid.
func (i *testIdP) rotate(t *testing.T) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rsaKey, i.rsaKid = key, i.kid()+"-next"
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
	i.mu.Lock()
	tok.Header["kid"] = i.kid()
	var key any = i.rsaKey
	i.mu.Unlock()
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
	// The HMAC key is a test fixture for the refusal of symmetric tokens,
	// not a credential.
	s, err := tok.SignedString(key) // nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key
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
		want   string // part of the error
	}{
		{name: "wrong issuer", claims: []func(jwt.MapClaims){set("iss", "https://evil.example/")}, want: "untrusted issuer"},
		{name: "issuer without trailing slash", claims: []func(jwt.MapClaims){set("iss", strings.TrimSuffix(idp.issuer(), "/"))}, want: "untrusted issuer"},
		{name: "signed by another key", signer: other, want: "verification error"},
		{name: "alg none", header: []func(*jwt.Token){method(jwt.SigningMethodNone)}, want: "signing method none is invalid"},
		{name: "alg HS256", header: []func(*jwt.Token){method(jwt.SigningMethodHS256)}, want: "signing method HS256 is invalid"},
		{name: "alg not allowed", header: []func(*jwt.Token){method(jwt.SigningMethodRS512)}, want: "signing method RS512 is invalid"},
		{name: "unknown kid", header: []func(*jwt.Token){header("kid", "nope")}, want: "key not found"},
		{name: "typ JWT (ID token)", header: []func(*jwt.Token){header("typ", "JWT")}, want: "is not at+jwt"},
		{name: "typ missing", header: []func(*jwt.Token){func(tok *jwt.Token) { delete(tok.Header, "typ") }}, want: "is not at+jwt"},
		{name: "expired", claims: []func(jwt.MapClaims){set("exp", now.Add(-2*time.Minute).Unix())}, want: "token is expired"},
		{name: "no exp", claims: []func(jwt.MapClaims){del("exp")}, want: "exp claim is required"},
		{name: "no iat", claims: []func(jwt.MapClaims){del("iat")}, want: "token has no iat"},
		{name: "nbf in the future", claims: []func(jwt.MapClaims){set("nbf", now.Add(5*time.Minute).Unix())}, want: "not valid yet"},
		{name: "iat in the future", claims: []func(jwt.MapClaims){set("iat", now.Add(5*time.Minute).Unix())}, want: "used before issued"},
		{name: "no sub", claims: []func(jwt.MapClaims){del("sub")}, want: "no sub"},
		{name: "user aud not configured", claims: []func(jwt.MapClaims){set("aud", []string{"other-frontend"})}, want: "no configured audience"},
		{name: "user without aud", claims: []func(jwt.MapClaims){del("aud")}, want: "no configured audience"},
		{name: "listed client, aud is a frontend", claims: []func(jwt.MapClaims){service, set("aud", webClient)}, want: "is not its own client ID"},
		{name: "listed client, aud has extra entries", claims: []func(jwt.MapClaims){service, set("aud", []string{serviceClient, webClient})}, want: "is not its own client ID"},
		{name: "user token whose sub is a listed client", claims: []func(jwt.MapClaims){set("sub", serviceClient)}, want: "not that client's own token"},
		{name: "unlisted client with own aud", claims: []func(jwt.MapClaims){set("sub", "x"), set("client_id", "x"), set("aud", "x")}, want: "no configured audience"},
		{name: "not a JWS", raw: "not-a-token", want: "malformed token"},
		{name: "no kid", header: []func(*jwt.Token){func(tok *jwt.Token) { delete(tok.Header, "kid") }}, want: "no kid"},
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
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("refused for %q, want %q", err, tt.want)
			}
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

func TestAuthenticate_AccessTokenTypes(t *testing.T) {
	idp := newTestIdP(t)
	a := newTestAuthenticator(t, idp)
	for _, typ := range []string{"at+jwt", "AT+JWT", "application/at+jwt"} {
		token := idp.sign(t, idp.claimsFor("alice@ls"), func(tok *jwt.Token) { tok.Header["typ"] = typ })
		if _, err := a.Authenticate(request(token)); err != nil {
			t.Errorf("typ %q: %v", typ, err)
		}
	}
}

func TestAuthenticate_DiscoveryRetriesAfterFailure(t *testing.T) {
	idp := newTestIdP(t)
	idp.down = true
	a := newTestAuthenticator(t, idp)
	token := idp.sign(t, idp.claimsFor("alice@ls"))

	if _, err := a.Authenticate(request(token)); !errors.Is(err, errUnavailable) {
		t.Fatalf("issuer down: err = %v, want errUnavailable", err)
	}
	// Within the retry delay, the failure is answered from memory.
	idp.down = false
	if _, err := a.Authenticate(request(token)); !errors.Is(err, errUnavailable) {
		t.Fatalf("within retry delay: err = %v, want errUnavailable", err)
	}
	// After it, discovery is tried again and succeeds.
	old := discoveryRetry
	discoveryRetry = 0
	t.Cleanup(func() { discoveryRetry = old })
	if _, err := a.Authenticate(request(token)); err != nil {
		t.Fatalf("issuer back: %v", err)
	}
}

func TestAuthenticate_UnreachableKeySetIsUnavailable(t *testing.T) {
	idp := newTestIdP(t)
	a := newTestAuthenticator(t, idp, func(c *Config) { c.Issuers[0].JWKSURI = "http://127.0.0.1:1/jwks" })
	_, err := a.Authenticate(request(idp.sign(t, idp.claimsFor("alice@ls"))))
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("err = %v, want errUnavailable", err)
	}
}

func TestAuthenticate_SlowIssuerDoesNotQueueRequests(t *testing.T) {
	idp := newTestIdP(t)
	started, release := make(chan struct{}, 1), make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	a := newTestAuthenticator(t, idp, func(c *Config) { c.Issuers[0].JWKSURI = slow.URL + "/jwks" })
	token := idp.sign(t, idp.claimsFor("alice@ls"))

	go a.Authenticate(request(token)) // starts the fetch and hangs on it
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first fetch never started")
	}
	begin := time.Now()
	_, err := a.Authenticate(request(token))
	if !errors.Is(err, errUnavailable) || !strings.Contains(err.Error(), "being fetched") {
		t.Fatalf("err = %v, want errUnavailable while the key set is being fetched", err)
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("waiting request took %v", d)
	}
}

func TestAuthenticate_SkipsEncryptionKeys(t *testing.T) {
	idp := newTestIdP(t)
	idp.rsaUse = "enc"
	a := newTestAuthenticator(t, idp)
	if _, err := a.Authenticate(request(idp.sign(t, idp.claimsFor("alice@ls")))); err == nil {
		t.Fatal("token verified with an encryption key")
	}
}

func TestAuthenticate_KeyRotation(t *testing.T) {
	idp := newTestIdP(t)
	idp.delay = 20 * time.Millisecond // a refetch must not need to be instant
	a := newTestAuthenticator(t, idp)
	if _, err := a.Authenticate(request(idp.sign(t, idp.claimsFor("alice@ls")))); err != nil {
		t.Fatal(err)
	}

	idp.rotate(t)
	if _, err := a.Authenticate(request(idp.sign(t, idp.claimsFor("alice@ls")))); err != nil {
		t.Fatalf("token with the rotated key: %v", err)
	}

	// The refetch is rate-limited: a second unknown kid right after is
	// refused at once rather than waiting for the limiter.
	idp.rotate(t)
	begin := time.Now()
	if _, err := a.Authenticate(request(idp.sign(t, idp.claimsFor("alice@ls")))); err == nil {
		t.Fatal("second rotation within a minute accepted")
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("rate-limited lookup took %v", d)
	}
}

func TestAuthenticate_FailedAttemptsLeaveNoGoroutines(t *testing.T) {
	idp := newTestIdP(t)
	a := newTestAuthenticator(t, idp, func(c *Config) { c.Issuers[0].JWKSURI = "http://127.0.0.1:1/jwks" })
	old := discoveryRetry
	discoveryRetry = 0
	t.Cleanup(func() { discoveryRetry = old })
	token := idp.sign(t, idp.claimsFor("alice@ls"))

	a.Authenticate(request(token)) // warm up
	before := runtime.NumGoroutine()
	for n := 0; n < 50; n++ {
		if _, err := a.Authenticate(request(token)); !errors.Is(err, errUnavailable) {
			t.Fatalf("err = %v, want errUnavailable", err)
		}
	}
	time.Sleep(100 * time.Millisecond) // let cancelled refreshers exit
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutines grew from %d to %d over 50 failed attempts", before, after)
	}
}

func TestKeyfunc_PanicDoesNotStickResolving(t *testing.T) {
	idp := newTestIdP(t)
	a := newTestAuthenticator(t, idp)
	iss := a.issuers[idp.issuer()]
	iss.client = nil // makes resolve panic

	func() {
		defer func() { _ = recover() }()
		iss.keyfunc()
		t.Fatal("resolve did not panic")
	}()
	iss.mu.Lock()
	resolving, lastError := iss.resolving, iss.lastError
	iss.mu.Unlock()
	if resolving || !errors.Is(lastError, errUnavailable) {
		t.Fatalf("after panic: resolving %v, last error %v", resolving, lastError)
	}
}

func TestCheckRedirect(t *testing.T) {
	req := func(u string) *http.Request { return httptest.NewRequest(http.MethodGet, u, nil) }
	if err := checkRedirect(req("https://keys.example/jwks"), nil); err != nil {
		t.Errorf("https redirect refused: %v", err)
	}
	if err := checkRedirect(req("http://127.0.0.1:8080/jwks"), nil); err != nil {
		t.Errorf("loopback http redirect refused: %v", err)
	}
	if err := checkRedirect(req("http://keys.example/jwks"), nil); err == nil {
		t.Error("redirect to plain http accepted")
	}
	if err := checkRedirect(req("https://keys.example/jwks"), make([]*http.Request, 10)); err == nil {
		t.Error("eleventh redirect accepted")
	}
}

func TestAuthenticate_KeySetRedirectToHTTPIsRefused(t *testing.T) {
	idp := newTestIdP(t)
	var hits int
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "http://keys.example.invalid/jwks", http.StatusFound)
	}))
	t.Cleanup(plain.Close)
	a := newTestAuthenticator(t, idp, func(c *Config) { c.Issuers[0].JWKSURI = plain.URL + "/jwks" })
	_, err := a.Authenticate(request(idp.sign(t, idp.claimsFor("alice@ls"))))
	if !errors.Is(err, errUnavailable) || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("err = %v, want errUnavailable from the refused redirect", err)
	}
	if hits == 0 {
		t.Fatal("key set never requested")
	}
}
