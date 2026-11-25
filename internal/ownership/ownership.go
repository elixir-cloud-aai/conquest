// Package ownership scopes runs, and the tasks that belong to them, to the
// caller that created them.
//
// A request carries an Owner in its context when an authentication layer
// has identified the caller. The repository then stamps new runs with that
// owner and returns only that owner's runs and tasks. A context without an
// Owner is unscoped: Conquest behaves as it does without authentication,
// and internal callers (reconcilers, the dispatcher, the runner callback
// API) keep seeing every run.
package ownership

import (
	"context"
	"encoding/json"
)

// Owner identifies the creator of a run.
type Owner struct {
	// ID is the stable owner identifier, e.g. "<issuer>|<subject>" for
	// OpenID Connect callers. Runs are matched on it exactly; an empty ID
	// matches no run.
	ID string
	// Context records how the owner authenticated (identifiers only, never
	// credentials), e.g. {"client_id": "...", "scope": "..."}. Optional.
	Context json.RawMessage
}

type ctxKey struct{}

// NewContext returns a copy of ctx scoped to owner.
func NewContext(ctx context.Context, owner Owner) context.Context {
	return context.WithValue(ctx, ctxKey{}, owner)
}

// FromContext returns the owner ctx is scoped to, and false if ctx is
// unscoped.
func FromContext(ctx context.Context) (Owner, bool) {
	owner, ok := ctx.Value(ctxKey{}).(Owner)
	return owner, ok
}
