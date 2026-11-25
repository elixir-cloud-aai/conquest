package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	entitled    = "urn:example:group:compute#idp.example"
	notEntitled = "urn:example:group:base#idp.example"
)

// userinfoStub is the test IdP's userinfo endpoint.
type userinfoStub struct {
	mu       sync.Mutex
	status   int            // 0 = 200
	delay    time.Duration  // latency
	raw      string         // body verbatim; "" = claims as JSON
	claims   map[string]any // answered claims
	calls    int
	gotToken string // Authorization header of the last call
}

func (u *userinfoStub) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.calls++
	u.gotToken = r.Header.Get("Authorization")
	status, delay, raw, claims := u.status, u.delay, u.raw, u.claims
	u.mu.Unlock()
	time.Sleep(delay)
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if raw != "" {
		w.Write([]byte(raw))
		return
	}
	json.NewEncoder(w).Encode(claims)
}

// set configures the next answers and resets the call count.
func (u *userinfoStub) set(status int, raw string, claims map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status, u.raw, u.claims, u.calls, u.gotToken = status, raw, claims, 0, ""
}

func (u *userinfoStub) stats() (calls int, token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls, u.gotToken
}

// newCreateAuthenticator is an authenticator that requires entitled to
// create and treats every POST as a create.
func newCreateAuthenticator(t *testing.T, idp *testIdP, edit ...func(*Config)) *Authenticator {
	t.Helper()
	edit = append([]func(*Config){func(c *Config) { c.Create.Entitlements = []string{entitled} }}, edit...)
	a := newTestAuthenticator(t, idp, edit...)
	a.Create = func(r *http.Request) bool { return r.Method == http.MethodPost }
	return a
}

func serveThrough(a *Authenticator, method, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/ga4gh/wes/v1/runs", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})).ServeHTTP(w, r)
	return w
}

func userinfoFor(sub string, entitlements any) map[string]any {
	c := map[string]any{"sub": sub}
	if entitlements != nil {
		c["eduperson_entitlement"] = entitlements
	}
	return c
}

// TestMiddleware_Create covers the submit rows of the authorisation
// matrix (gi#18 design, section 3: rows 2-4, 6, 7, 12, 13). 418 stands for
// "passed to the handler".
func TestMiddleware_Create(t *testing.T) {
	idp := newTestIdP(t)
	a := newCreateAuthenticator(t, idp)
	user := func(sub string) string { return idp.sign(t, idp.claimsFor(sub)) }
	service := func() string {
		c := idp.claimsFor(serviceClient)
		c["client_id"], c["aud"] = serviceClient, []string{serviceClient}
		return idp.sign(t, c)
	}
	// A client credentials token of a client that isn't listed, with an
	// audience Conquest accepts: authenticated as a caller, but not a user.
	unlisted := func() string {
		c := idp.claimsFor("other-client")
		c["client_id"] = "other-client"
		return idp.sign(t, c)
	}

	tests := []struct {
		name      string
		method    string
		token     string
		status    int            // userinfo status (0 = 200)
		raw       string         // userinfo body verbatim
		claims    map[string]any // userinfo claims
		want      int
		wantCalls int
	}{
		{"row 2: user without the entitlement", http.MethodPost, user("alice"),
			0, "", userinfoFor("alice", []string{notEntitled}), http.StatusForbidden, 1},
		{"row 2: user without any entitlement", http.MethodPost, user("alice"),
			0, "", userinfoFor("alice", nil), http.StatusForbidden, 1},
		{"row 2: prefix of the entitlement", http.MethodPost, user("alice"),
			0, "", userinfoFor("alice", []string{strings.Split(entitled, "#")[0]}), http.StatusForbidden, 1},
		{"row 2: entitlement with a suffix", http.MethodPost, user("alice"),
			0, "", userinfoFor("alice", []string{entitled + ":sub"}), http.StatusForbidden, 1},
		{"row 2: entitlement claim of another type", http.MethodPost, user("alice"),
			0, "", userinfoFor("alice", 42), http.StatusForbidden, 1},
		{"row 2: user without the entitlement lists", http.MethodGet, user("alice"),
			0, "", userinfoFor("alice", nil), http.StatusTeapot, 0},
		{"row 3: member", http.MethodPost, user("alice"),
			0, "", userinfoFor("alice", []string{notEntitled, entitled}), http.StatusTeapot, 1},
		{"row 3: member, claim as a single string", http.MethodPost, user("alice"),
			0, "", userinfoFor("alice", entitled), http.StatusTeapot, 1},
		{"row 4: former member reads", http.MethodGet, user("alice"),
			0, "", userinfoFor("alice", nil), http.StatusTeapot, 0},
		{"row 6: listed service client creates", http.MethodPost, service(),
			http.StatusForbidden, "", nil, http.StatusTeapot, 0},
		{"row 7: unlisted service client creates", http.MethodPost, unlisted(),
			0, "", userinfoFor("other-client", []string{entitled}), http.StatusForbidden, 0},
		{"row 7: unlisted service client lists", http.MethodGet, unlisted(),
			0, "", nil, http.StatusTeapot, 0},
		{"row 12: userinfo 5xx", http.MethodPost, user("alice"),
			http.StatusBadGateway, "", nil, http.StatusServiceUnavailable, 1},
		{"row 12: userinfo not JSON", http.MethodPost, user("alice"),
			0, "<html>", nil, http.StatusServiceUnavailable, 1},
		{"row 12: userinfo JSON but not an object", http.MethodPost, user("alice"),
			0, `["sub"]`, nil, http.StatusServiceUnavailable, 1},
		{"row 12: userinfo redirect", http.MethodPost, user("alice"),
			http.StatusFound, "", nil, http.StatusServiceUnavailable, 1},
		{"row 13: userinfo 401", http.MethodPost, user("alice"),
			http.StatusUnauthorized, "", nil, http.StatusUnauthorized, 1},
		{"row 13: userinfo 403", http.MethodPost, user("alice"),
			http.StatusForbidden, "", nil, http.StatusUnauthorized, 1},
		{"userinfo names another sub", http.MethodPost, user("alice"),
			0, "", userinfoFor("mallory", []string{entitled}), http.StatusUnauthorized, 1},
		{"userinfo without sub", http.MethodPost, user("alice"),
			0, "", map[string]any{"eduperson_entitlement": []string{entitled}}, http.StatusUnauthorized, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idp.userinfo.set(tt.status, tt.raw, tt.claims)
			w := serveThrough(a, tt.method, tt.token)
			calls, gotToken := idp.userinfo.stats()
			if w.Code != tt.want || calls != tt.wantCalls {
				t.Fatalf("status %d, userinfo calls %d; want %d, %d (body %s)", w.Code, calls, tt.want, tt.wantCalls, w.Body)
			}
			if calls > 0 && gotToken != "Bearer "+tt.token {
				t.Errorf("userinfo got another token than the request's")
			}
			if strings.Contains(w.Body.String(), tt.token) {
				t.Error("response echoes the token")
			}
			if tt.want == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") != `Bearer error="invalid_token"` {
				t.Errorf("WWW-Authenticate = %q", w.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

// TestMiddleware_CreateNoCache: every submit asks the IdP again, so a
// revoked entitlement takes effect on the next submit.
func TestMiddleware_CreateNoCache(t *testing.T) {
	idp := newTestIdP(t)
	a := newCreateAuthenticator(t, idp)
	token := idp.sign(t, idp.claimsFor("alice"))

	idp.userinfo.set(0, "", userinfoFor("alice", []string{entitled}))
	if w := serveThrough(a, http.MethodPost, token); w.Code != http.StatusTeapot {
		t.Fatalf("member: status %d", w.Code)
	}
	idp.userinfo.set(0, "", userinfoFor("alice", nil))
	if w := serveThrough(a, http.MethodPost, token); w.Code != http.StatusForbidden {
		t.Fatalf("revoked: status %d", w.Code)
	}
}

func TestMiddleware_CreateUserinfoTimeoutIs503(t *testing.T) {
	idp := newTestIdP(t)
	one := 1
	a := newCreateAuthenticator(t, idp, func(c *Config) { c.TimeoutSeconds = &one })
	token := idp.sign(t, idp.claimsFor("alice"))
	idp.userinfo.set(0, "", userinfoFor("alice", []string{entitled}))
	if w := serveThrough(a, http.MethodGet, token); w.Code != http.StatusTeapot { // resolves the key set
		t.Fatalf("GET: status %d", w.Code)
	}
	idp.userinfo.mu.Lock()
	idp.userinfo.delay = 1500 * time.Millisecond
	idp.userinfo.mu.Unlock()
	if w := serveThrough(a, http.MethodPost, token); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
}

func TestMiddleware_CreateEntitlementClaim(t *testing.T) {
	idp := newTestIdP(t)
	a := newCreateAuthenticator(t, idp, func(c *Config) { c.Create.EntitlementClaim = "groups" })
	token := idp.sign(t, idp.claimsFor("alice"))

	idp.userinfo.set(0, "", userinfoFor("alice", []string{entitled})) // the default claim
	if w := serveThrough(a, http.MethodPost, token); w.Code != http.StatusForbidden {
		t.Fatalf("default claim: status %d, want 403", w.Code)
	}
	idp.userinfo.set(0, "", map[string]any{"sub": "alice", "groups": []string{entitled}})
	if w := serveThrough(a, http.MethodPost, token); w.Code != http.StatusTeapot {
		t.Fatalf("configured claim: status %d", w.Code)
	}
}

// With no entitlements configured, only listed service clients create,
// and users are refused without asking the IdP.
func TestMiddleware_CreateServiceClientsOnly(t *testing.T) {
	idp := newTestIdP(t)
	a := newTestAuthenticator(t, idp)
	a.Create = func(r *http.Request) bool { return r.Method == http.MethodPost }
	idp.userinfo.set(0, "", userinfoFor("alice", []string{entitled}))
	if w := serveThrough(a, http.MethodPost, idp.sign(t, idp.claimsFor("alice"))); w.Code != http.StatusForbidden {
		t.Fatalf("user: status %d, want 403", w.Code)
	}
	if calls, _ := idp.userinfo.stats(); calls != 0 {
		t.Fatalf("userinfo called %d times", calls)
	}
}

// Without the Create predicate nothing is a create: no request asks the IdP.
func TestMiddleware_NoCreatePredicate(t *testing.T) {
	idp := newTestIdP(t)
	a := newCreateAuthenticator(t, idp)
	a.Create = nil
	idp.userinfo.set(0, "", userinfoFor("alice", nil))
	if w := serveThrough(a, http.MethodPost, idp.sign(t, idp.claimsFor("alice"))); w.Code != http.StatusTeapot {
		t.Fatalf("status %d", w.Code)
	}
	if calls, _ := idp.userinfo.stats(); calls != 0 {
		t.Fatalf("userinfo called %d times", calls)
	}
}

func TestMiddleware_CreateConfiguredUserinfoURI(t *testing.T) {
	idp := newTestIdP(t)
	other := &userinfoStub{}
	srv := httptest.NewServer(http.HandlerFunc(other.serve))
	t.Cleanup(srv.Close)
	a := newCreateAuthenticator(t, idp, func(c *Config) { c.Issuers[0].UserinfoURI = srv.URL })
	other.set(0, "", userinfoFor("alice", []string{entitled}))
	idp.userinfo.set(0, "", userinfoFor("alice", nil))
	if w := serveThrough(a, http.MethodPost, idp.sign(t, idp.claimsFor("alice"))); w.Code != http.StatusTeapot {
		t.Fatalf("status %d", w.Code)
	}
	if calls, _ := other.stats(); calls != 1 {
		t.Fatalf("configured userinfo called %d times", calls)
	}
	if calls, _ := idp.userinfo.stats(); calls != 0 {
		t.Fatalf("discovered userinfo called %d times", calls)
	}
}

// A configured jwks_uri skips discovery, unless the userinfo endpoint has
// to be discovered.
func TestAuthenticate_JWKSURIWithoutUserinfoURIDiscovers(t *testing.T) {
	idp := newTestIdP(t)
	a := newCreateAuthenticator(t, idp, func(c *Config) { c.Issuers[0].JWKSURI = idp.srv.URL + "/jwks" })
	idp.userinfo.set(0, "", userinfoFor("alice", []string{entitled}))
	if w := serveThrough(a, http.MethodPost, idp.sign(t, idp.claimsFor("alice"))); w.Code != http.StatusTeapot {
		t.Fatalf("status %d", w.Code)
	}
}

func TestMiddleware_CreateNoUserinfoEndpointIs503(t *testing.T) {
	idp := newTestIdP(t)
	idp.noUserinfo = true // discovery names no userinfo_endpoint
	a := newCreateAuthenticator(t, idp)
	token := idp.sign(t, idp.claimsFor("alice"))
	if w := serveThrough(a, http.MethodGet, token); w.Code != http.StatusTeapot {
		t.Fatalf("GET: status %d (reads need no userinfo)", w.Code)
	}
	if w := serveThrough(a, http.MethodPost, token); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST: status %d, want 503", w.Code)
	}
}

func TestEntitlements(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want []string
	}{
		{`["a","b"]`, []string{"a", "b"}},
		{`"a"`, []string{"a"}},
		{`42`, nil},
		{`null`, nil},
		{`[1,"a"]`, nil},
		{``, nil},
	} {
		got := entitlements(map[string]json.RawMessage{"e": json.RawMessage(tt.raw)}, "e")
		if strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("%s: got %v, want %v", tt.raw, got, tt.want)
		}
	}
	if got := entitlements(map[string]json.RawMessage{}, "e"); got != nil {
		t.Errorf("missing claim: %v", got)
	}
}
