package auth

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Config is the authentication configuration file (CONQUEST_AUTH_CONFIG).
type Config struct {
	// Issuers whose access tokens are accepted. A token's iss must equal
	// one of their URLs exactly.
	Issuers []IssuerConfig `json:"issuers"`
	// Algorithms are the accepted JWS algorithms (default RS256, ES256).
	// Only asymmetric algorithms are allowed: never none or HMAC.
	Algorithms []string `json:"algorithms,omitempty"`
	// ClockSkewSeconds is the leeway for exp, nbf and iat (default 60).
	ClockSkewSeconds *int `json:"clock_skew_seconds,omitempty"`
	// TimeoutSeconds bounds each request to an issuer: discovery and key
	// set fetches, userinfo requests (default 10, at most 10).
	TimeoutSeconds *int `json:"timeout_seconds,omitempty"`
	// Create is who may create runs and tasks.
	Create CreateConfig `json:"create,omitempty"`
}

// IssuerConfig is one trusted token issuer.
type IssuerConfig struct {
	// URL is the issuer identifier, exactly as in the tokens' iss claim.
	URL string `json:"url"`
	// JWKSURI is the issuer's key set. Empty: taken from the issuer's
	// OpenID Connect discovery document.
	JWKSURI string `json:"jwks_uri,omitempty"`
	// UserinfoURI is the issuer's userinfo endpoint, asked for a user's
	// entitlements on create. Empty: taken from the discovery document.
	UserinfoURI string `json:"userinfo_uri,omitempty"`
	// Audiences are the client IDs of the front ends whose user tokens
	// are accepted: a user token's aud must contain one of them.
	Audiences []string `json:"audiences"`
	// ServiceClients are the clients whose own tokens (client credentials:
	// sub == client_id) are accepted. Such a token's aud must be exactly
	// the client's own ID.
	ServiceClients []ServiceClient `json:"service_clients,omitempty"`
}

// ServiceClient is a client that may call Conquest on its own behalf.
type ServiceClient struct {
	ClientID string `json:"client_id"`
}

// asymmetric lists the algorithms that may be configured.
var asymmetric = map[string]bool{
	"RS256": true, "RS384": true, "RS512": true,
	"PS256": true, "PS384": true, "PS512": true,
	"ES256": true, "ES384": true, "ES512": true,
	"EdDSA": true,
}

// LoadConfig reads and validates the authentication configuration file.
func LoadConfig(file string) (*Config, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read auth config: %w", err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse auth config %s: %w", file, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("auth config %s: %w", file, err)
	}
	return &cfg, nil
}

// Validate checks the configuration and fills in defaults.
func (c *Config) Validate() error {
	if len(c.Issuers) == 0 {
		return fmt.Errorf("no issuers configured")
	}
	seen := map[string]bool{}
	anyServiceClient := false
	for i := range c.Issuers {
		iss := &c.Issuers[i]
		if err := checkURL("issuer url", iss.URL); err != nil {
			return err
		}
		if strings.Contains(iss.URL, "|") {
			// "|" separates issuer and subject in owner IDs.
			return fmt.Errorf("issuer %q: url must not contain |", iss.URL)
		}
		if seen[iss.URL] {
			return fmt.Errorf("issuer %q configured twice", iss.URL)
		}
		seen[iss.URL] = true
		if iss.JWKSURI != "" {
			if err := checkURL("jwks_uri of issuer "+iss.URL, iss.JWKSURI); err != nil {
				return err
			}
		}
		if iss.UserinfoURI != "" {
			if err := checkURL("userinfo_uri of issuer "+iss.URL, iss.UserinfoURI); err != nil {
				return err
			}
		}
		if len(iss.Audiences) == 0 {
			return fmt.Errorf("issuer %q: no audiences configured", iss.URL)
		}
		for _, aud := range iss.Audiences {
			if aud == "" {
				return fmt.Errorf("issuer %q: empty audience", iss.URL)
			}
		}
		clients := map[string]bool{}
		for _, sc := range iss.ServiceClients {
			if sc.ClientID == "" {
				return fmt.Errorf("issuer %q: service client without client_id", iss.URL)
			}
			if clients[sc.ClientID] {
				return fmt.Errorf("issuer %q: service client %q listed twice", iss.URL, sc.ClientID)
			}
			clients[sc.ClientID] = true
			anyServiceClient = true
		}
	}
	if err := c.Create.validate(anyServiceClient); err != nil {
		return err
	}
	if len(c.Algorithms) == 0 {
		c.Algorithms = []string{"RS256", "ES256"}
	}
	for _, alg := range c.Algorithms {
		if !asymmetric[alg] {
			return fmt.Errorf("algorithm %q not allowed (asymmetric algorithms only)", alg)
		}
	}
	if c.ClockSkewSeconds == nil {
		skew := 60
		c.ClockSkewSeconds = &skew
	}
	if *c.ClockSkewSeconds < 0 || *c.ClockSkewSeconds > 300 {
		return fmt.Errorf("clock_skew_seconds must be between 0 and 300")
	}
	if c.TimeoutSeconds == nil {
		timeout := 10
		c.TimeoutSeconds = &timeout
	}
	// A request may wait for two issuer requests in a row (discovery and
	// key set, or the unknown-kid wait and the refetch). The cap keeps
	// that well below the server's 30 s write timeout, so the 503 still
	// reaches the caller, and below the one-minute refetch interval, so
	// lookups fail at once while the refetch slot is used up.
	if *c.TimeoutSeconds <= 0 || *c.TimeoutSeconds > 10 {
		return fmt.Errorf("timeout_seconds must be between 1 and 10")
	}
	return nil
}

func (c *Config) clockSkew() time.Duration { return time.Duration(*c.ClockSkewSeconds) * time.Second }
func (c *Config) timeout() time.Duration   { return time.Duration(*c.TimeoutSeconds) * time.Second }

// checkURL requires an absolute https URL; plain http only to a loopback
// host (tests, local development).
func checkURL(what, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s %q: not an absolute URL", what, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("%s %q: must use https", what, raw)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validate checks the submit rule and fills in defaults.
func (c *CreateConfig) validate(anyServiceClient bool) error {
	if c.EntitlementClaim == "" {
		c.EntitlementClaim = defaultEntitlementClaim
	}
	seen := map[string]bool{}
	for _, e := range c.Entitlements {
		if e == "" {
			return fmt.Errorf("create: empty entitlement")
		}
		if seen[e] {
			return fmt.Errorf("create: entitlement %q listed twice", e)
		}
		seen[e] = true
	}
	if len(c.Entitlements) == 0 && !anyServiceClient {
		// Nobody could ever create: a configuration error, not a policy.
		return fmt.Errorf("create: no entitlements and no service clients configured, so nobody could create runs")
	}
	return nil
}
