package pg

import (
	"context"
	"fmt"

	"github.com/anuragxxd/conquest/internal/ownership"
)

// Owner scoping (package ownership). Read queries that serve API callers
// append one of these predicates, so that a context scoped to an owner sees
// only that owner's runs and their tasks. An unscoped context gets no
// predicate and sees everything. NULL owners (runs from before
// authentication) never match an owner.
//
// Writes are not filtered: callers address a run or task by ID only after a
// scoped read has returned it. Aggregates (CountByState) and the internal
// scans used by reconcilers stay unscoped.

// runOwnerFilter returns " AND owner = $n" and args with the owner appended
// (n = len(args)+1) for a context scoped to an owner, or "" and args
// unchanged for an unscoped one.
func runOwnerFilter(ctx context.Context, args ...any) (string, []any) {
	owner, ok := ownership.FromContext(ctx)
	if !ok {
		return "", args
	}
	args = append(args, owner.ID)
	return fmt.Sprintf(" AND owner = $%d", len(args)), args
}

// taskOwnerFilter is runOwnerFilter for queries on tasks: a task belongs to
// the owner of its run.
func taskOwnerFilter(ctx context.Context, args ...any) (string, []any) {
	owner, ok := ownership.FromContext(ctx)
	if !ok {
		return "", args
	}
	args = append(args, owner.ID)
	return fmt.Sprintf(" AND EXISTS (SELECT 1 FROM runs WHERE runs.run_id = tasks.run_id AND runs.owner = $%d)", len(args)),
		args
}
