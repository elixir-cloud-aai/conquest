package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CreateConfig is who may create runs and tasks (the create rule; README:
// Authentication).
type CreateConfig struct {
	// Entitlements are the values, one of which a user must hold to create.
	// Empty: only listed service clients create.
	Entitlements []string `json:"entitlements,omitempty"`
	// EntitlementClaim is the userinfo claim that holds the user's
	// entitlements, as a list of strings or a single string (default
	// eduperson_entitlement).
	EntitlementClaim string `json:"entitlement_claim,omitempty"`
}

const defaultEntitlementClaim = "eduperson_entitlement"

// maxUserinfoBytes bounds the userinfo body Conquest reads.
const maxUserinfoBytes = 1 << 20

// errForbidden marks an authenticated caller who may not create (403).
var errForbidden = errors.New("caller may not create")

// errUserinfoRefused marks a token that the issuer's userinfo endpoint
// refused (401 or 403): the token is treated as invalid (401).
var errUserinfoRefused = errors.New("userinfo refused the token")

// authorizeCreate checks that the authenticated caller p may create.
func (a *Authenticator) authorizeCreate(r *http.Request, p *Principal) error {
	if p.Service {
		return nil // a listed client stands in for the entitlement
	}
	if p.ClientID != "" && p.Subject == p.ClientID {
		// A client's own token, but the client isn't listed. The issuer
		// releases no user entitlements for it, so don't ask.
		return fmt.Errorf("%w: token of client %q, which is not a listed service client", errForbidden, truncate(p.ClientID, 256))
	}
	if len(a.entitlements) == 0 {
		return fmt.Errorf("%w: no entitlements configured, only service clients create", errForbidden)
	}
	iss := a.issuers[p.Issuer] // Authenticate accepted the issuer
	token, err := bearerToken(r)
	if err != nil {
		return err
	}
	info, err := iss.userinfo(r.Context(), token)
	if err != nil {
		return err
	}
	var sub string
	if err := json.Unmarshal(info["sub"], &sub); err != nil || sub != p.Subject {
		return fmt.Errorf("%w: userinfo sub does not match the token's sub", errUserinfoRefused)
	}
	for _, e := range entitlements(info, a.entitlementClaim) {
		if a.entitlements[e] {
			return nil
		}
	}
	return fmt.Errorf("%w: no configured entitlement in userinfo claim %s", errForbidden, a.entitlementClaim)
}

// entitlements returns the claim's values: a list of strings or a single
// string. Anything else counts as no entitlements.
func entitlements(info map[string]json.RawMessage, claim string) []string {
	raw, ok := info[claim]
	if !ok {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	return nil
}

// userinfo asks the issuer's userinfo endpoint about token. A refusal of the
// token (401, 403) is errUserinfoRefused; an endpoint that can't be reached,
// times out or doesn't answer 200 with a JSON object is errUnavailable.
func (i *issuer) userinfo(ctx context.Context, token string) (map[string]json.RawMessage, error) {
	endpoint, err := i.userinfoEndpoint()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: userinfo: %v", errUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := i.userinfoClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: userinfo: %v", errUnavailable, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", errUserinfoRefused, resp.Status)
	default:
		return nil, fmt.Errorf("%w: userinfo: %s", errUnavailable, resp.Status)
	}
	// One byte over the limit tells a body that is too large from one
	// that just fits; Unmarshal refuses data after the object.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUserinfoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: userinfo: %v", errUnavailable, err)
	}
	if len(body) > maxUserinfoBytes {
		return nil, fmt.Errorf("%w: userinfo: body larger than %d bytes", errUnavailable, maxUserinfoBytes)
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(body, &info); err != nil || info == nil {
		return nil, fmt.Errorf("%w: userinfo: not a single JSON object", errUnavailable)
	}
	return info, nil
}

// userinfoEndpoint returns the configured userinfo endpoint or the one named
// by the issuer's discovery document. Discovery happens on the first create
// and is retried discoveryRetry after a failure; it never affects token
// validation, so reads keep working when it fails. It runs on the issuer's
// context, not the request's, so a caller who goes away can't cause a
// failure that other callers would then get.
func (i *issuer) userinfoEndpoint() (string, error) {
	if i.cfg.UserinfoURI != "" {
		return i.cfg.UserinfoURI, nil
	}
	i.userinfoMu.Lock()
	defer i.userinfoMu.Unlock()
	switch {
	case i.userinfoURI != "":
		return i.userinfoURI, nil
	case !i.userinfoLastTry.IsZero() && time.Since(i.userinfoLastTry) < discoveryRetry:
		return "", i.userinfoErr
	}
	endpoint, err := i.discoverUserinfo(i.ctx)
	i.userinfoLastTry = time.Now()
	if err != nil {
		i.userinfoErr = fmt.Errorf("%w: issuer %s: %v", errUnavailable, i.cfg.URL, err)
		return "", i.userinfoErr
	}
	i.userinfoURI = endpoint
	return endpoint, nil
}

// discoverUserinfo reads userinfo_endpoint from the discovery document; it
// must be named and pass checkURL.
func (i *issuer) discoverUserinfo(ctx context.Context) (string, error) {
	doc, err := i.discover(ctx)
	if err != nil {
		return "", err
	}
	if doc.UserinfoEndpoint == "" {
		return "", fmt.Errorf("discovery names no userinfo_endpoint")
	}
	if err := checkURL("discovered userinfo_endpoint", doc.UserinfoEndpoint); err != nil {
		return "", err
	}
	return doc.UserinfoEndpoint, nil
}
