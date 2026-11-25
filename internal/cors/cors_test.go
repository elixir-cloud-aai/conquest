package cors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseOrigins(t *testing.T) {
	got, err := ParseOrigins(" https://portal.example.org, ,http://localhost:3000,HTTPS://Other.example ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://portal.example.org", "http://localhost:3000", "https://other.example"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, err := ParseOrigins(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{"*", "https://portal.example.org/", "https://x/path", "portal.example.org", "ftp://x", "https://u@x", "https://x?q=1", "https://x:443", "http://x:80", "https://x?", "https://x#", "https://x/?", " https://x? "} {
		if _, err := ParseOrigins(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func serve(origins []string, method, path string, header map[string]string) (*httptest.ResponseRecorder, bool) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusUnauthorized) // stands in for the token middleware
	})
	h := Middleware(origins, []string{"/ga4gh/wes/", "/ga4gh/tes/"})(next)
	req := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w, reached
}

func TestPreflightIsAnsweredBeforeAuth(t *testing.T) {
	origins := []string{"https://portal.example.org"}
	w, reached := serve(origins, http.MethodOptions, "/ga4gh/wes/v1/runs", map[string]string{
		"Origin":                         "https://portal.example.org",
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "authorization,content-type",
	})
	if reached || w.Code != http.StatusNoContent {
		t.Fatalf("status %d, reached %v; want 204 without auth", w.Code, reached)
	}
	h := w.Header()
	if h.Get("Access-Control-Allow-Origin") != "https://portal.example.org" ||
		h.Get("Access-Control-Allow-Methods") != "GET, POST, OPTIONS" ||
		h.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" ||
		h.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("headers %v", h)
	}
}

func TestOriginMatchIgnoresCase(t *testing.T) {
	w, _ := serve([]string{"https://portal.example.org"}, http.MethodGet, "/ga4gh/tes/v1/tasks",
		map[string]string{"Origin": "https://Portal.Example.org"})
	if w.Header().Get("Access-Control-Allow-Origin") != "https://Portal.Example.org" {
		t.Fatalf("headers %v", w.Header())
	}
}

func TestActualRequestGetsAllowOrigin(t *testing.T) {
	w, reached := serve([]string{"https://portal.example.org"}, http.MethodGet, "/ga4gh/wes/v1/service-info",
		map[string]string{"Origin": "https://portal.example.org"})
	if !reached || w.Header().Get("Access-Control-Allow-Origin") != "https://portal.example.org" {
		t.Fatalf("reached %v, headers %v", reached, w.Header())
	}
	if !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "Origin") {
		t.Fatalf("no Vary: Origin: %v", w.Header())
	}
}

func TestOtherRequestsPassUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		origins []string
		method  string
		path    string
		header  map[string]string
	}{
		{"unlisted origin, preflight", []string{"https://portal.example.org"}, http.MethodOptions, "/ga4gh/wes/v1/runs",
			map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"}},
		{"unlisted origin", []string{"https://portal.example.org"}, http.MethodGet, "/ga4gh/wes/v1/runs",
			map[string]string{"Origin": "https://evil.example"}},
		{"no origin", []string{"https://portal.example.org"}, http.MethodGet, "/ga4gh/wes/v1/runs", nil},
		{"internal path", []string{"https://portal.example.org"}, http.MethodOptions, "/internal/runs/r1/ga4gh/tes/v1/tasks",
			map[string]string{"Origin": "https://portal.example.org", "Access-Control-Request-Method": "POST"}},
		{"storage path", []string{"https://portal.example.org"}, http.MethodGet, "/storage/v1/t/x",
			map[string]string{"Origin": "https://portal.example.org"}},
		{"CORS off", nil, http.MethodOptions, "/ga4gh/wes/v1/runs",
			map[string]string{"Origin": "https://portal.example.org", "Access-Control-Request-Method": "POST"}},
	}
	for _, c := range cases {
		w, reached := serve(c.origins, c.method, c.path, c.header)
		if !reached || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: reached %v, headers %v", c.name, reached, w.Header())
		}
	}
}

// An OPTIONS request that isn't a preflight goes on to routing and auth.
func TestPlainOptionsIsNotAPreflight(t *testing.T) {
	w, reached := serve([]string{"https://portal.example.org"}, http.MethodOptions, "/ga4gh/wes/v1/runs",
		map[string]string{"Origin": "https://portal.example.org"})
	if !reached || w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, reached %v; want it passed on", w.Code, reached)
	}
}
