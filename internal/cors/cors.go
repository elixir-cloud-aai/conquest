// Package cors lets browser front ends on configured origins call the
// public GA4GH APIs (Cross-Origin Resource Sharing).
//
// Origins are exact ("https://portal.example.org"), never "*": the APIs
// take bearer tokens in the Authorization header. Preflight requests
// (OPTIONS with Access-Control-Request-Method) are answered here, before
// routing and before authentication, since browsers send them without the
// token. Cookies are not used, so credentials are never allowed.
package cors

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Methods and headers a front end may use.
const (
	allowMethods = "GET, POST, OPTIONS"
	allowHeaders = "Authorization, Content-Type"
	maxAge       = "600"
)

// ParseOrigins parses a comma-separated list of exact origins
// (scheme://host[:port], no path); empty entries are skipped.
func ParseOrigins(raw string) ([]string, error) {
	var origins []string
	for _, o := range strings.Split(raw, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o == "*" {
			return nil, fmt.Errorf("origin %q: list exact origins; a wildcard is not allowed with bearer tokens", o)
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("origin %q must be scheme://host[:port] without path", o)
		}
		if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
			return nil, fmt.Errorf("origin %q: leave out the default port; browsers send origins without it", o)
		}
		origins = append(origins, strings.ToLower(o))
	}
	return origins, nil
}

// Middleware adds CORS headers to requests below one of pathPrefixes from
// one of origins, and answers their preflight requests with 204. Other
// requests pass unchanged. With no origins it returns next as it is.
func Middleware(origins, pathPrefixes []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[strings.ToLower(o)] = true
	}
	return func(next http.Handler) http.Handler {
		if len(allowed) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !hasPrefix(r.URL.Path, pathPrefixes) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Add("Vary", "Origin")
			origin := r.Header.Get("Origin")
			if origin == "" || !allowed[strings.ToLower(origin)] {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Add("Vary", "Access-Control-Request-Headers")
				w.Header().Set("Access-Control-Allow-Methods", allowMethods)
				w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
				w.Header().Set("Access-Control-Max-Age", maxAge)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func hasPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
