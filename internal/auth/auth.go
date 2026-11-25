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
//
// Requests that create runs or tasks (Authenticator.Create) also need the
// caller to be entitled to create (create.go).
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

	"github.com/MicahParks/jwkset"
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

// authContext is what runs record about how their owner authenticated:
// identifiers only, never the token.
func (p *Principal) authContext() json.RawMessage {
	// Marshalling two strings cannot fail.
	raw, _ := json.Marshal(struct {
		ClientID string `json:"client_id,omitempty"`
		Scope    string `json:"scope,omitempty"`
	}{p.ClientID, p.Scope})
	return raw
}

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
	// Create reports requests that create runs or tasks: the caller must
	// also be entitled to create (create.go). nil: no request creates.
	Create func(*http.Request) bool

	entitlements     map[string]bool
	entitlementClaim string
}

// issuer holds one configured issuer and, once resolved, its key set.
type issuer struct {
	cfg     IssuerConfig
	clients map[string]bool
	client  *http.Client
	// userinfoClient sends user tokens: it follows no redirects, so a
	// token never goes to a URL other than the userinfo endpoint.
	userinfoClient *http.Client
	ctx            context.Context
	log            *slog.Logger

	mu        sync.Mutex
	keys      keyfunc.Keyfunc
	resolving bool
	lastTry   time.Time
	lastError error

	// The userinfo endpoint, discovered on the first create unless
	// configured (create.go), independently of the key set.
	userinfoMu      sync.Mutex
	userinfoURI     string
	userinfoLastTry time.Time
	userinfoErr     error
}

// discoveryRetry is how long a failed discovery is not retried.
var discoveryRetry = 10 * time.Second

// New creates an Authenticator. ctx ends the background refreshes of the
// issuers' key sets. Key sets are resolved on first use, so an issuer that
// is down at startup does not keep Conquest from starting.
func New(ctx context.Context, cfg *Config, log *slog.Logger) (*Authenticator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: cfg.timeout(), CheckRedirect: checkRedirect}
	userinfoClient := &http.Client{
		Timeout:       cfg.timeout(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	a := &Authenticator{
		issuers: make(map[string]*issuer, len(cfg.Issuers)),
		parser: jwt.NewParser(
			jwt.WithValidMethods(cfg.Algorithms),
			jwt.WithLeeway(cfg.clockSkew()),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
		),
		log:              log,
		entitlements:     make(map[string]bool, len(cfg.Create.Entitlements)),
		entitlementClaim: cfg.Create.EntitlementClaim,
	}
	for _, e := range cfg.Create.Entitlements {
		a.entitlements[e] = true
	}
	for _, ic := range cfg.Issuers {
		clients := make(map[string]bool, len(ic.ServiceClients))
		for _, sc := range ic.ServiceClients {
			clients[sc.ClientID] = true
		}
		a.issuers[ic.URL] = &issuer{
			cfg: ic, clients: clients, client: client, userinfoClient: userinfoClient, ctx: ctx, log: log,
		}
	}
	return a, nil
}

// checkRedirect follows a redirect of a discovery or key set request
// only to a URL that checkURL accepts, so an https endpoint can't hand
// the keys over to plain http.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return checkURL("redirect", req.URL.String())
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
		return nil, fmt.Errorf("untrusted issuer %q", truncate(unverified.Issuer, 256))
	}
	keys, err := iss.keyfunc()
	if err != nil {
		return nil, err
	}

	// The key is chosen by kid only, never by trying every key of the set.
	kidKeyfunc := func(t *jwt.Token) (any, error) {
		if kid, _ := t.Header["kid"].(string); kid == "" {
			return nil, fmt.Errorf("token has no kid")
		}
		return keys.KeyfuncCtx(r.Context())(t)
	}
	var c claims
	token, err := a.parser.ParseWithClaims(raw, &c, kidKeyfunc)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	if typ, _ := token.Header["typ"].(string); !isAccessTokenType(typ) {
		return nil, fmt.Errorf("token typ %q is not at+jwt", typ)
	}
	if c.Subject == "" {
		return nil, fmt.Errorf("token has no sub")
	}
	if c.IssuedAt == nil { // RFC 9068 requires iat; WithIssuedAt only checks it if present
		return nil, fmt.Errorf("token has no iat")
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

// Middleware rejects requests without a valid bearer token, and creates by
// callers who may not create (create.go). It scopes all other requests to
// their caller.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Public != nil && a.Public(r) {
			next.ServeHTTP(w, r)
			return
		}
		p, err := a.Authenticate(r)
		if err == nil && a.Create != nil && a.Create(r) {
			err = a.authorizeCreate(r, p)
		}
		if err != nil {
			a.refuse(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		ctx = ownership.NewContext(ctx, ownership.Owner{ID: p.Owner(), Context: p.authContext()})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// refuse answers a request that failed authentication or the create rule.
func (a *Authenticator) refuse(w http.ResponseWriter, r *http.Request, err error) {
	// The cause goes to the log, never the token.
	msg := "request not authenticated"
	if errors.Is(err, errForbidden) {
		msg = "create refused"
	}
	a.log.Info(msg, "method", r.Method, "path", r.URL.Path, "err", err)
	switch {
	case errors.Is(err, errUnavailable):
		writeError(w, http.StatusServiceUnavailable, "token validation temporarily unavailable")
	case errors.Is(err, errForbidden):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, errNoToken):
		w.Header().Set("WWW-Authenticate", `Bearer`)
		writeError(w, http.StatusUnauthorized, "unauthorized")
	default:
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeError(w, http.StatusUnauthorized, "unauthorized")
	}
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

// truncate shortens untrusted values before they are logged.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
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

// keyfunc returns the issuer's key set, resolving it on first use. Only one
// request resolves at a time, without holding the lock; requests that
// arrive meanwhile, or within discoveryRetry after a failure, fail fast
// with errUnavailable instead of queueing behind a slow issuer.
func (i *issuer) keyfunc() (keys keyfunc.Keyfunc, err error) {
	i.mu.Lock()
	switch {
	case i.keys != nil:
		defer i.mu.Unlock()
		return i.keys, nil
	case i.resolving:
		i.mu.Unlock()
		return nil, fmt.Errorf("%w: issuer %s: key set being fetched", errUnavailable, i.cfg.URL)
	case !i.lastTry.IsZero() && time.Since(i.lastTry) < discoveryRetry:
		defer i.mu.Unlock()
		return nil, i.lastError
	}
	i.resolving = true
	i.mu.Unlock()

	// The state is updated however resolve ends, a panic included, so
	// that a failed attempt never leaves the issuer "being fetched".
	err = errResolvePanicked
	defer func() {
		i.mu.Lock()
		defer i.mu.Unlock()
		i.resolving = false
		i.lastTry = time.Now() // the retry delay starts when the attempt ends
		if err != nil {
			i.lastError = fmt.Errorf("%w: issuer %s: %v", errUnavailable, i.cfg.URL, err)
			keys, err = nil, i.lastError
			return
		}
		i.keys = keys
	}()
	keys, err = i.resolve()
	return keys, err
}

var errResolvePanicked = errors.New("key set resolution panicked")

// resolve discovers the issuer's key set (unless configured) and fetches it.
func (i *issuer) resolve() (keyfunc.Keyfunc, error) {
	// The key set's background refresh runs on ctx: an attempt that fails
	// stops it, so failed attempts leave no goroutines behind.
	ctx, cancel := context.WithCancel(i.ctx)
	ok := false
	defer func() {
		if !ok {
			cancel()
		}
	}()

	jwksURI := i.cfg.JWKSURI
	if jwksURI == "" {
		doc, err := i.discover(ctx)
		if err != nil {
			return nil, err
		}
		if err := checkURL("discovered jwks_uri", doc.JWKSURI); err != nil {
			return nil, err
		}
		jwksURI = doc.JWKSURI
	}
	failFirst := false // a key set that can't be fetched is an error, retried later
	keys, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{jwksURI}, keyfunc.Override{
		Client:                    i.client,
		HTTPTimeout:               i.client.Timeout,
		NoErrorReturnFirstHTTPReq: &failFirst,
		// An unknown kid refetches the key set at most once a minute. The
		// wait bound is also the refetch's deadline, so it is the request
		// timeout, capped below the interval (Validate): while the limiter
		// is used up, lookups fail at once instead of queueing.
		RefreshUnknownKID: rate.NewLimiter(rate.Every(time.Minute), 1),
		RateLimitWaitMax:  i.client.Timeout,
		RefreshErrorHandlerFunc: func(u string) func(context.Context, error) {
			return func(ctx context.Context, err error) {
				i.log.WarnContext(ctx, "key set refresh failed", "issuer", i.cfg.URL, "url", u, "err", err)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("key set: %w", err)
	}
	// Only signing keys (or keys that don't say) verify tokens.
	kf, err := keyfunc.New(keyfunc.Options{
		Ctx:          ctx,
		Storage:      keys.Storage(),
		UseWhitelist: []jwkset.USE{jwkset.UseSig, ""},
	})
	if err != nil {
		return nil, err
	}
	ok = true
	return kf, nil
}

// discoveryDoc is the part of an OpenID Connect discovery document that
// Conquest reads.
type discoveryDoc struct {
	Issuer           string `json:"issuer"`
	JWKSURI          string `json:"jwks_uri"`
	UserinfoEndpoint string `json:"userinfo_endpoint"`
}

// discover reads the issuer's OpenID Connect discovery document, which
// must name the configured issuer exactly.
func (i *issuer) discover(ctx context.Context) (*discoveryDoc, error) {
	u := strings.TrimSuffix(i.cfg.URL, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := i.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery: %s", resp.Status)
	}
	var doc discoveryDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if doc.Issuer != i.cfg.URL {
		return nil, fmt.Errorf("discovery names issuer %q", doc.Issuer)
	}
	return &doc, nil
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(struct {
		Msg        string `json:"msg"`
		StatusCode int    `json:"status_code"`
	}{msg, status})
}
