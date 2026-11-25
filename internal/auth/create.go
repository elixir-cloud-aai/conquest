package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Who may create runs and tasks (the submit rule): a listed service client,
// or a user whose entitlements, as the issuer's userinfo endpoint releases
// them for the request's own token, contain one of the configured values.
// Entitlements are compared as exact strings, never as prefixes. Only
// creates check them: reading and cancelling own runs needs no entitlement,
// so a former member keeps access to their runs and an IdP outage doesn't
// block it. Nothing is cached, so a revoked entitlement takes effect on the
// next create.

// CreateConfig is who may create runs and tasks.
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
	iss, ok := a.issuers[p.Issuer]
	if !ok { // Authenticate accepted the token, so this can't happen
		return fmt.Errorf("untrusted issuer %q", truncate(p.Issuer, 256))
	}
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
		return fmt.Errorf("userinfo sub does not match the token's sub")
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
	i.mu.Lock()
	endpoint := i.userinfoURI
	i.mu.Unlock()
	if endpoint == "" {
		return nil, fmt.Errorf("%w: issuer %s: no userinfo endpoint configured or discovered", errUnavailable, i.cfg.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: userinfo: %v", errUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := i.client.Do(req)
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
	var info map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil || info == nil {
		return nil, fmt.Errorf("%w: userinfo: not a JSON object", errUnavailable)
	}
	return info, nil
}
