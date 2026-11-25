package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
issuers:
  - url: https://login.example.org/oidc/
    audiences: [web]
    service_clients:
      - client_id: svc
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Algorithms, ","); got != "RS256,ES256" {
		t.Errorf("algorithms = %s", got)
	}
	if *cfg.ClockSkewSeconds != 60 || *cfg.TimeoutSeconds != 10 {
		t.Errorf("clock skew %d, timeout %d", *cfg.ClockSkewSeconds, *cfg.TimeoutSeconds)
	}
	if cfg.Issuers[0].ServiceClients[0].ClientID != "svc" {
		t.Errorf("service clients = %+v", cfg.Issuers[0].ServiceClients)
	}
}

func TestLoadConfig_Refused(t *testing.T) {
	tests := map[string]string{
		"no issuers":          `issuers: []`,
		"unknown key":         "issuers:\n  - url: https://a.example/\n    audiences: [web]\n    audience: [typo]",
		"http issuer":         "issuers:\n  - url: http://a.example/\n    audiences: [web]",
		"relative issuer":     "issuers:\n  - url: /oidc\n    audiences: [web]",
		"http jwks_uri":       "issuers:\n  - url: https://a.example/\n    jwks_uri: http://a.example/jwks\n    audiences: [web]",
		"no audiences":        "issuers:\n  - url: https://a.example/",
		"empty audience":      "issuers:\n  - url: https://a.example/\n    audiences: ['']",
		"duplicate issuer":    "issuers:\n  - url: https://a.example/\n    audiences: [web]\n  - url: https://a.example/\n    audiences: [web]",
		"empty client_id":     "issuers:\n  - url: https://a.example/\n    audiences: [web]\n    service_clients: [{client_id: ''}]",
		"duplicate client":    "issuers:\n  - url: https://a.example/\n    audiences: [web]\n    service_clients: [{client_id: s}, {client_id: s}]",
		"symmetric algorithm": "issuers:\n  - url: https://a.example/\n    audiences: [web]\nalgorithms: [HS256]",
		"alg none":            "issuers:\n  - url: https://a.example/\n    audiences: [web]\nalgorithms: [none]",
		"negative skew":       "issuers:\n  - url: https://a.example/\n    audiences: [web]\nclock_skew_seconds: -1",
		"zero timeout":        "issuers:\n  - url: https://a.example/\n    audiences: [web]\ntimeout_seconds: 0",
		"long timeout":        "issuers:\n  - url: https://a.example/\n    audiences: [web]\ntimeout_seconds: 31",
	}
	for name, body := range tests {
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestLoadConfig_LoopbackHTTPAllowed(t *testing.T) {
	for _, u := range []string{"http://localhost:8080/", "http://127.0.0.1:9000/", "http://[::1]:9000/"} {
		if _, err := LoadConfig(writeConfig(t, "issuers:\n  - url: "+u+"\n    audiences: [web]")); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}
