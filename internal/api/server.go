// Package api assembles the HTTP server with all route groups.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/anuragxxd/conquest/internal/api/tes"
	"github.com/anuragxxd/conquest/internal/api/wes"
)

// PublicPathPrefixes are the path prefixes of the public GA4GH APIs (WES
// and TES), e.g. for CORS.
var PublicPathPrefixes = []string{"/ga4gh/wes/", "/ga4gh/tes/"}

// NewRouter creates a chi router with all WES and TES routes registered,
// plus health, readiness, and startup probes matching the platform convention.
// extra mounts further routes (e.g. the storage gateway).
func NewRouter(wesHandler *wes.Handler, tesHandler *tes.Handler, extra ...func(chi.Router)) http.Handler {
	return NewRouterWithAuth(wesHandler, tesHandler, nil, extra...)
}

// NewRouterWithAuth is NewRouter with authn (e.g. auth.Authenticator's
// Middleware) in front of the public WES and TES routes; nil mounts none.
// Probes, the internal runner routes and extra routes are not affected.
func NewRouterWithAuth(wesHandler *wes.Handler, tesHandler *tes.Handler, authn func(http.Handler) http.Handler, extra ...func(chi.Router)) http.Handler {
	r := chi.NewRouter()

	// Middleware.
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))

	// Kubernetes probes — matches infra chart pattern:
	//   startupProbe:  /startupz
	//   readinessProbe: /readyz
	//   livenessProbe:  /healthz
	okResponse := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
	r.Get("/healthz", okResponse)
	r.Get("/readyz", okResponse)
	r.Get("/startupz", okResponse)

	r.Group(func(r chi.Router) {
		if authn != nil {
			r.Use(authn)
		}

		// WES routes.
		wesHandler.Routes(r)

		// TES routes (public).
		tesHandler.Routes(r)
	})

	// TES routes (internal, run-scoped — used by Nextflow runner).
	tesHandler.InternalRoutes(r)

	for _, mount := range extra {
		mount(r)
	}

	return r
}
