// Package runnerauth authenticates workflow runners on the runner callback
// API (/internal/runs/{run_id}/...).
//
// Every run gets its own runner token, HMAC-SHA256(secret, "runner:" +
// run ID), handed to the runner Job as the environment variable EnvToken.
// The engines send it as "Authorization: Bearer <token>" on every callback;
// the middleware accepts a callback only with the token of the run named in
// its path, and only while that run is not terminal. A runner, or workflow
// code running next to it, can therefore act on its own run only. Tokens
// need no storage: they are derived again on every request.
package runnerauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// EnvToken is the environment variable that carries a run's runner token
// into its runner pod.
const EnvToken = "CONQUEST_RUNNER_TOKEN"

// MinSecretBytes is the minimum length of the signing secret.
const MinSecretBytes = 32

// ErrRunNotFound is returned by a RunLookup for a run that doesn't exist.
var ErrRunNotFound = errors.New("run not found")

// RunLookup reports whether a run is terminal; ErrRunNotFound if it
// doesn't exist.
type RunLookup func(ctx context.Context, runID string) (terminal bool, err error)

// Tokens derives and checks runner tokens.
type Tokens struct {
	secret []byte
}

// New returns Tokens signing with secret (at least MinSecretBytes).
func New(secret []byte) (*Tokens, error) {
	if len(secret) < MinSecretBytes {
		return nil, errors.New("runner secret must be at least 32 bytes")
	}
	return &Tokens{secret: append([]byte(nil), secret...)}, nil
}

// Token returns the runner token of a run.
func (t *Tokens) Token(runID string) string {
	return hex.EncodeToString(t.mac(runID))
}

// Valid reports, in constant time, whether token is the runner token of
// runID.
func (t *Tokens) Valid(runID, token string) bool {
	got, err := hex.DecodeString(token)
	if err != nil {
		return false
	}
	return hmac.Equal(got, t.mac(runID))
}

func (t *Tokens) mac(runID string) []byte {
	m := hmac.New(sha256.New, t.secret)
	m.Write([]byte("runner:" + runID))
	return m.Sum(nil)
}

// Middleware refuses callbacks without the runner token of the run in the
// path (chi URL parameter "run_id"; mount it below a route that defines
// it) with 401, callbacks for a missing run with 404 and callbacks for a
// terminal run with 401: a finished run's token is spent.
func (t *Tokens) Middleware(runs RunLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			runID := chi.URLParam(r, "run_id")
			token, ok := bearer(r)
			if !ok || runID == "" || !t.Valid(runID, token) {
				unauthorized(w, "a valid runner token for this run is required")
				return
			}
			terminal, err := runs(r.Context(), runID)
			switch {
			case errors.Is(err, ErrRunNotFound):
				writeError(w, http.StatusNotFound, "run not found")
				return
			case err != nil:
				writeError(w, http.StatusInternalServerError, "look up run: "+err.Error())
				return
			case terminal:
				unauthorized(w, "run is in a terminal state")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearer returns the token of an "Authorization: Bearer" header.
func bearer(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	writeError(w, http.StatusUnauthorized, msg)
}

// writeError answers in the TES error shape the callback API uses.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"msg": msg, "status_code": status})
}
