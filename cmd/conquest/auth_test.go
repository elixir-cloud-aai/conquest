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

	"github.com/go-chi/chi/v5"

	"github.com/anuragxxd/conquest/internal/api"
	"github.com/anuragxxd/conquest/internal/api/tes"
	"github.com/anuragxxd/conquest/internal/api/wes"
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
	if err := os.WriteFile(file, []byte("issuers:\n  - url: http://127.0.0.1:1/\n    audiences: [web]\n    service_clients: [{client_id: svc}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, public := range []bool{true, false} {
		cfg := &config.Config{AuthEnabled: true, AuthServiceInfoPublic: public, AuthConfigFile: file}
		mw, err := newAuthMiddleware(context.Background(), cfg, log)
		if err != nil {
			t.Fatal(err)
		}
		// Routed as in api.NewRouterWithAuth: the middleware sits in a
		// group, so it runs after the route has been matched.
		r := chi.NewRouter()
		r.Group(func(r chi.Router) {
			r.Use(mw)
			for pattern := range serviceInfoRoutes {
				r.Get(pattern, ok)
				r.Post(pattern, ok)
			}
			r.Get("/ga4gh/wes/v1/runs/{run_id}", ok)
		})
		for _, tt := range []struct {
			method, path string
			open         bool
		}{
			{http.MethodGet, "/ga4gh/wes/v1/service-info", public},
			{http.MethodGet, "/ga4gh/tes/v1/service-info", public},
			{http.MethodGet, "/ga4gh/tes/service-info", public},
			{http.MethodPost, "/ga4gh/wes/v1/service-info", false},
			{http.MethodGet, "/ga4gh/wes/v1/runs/service-info", false},
			{http.MethodGet, "/ga4gh/wes/v1/runs/r1", false},
		} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
			if got := w.Code == http.StatusOK; got != tt.open {
				t.Errorf("public=%v %s %s: status %d", public, tt.method, tt.path, w.Code)
			}
		}
	}
}

// The submit rule applies to exactly the routes that create runs or tasks:
// WES POST /runs and public TES POST /tasks, as matched by the router.
func TestIsCreate(t *testing.T) {
	ok := func(w http.ResponseWriter, r *http.Request) {
		if isCreate(r) {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
	r := chi.NewRouter()
	// In a group, as in api.NewRouterWithAuth.
	r.Group(func(r chi.Router) {
		r.Post("/ga4gh/wes/v1/runs", ok)
		r.Get("/ga4gh/wes/v1/runs", ok)
		r.Post("/ga4gh/wes/v1/runs/{run_id}/cancel", ok)
		r.Post("/ga4gh/tes/v1/tasks", ok)
		r.Get("/ga4gh/tes/v1/tasks", ok)
		r.Post("/ga4gh/tes/v1/tasks/{id}:cancel", ok)
	})
	for _, tt := range []struct {
		method, path string
		create       bool
	}{
		{http.MethodPost, "/ga4gh/wes/v1/runs", true},
		{http.MethodPost, "/ga4gh/tes/v1/tasks", true},
		{http.MethodGet, "/ga4gh/wes/v1/runs", false},
		{http.MethodGet, "/ga4gh/tes/v1/tasks", false},
		{http.MethodPost, "/ga4gh/wes/v1/runs/r1/cancel", false},
		{http.MethodPost, "/ga4gh/tes/v1/tasks/t1:cancel", false},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
		if got := w.Code == http.StatusCreated; got != tt.create {
			t.Errorf("%s %s: create %v", tt.method, tt.path, got)
		}
	}
}

func TestNewAuthMiddleware_BadConfigFails(t *testing.T) {
	cfg := &config.Config{AuthEnabled: true, AuthConfigFile: filepath.Join(t.TempDir(), "missing.yaml")}
	if _, err := newAuthMiddleware(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("started without an auth config")
	}
}

// Through the real router: the create rule sees exactly the two create
// routes, and the runner callback routes never reach the auth middleware.
func TestIsCreate_Router(t *testing.T) {
	var seen, create bool
	authn := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen, create = true, isCreate(r)
			w.WriteHeader(http.StatusTeapot) // the handlers are never called
		})
	}
	router := api.NewRouterWithAuth(&wes.Handler{}, &tes.Handler{}, authn)
	for _, tt := range []struct {
		method, path  string
		authn, create bool
	}{
		{http.MethodPost, "/ga4gh/wes/v1/runs", true, true},
		{http.MethodPost, "/ga4gh/tes/v1/tasks", true, true},
		{http.MethodGet, "/ga4gh/wes/v1/runs", true, false},
		{http.MethodGet, "/ga4gh/tes/v1/tasks", true, false},
		{http.MethodPost, "/ga4gh/wes/v1/runs/r1/cancel", true, false},
		{http.MethodPost, "/ga4gh/tes/v1/tasks/t1:cancel", true, false},
		{http.MethodPost, "/internal/runs/r1/ga4gh/tes/v1/tasks", false, false},
	} {
		seen, create = false, false
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
		if seen != tt.authn || create != tt.create {
			t.Errorf("%s %s: authn %v, create %v (status %d)", tt.method, tt.path, seen, create, w.Code)
		}
	}
}
