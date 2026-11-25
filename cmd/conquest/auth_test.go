package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/anuragxxd/conquest/internal/config"
)

func TestNewAuthMiddleware_Off(t *testing.T) {
	mw, err := newAuthMiddleware(context.Background(), &config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || mw != nil {
		t.Fatalf("middleware = %v, err = %v; want none", mw != nil, err)
	}
}

func TestNewAuthMiddleware_ServiceInfo(t *testing.T) {
	file := filepath.Join(t.TempDir(), "auth.yaml")
	if err := os.WriteFile(file, []byte("issuers:\n  - url: http://127.0.0.1:1/\n    audiences: [web]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, public := range []bool{true, false} {
		cfg := &config.Config{AuthEnabled: true, AuthServiceInfoPublic: public, AuthConfigFile: file}
		mw, err := newAuthMiddleware(context.Background(), cfg, log)
		if err != nil {
			t.Fatal(err)
		}
		h := mw(ok)
		for path, wantOpen := range map[string]bool{
			"/ga4gh/wes/v1/service-info": public,
			"/ga4gh/tes/v1/service-info": public,
			"/ga4gh/tes/service-info":    public,
			"/ga4gh/wes/v1/runs":         false,
		} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if got := w.Code == http.StatusOK; got != wantOpen {
				t.Errorf("public=%v %s: status %d", public, path, w.Code)
			}
		}
	}
}

func TestNewAuthMiddleware_BadConfigFails(t *testing.T) {
	cfg := &config.Config{AuthEnabled: true, AuthConfigFile: filepath.Join(t.TempDir(), "missing.yaml")}
	if _, err := newAuthMiddleware(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("started without an auth config")
	}
}
