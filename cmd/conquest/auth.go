package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/anuragxxd/conquest/internal/auth"
	"github.com/anuragxxd/conquest/internal/config"
)

// serviceInfoPaths are the public service-info endpoints.
var serviceInfoPaths = map[string]bool{
	"/ga4gh/wes/v1/service-info": true,
	"/ga4gh/tes/v1/service-info": true,
	"/ga4gh/tes/service-info":    true,
}

// newAuthMiddleware returns the token validation middleware for the public
// APIs, or nil when authentication is off.
func newAuthMiddleware(ctx context.Context, cfg *config.Config, log *slog.Logger) (func(http.Handler) http.Handler, error) {
	if !cfg.AuthEnabled {
		log.Warn("authentication disabled: the WES and TES APIs are open to every caller (set CONQUEST_AUTH_ENABLED=true)")
		return nil, nil
	}
	authCfg, err := auth.LoadConfig(cfg.AuthConfigFile)
	if err != nil {
		return nil, err
	}
	a, err := auth.New(ctx, authCfg, log)
	if err != nil {
		return nil, err
	}
	if cfg.AuthServiceInfoPublic {
		a.Public = func(r *http.Request) bool {
			return r.Method == http.MethodGet && serviceInfoPaths[r.URL.Path]
		}
	}
	issuers := make([]string, len(authCfg.Issuers))
	for i, iss := range authCfg.Issuers {
		issuers[i] = iss.URL
	}
	log.Info("authentication enabled", "issuers", issuers, "service_info_public", cfg.AuthServiceInfoPublic)
	return a.Middleware, nil
}
