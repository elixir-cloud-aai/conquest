package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/anuragxxd/conquest/internal/api/tes"
	"github.com/anuragxxd/conquest/internal/api/wes"
	"github.com/anuragxxd/conquest/internal/controller"
	"github.com/anuragxxd/conquest/internal/cors"
	"github.com/anuragxxd/conquest/internal/storage"
)

// denyAll stands in for the token middleware.
func denyAll(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
}

func TestNewRouterWithAuth_GuardsOnlyThePublicAPIs(t *testing.T) {
	ctrl := controller.NewRunController(nil, nil, nil, storage.Layout{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	extra := func(r chi.Router) {
		r.Get("/storage/v1/ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	}
	router := NewRouterWithAuth(wes.NewHandler(ctrl), tes.NewHandler(nil, nil, nil, nil), denyAll, extra)

	guarded := []struct{ method, path string }{
		{http.MethodGet, "/ga4gh/wes/v1/service-info"},
		{http.MethodPost, "/ga4gh/wes/v1/runs"},
		{http.MethodGet, "/ga4gh/wes/v1/runs"},
		{http.MethodGet, "/ga4gh/wes/v1/runs/r1"},
		{http.MethodGet, "/ga4gh/wes/v1/runs/r1/status"},
		{http.MethodPost, "/ga4gh/wes/v1/runs/r1/cancel"},
		{http.MethodGet, "/ga4gh/wes/v1/runs/r1/tasks"},
		{http.MethodGet, "/ga4gh/wes/v1/runs/r1/tasks/t1"},
		{http.MethodGet, "/ga4gh/tes/v1/service-info"},
		{http.MethodGet, "/ga4gh/tes/service-info"},
		{http.MethodPost, "/ga4gh/tes/v1/tasks"},
		{http.MethodGet, "/ga4gh/tes/v1/tasks"},
		{http.MethodGet, "/ga4gh/tes/v1/tasks/t1"},
		{http.MethodPost, "/ga4gh/tes/v1/tasks/t1:cancel"},
	}
	for _, rt := range guarded {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status %d, want 401", rt.method, rt.path, w.Code)
		}
	}

	// Probes, the runner callback API and extra routes are not guarded.
	open := []string{"/healthz", "/readyz", "/startupz", "/internal/runs/r1/ga4gh/tes/v1/service-info", "/storage/v1/ping"}
	for _, path := range open {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed {
			t.Errorf("GET %s: status %d, want an open route", path, w.Code)
		}
	}
}

func TestNewRouter_WithoutAuth(t *testing.T) {
	router := NewRouter(wes.NewHandler(nil), tes.NewHandler(nil, nil, nil, nil))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ga4gh/tes/v1/service-info", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
}

// Wrapped around the router as main does, CORS answers preflights before
// chi (no OPTIONS routes: 405) and the token middleware (401) see them.
func TestCORSPreflightBeforeRoutingAndAuth(t *testing.T) {
	ctrl := controller.NewRunController(nil, nil, nil, storage.Layout{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := cors.Middleware([]string{"https://portal.example.org"}, []string{"/ga4gh/wes/", "/ga4gh/tes/"})(
		NewRouterWithAuth(wes.NewHandler(ctrl), tes.NewHandler(nil, nil, nil, nil), denyAll))

	req := httptest.NewRequest(http.MethodOptions, "/ga4gh/wes/v1/runs", nil)
	req.Header.Set("Origin", "https://portal.example.org")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "https://portal.example.org" {
		t.Fatalf("preflight: status %d, headers %v", w.Code, w.Header())
	}

	// the actual request still needs the token, and carries the CORS header
	req = httptest.NewRequest(http.MethodGet, "/ga4gh/wes/v1/runs", nil)
	req.Header.Set("Origin", "https://portal.example.org")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || w.Header().Get("Access-Control-Allow-Origin") != "https://portal.example.org" {
		t.Fatalf("request: status %d, headers %v", w.Code, w.Header())
	}
}
