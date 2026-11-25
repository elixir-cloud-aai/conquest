// Package auth validates OAuth 2.0 bearer access tokens (JWT, RFC 9068) on
// Conquest's public APIs and scopes each authenticated request to its
// caller (package ownership).
//
// The middleware is mounted only on the public WES and TES routes; probes,
// the runner callback API and the storage gateway are not affected. With
// authentication off (the default) nothing is mounted and Conquest behaves
// as before.
//
// A token is accepted when it is a JWS signed with an allowed asymmetric
// algorithm by a key of a configured issuer (iss matches exactly; keys come
// only from that issuer's key set), its typ is at+jwt, it has not expired,
// and its audience fits the caller:
//
//   - a service client (its client_id is listed for the issuer and sub ==
//     client_id) must present aud == its own client ID;
//   - a user token's aud must contain one of the issuer's audiences (the
//     client IDs of the front ends whose tokens Conquest accepts).
//
// A token whose sub equals a listed client ID without being that client's
// own token is refused. The caller's owner identity is "<iss>|<sub>".
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/time/rate"

	"github.com/anuragxxd/conquest/internal/ownership"
)

// Principal is an authenticated caller.
type Principal struct {
	Issuer   string
	Subject  string
	ClientID string // client_id claim, if any
	Scope    string // scope claim, if any
	// Service is true for a listed service client using its own token.
	Service bool
}

// Owner is the caller's owner identifier: "<iss>|<sub>".
func (p *Principal) Owner() string { return p.Issuer + "|" + p.Subject }

type principalKey struct{}

// PrincipalFromContext returns the principal of an authenticated request.
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok
}

// errUnavailable marks failures on Conquest's side (an issuer's metadata
// could not be fetched): the request is answered 503, not 401.
var errUnavailable = errors.New("token validation unavailable")

// Authenticator validates bearer tokens.
type Authenticator struct {
	issuers map[string]*issuer
	parser  *jwt.Parser
	log     *slog.Logger
	// Public reports requests that need no token (e.g. service-info); nil:
	// every request needs one.
	Public func(*http.Request) bool
}

// issuer holds one configured issuer and, once resolved, its key set.
type issuer struct {
	cfg     IssuerConfig
	clients map[string]bool
	client  *http.Client
	ctx     context.Context

	mu        sync.Mutex
	keys      keyfunc.Keyfunc
	lastTry   time.Time
	lastError error
}

// discoveryRetry is how long a failed discovery is not retried.
const discoveryRetry = 10 * time.Second

// New creates an Authenticator. ctx ends the background refreshes of the
// issuers' key sets. Key sets are resolved on first use, so an issuer that
// is down at startup does not keep Conquest from starting.
func New(ctx context.Context, cfg *Config, log *slog.Logger) (*Authenticator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: cfg.timeout()}
	a := &Authenticator{
		issuers: make(map[string]*issuer, len(cfg.Issuers)),
		parser: jwt.NewParser(
			jwt.WithValidMethods(cfg.Algorithms),
			jwt.WithLeeway(cfg.clockSkew()),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
		),
		log: log,
	}
	for _, ic := range cfg.Issuers {
		clients := make(map[string]bool, len(ic.ServiceClients))
		for _, sc := range ic.ServiceClients {
			clients[sc.ClientID] = true
		}
		a.issuers[ic.URL] = &issuer{cfg: ic, clients: clients, client: client, ctx: ctx}
	}
	return a, nil
}

// claims are the access token claims Conquest reads.
type claims struct {
	jwt.RegisteredClaims
	ClientID string `json:"client_id,omitempty"`
	Scope    string `json:"scope,omitempty"`
}

// Authenticate validates the request's bearer token.
func (a *Authenticator) Authenticate(r *http.Request) (*Principal, error) {
	raw, err := bearerToken(r)
	if err != nil {
		return nil, err
	}

	// The issuer selects the key set; until the signature is verified, iss
	// is only a lookup key that must match a configured issuer exactly.
	var unverified claims
	if _, _, err := jwt.NewParser().ParseUnverified(raw, &unverified); err != nil {
		return nil, fmt.Errorf("malformed token: %w", err)
	}
	iss, ok := a.issuers[unverified.Issuer]
	if !ok {
		return nil, fmt.Errorf("untrusted issuer %q", unverified.Issuer)
	}
	keys, err := iss.keyfunc()
	if err != nil {
		return nil, err
	}

	var c claims
	token, err := a.parser.ParseWithClaims(raw, &c, keys.KeyfuncCtx(r.Context()))
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	if c.Issuer != iss.cfg.URL {
		return nil, fmt.Errorf("issuer changed between parses")
	}
	if typ, _ := token.Header["typ"].(string); !isAccessTokenType(typ) {
		return nil, fmt.Errorf("token typ %q is not at+jwt", typ)
	}
	if c.Subject == "" {
		return nil, fmt.Errorf("token has no sub")
	}

	p := &Principal{Issuer: c.Issuer, Subject: c.Subject, ClientID: c.ClientID, Scope: c.Scope}
	switch {
	case iss.clients[c.ClientID] && c.Subject == c.ClientID:
		// A listed service client's own token: aud must be its own ID.
		if len(c.Audience) != 1 || c.Audience[0] != c.ClientID {
			return nil, fmt.Errorf("service client %q: aud %v is not its own client ID", c.ClientID, []string(c.Audience))
		}
		p.Service = true
	case iss.clients[c.Subject]:
		return nil, fmt.Errorf("sub %q is a service client ID but not that client's own token", c.Subject)
	default:
		if !containsAny(c.Audience, iss.cfg.Audiences) {
			return nil, fmt.Errorf("aud %v contains no configured audience", []string(c.Audience))
		}
	}
	return p, nil
}

// Middleware rejects requests without a valid bearer token and scopes the
// others to their caller.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Public != nil && a.Public(r) {
			next.ServeHTTP(w, r)
			return
		}
		p, err := a.Authenticate(r)
		if err != nil {
			// The cause goes to the log, never the token.
			a.log.Info("request not authenticated", "method", r.Method, "path", r.URL.Path, "err", err)
			if errors.Is(err, errUnavailable) {
				writeError(w, http.StatusServiceUnavailable, "token validation temporarily unavailable")
				return
			}
			if errors.Is(err, errNoToken) {
				w.Header().Set("WWW-Authenticate", `Bearer`)
			} else {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			}
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		authContext, _ := json.Marshal(struct {
			ClientID string `json:"client_id,omitempty"`
			Scope    string `json:"scope,omitempty"`
		}{p.ClientID, p.Scope})
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		ctx = ownership.NewContext(ctx, ownership.Owner{ID: p.Owner(), Context: authContext})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

var errNoToken = errors.New("no bearer token")

// bearerToken returns the token of the request's single Authorization
// header. Tokens in query parameters or cookies are not accepted.
func bearerToken(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return "", errNoToken
	}
	if len(values) > 1 {
		return "", fmt.Errorf("more than one Authorization header")
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("authorization scheme is not Bearer")
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", fmt.Errorf("malformed bearer token")
	}
	return token, nil
}

// isAccessTokenType accepts the JWT access token types of RFC 9068.
func isAccessTokenType(typ string) bool {
	return strings.EqualFold(typ, "at+jwt") || strings.EqualFold(typ, "application/at+jwt")
}

func containsAny(have jwt.ClaimStrings, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

// keyfunc returns the issuer's key set, resolving it on first use.
func (i *issuer) keyfunc() (keyfunc.Keyfunc, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.keys != nil {
		return i.keys, nil
	}
	if !i.lastTry.IsZero() && time.Since(i.lastTry) < discoveryRetry {
		return nil, i.lastError
	}
	i.lastTry = time.Now()
	jwksURI := i.cfg.JWKSURI
	if jwksURI == "" {
		var err error
		if jwksURI, err = i.discover(); err != nil {
			i.lastError = fmt.Errorf("%w: issuer %s: %v", errUnavailable, i.cfg.URL, err)
			return nil, i.lastError
		}
	}
	keys, err := keyfunc.NewDefaultOverrideCtx(i.ctx, []string{jwksURI}, keyfunc.Override{
		Client:      i.client,
		HTTPTimeout: i.client.Timeout,
		// An unknown kid refetches the key set at most once a minute and
		// never makes a request wait for that.
		RefreshUnknownKID: rate.NewLimiter(rate.Every(time.Minute), 1),
		RateLimitWaitMax:  time.Millisecond,
	})
	if err != nil {
		i.lastError = fmt.Errorf("%w: issuer %s: key set: %v", errUnavailable, i.cfg.URL, err)
		return nil, i.lastError
	}
	i.keys = keys
	return keys, nil
}

// discover reads jwks_uri from the issuer's OpenID Connect discovery
// document, which must name the configured issuer exactly.
func (i *issuer) discover() (string, error) {
	u := strings.TrimSuffix(i.cfg.URL, "/") + "/.well-known/openid-configuration"
	resp, err := i.client.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery: %s", resp.Status)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("discovery: %w", err)
	}
	if doc.Issuer != i.cfg.URL {
		return "", fmt.Errorf("discovery names issuer %q", doc.Issuer)
	}
	if err := checkURL("discovered jwks_uri", doc.JWKSURI); err != nil {
		return "", err
	}
	return doc.JWKSURI, nil
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(struct {
		Msg        string `json:"msg"`
		StatusCode int    `json:"status_code"`
	}{msg, status})
}
