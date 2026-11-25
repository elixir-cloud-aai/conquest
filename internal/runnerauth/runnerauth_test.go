package runnerauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

var testSecret = []byte(strings.Repeat("s", MinSecretBytes))

func newTokens(t *testing.T) *Tokens {
	t.Helper()
	tokens, err := New(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

func TestNewRefusesShortSecrets(t *testing.T) {
	if _, err := New(testSecret[:MinSecretBytes-1]); err == nil {
		t.Fatal("31-byte secret accepted")
	}
	if _, err := New(nil); err == nil {
		t.Fatal("empty secret accepted")
	}
}

func TestTokenIsPerRunAndPerSecret(t *testing.T) {
	a := newTokens(t)
	other, err := New([]byte(strings.Repeat("o", MinSecretBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if a.Token("run-1") != a.Token("run-1") {
		t.Fatal("token not deterministic")
	}
	if a.Token("run-1") == a.Token("run-2") {
		t.Fatal("two runs share a token")
	}
	if a.Token("run-1") == other.Token("run-1") {
		t.Fatal("two secrets give the same token")
	}
	if len(a.Token("run-1")) != 64 {
		t.Fatalf("token length %d, want 64 hex digits", len(a.Token("run-1")))
	}
}

func TestValid(t *testing.T) {
	tokens := newTokens(t)
	tok := tokens.Token("run-1")
	cases := []struct {
		name, runID, token string
		want               bool
	}{
		{"own run", "run-1", tok, true},
		{"other run", "run-2", tok, false},
		{"upper-case hex", "run-1", strings.ToUpper(tok), true},
		{"truncated", "run-1", tok[:63], false},
		{"not hex", "run-1", "zz" + tok[2:], false},
		{"empty", "run-1", "", false},
	}
	for _, c := range cases {
		if got := tokens.Valid(c.runID, c.token); got != c.want {
			t.Errorf("%s: Valid = %v, want %v", c.name, got, c.want)
		}
	}
}

// serve sends one request through the middleware on a route like the
// callback API's.
func serve(t *testing.T, lookup RunLookup, path string, header http.Header) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	r := chi.NewRouter()
	r.Route("/internal/runs/{run_id}", func(r chi.Router) {
		r.Use(newTokens(t).Middleware(lookup))
		r.Get("/ga4gh/tes/v1/tasks/{id}", func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
	})
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec, reached
}

func TestMiddleware(t *testing.T) {
	tokens := newTokens(t)
	active := func(context.Context, string) (bool, error) { return false, nil }
	terminal := func(context.Context, string) (bool, error) { return true, nil }
	missing := func(context.Context, string) (bool, error) { return false, ErrRunNotFound }
	failing := func(context.Context, string) (bool, error) { return false, errors.New("db down") }
	const path = "/internal/runs/run-1/ga4gh/tes/v1/tasks/t1"
	auth := func(v ...string) http.Header { return http.Header{"Authorization": v} }

	cases := []struct {
		name    string
		lookup  RunLookup
		header  http.Header
		status  int
		reached bool
	}{
		{"own token", active, auth("Bearer " + tokens.Token("run-1")), 200, true},
		{"lower-case scheme", active, auth("bearer " + tokens.Token("run-1")), 200, true},
		{"no header", active, nil, 401, false},
		{"other run's token", active, auth("Bearer " + tokens.Token("run-2")), 401, false},
		{"user JWT", active, auth("Bearer eyJhbGciOiJSUzI1NiJ9.e30.sig"), 401, false},
		{"basic auth", active, auth("Basic dXNlcjpwYXNz"), 401, false},
		{"empty bearer", active, auth("Bearer "), 401, false},
		{"two headers", active, auth("Bearer "+tokens.Token("run-1"), "Bearer "+tokens.Token("run-1")), 401, false},
		{"terminal run", terminal, auth("Bearer " + tokens.Token("run-1")), 401, false},
		{"missing run", missing, auth("Bearer " + tokens.Token("run-1")), 404, false},
		{"lookup error", failing, auth("Bearer " + tokens.Token("run-1")), 500, false},
		// the run isn't looked up before the token is checked
		{"bad token, missing run", missing, auth("Bearer x"), 401, false},
	}
	for _, c := range cases {
		rec, reached := serve(t, c.lookup, path, c.header)
		if rec.Code != c.status || reached != c.reached {
			t.Errorf("%s: status %d, reached %v; want %d, %v", c.name, rec.Code, reached, c.status, c.reached)
		}
		if rec.Code == 401 && rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: 401 without WWW-Authenticate", c.name)
		}
	}
}
