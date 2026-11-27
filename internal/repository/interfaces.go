// Package repository defines storage interfaces for the domain entities.
//
// Implementations live in sub-packages (e.g. repository/pg for Postgres).
// The interfaces are small and focused on what the controller layer needs.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/anuragxxd/conquest/internal/domain"
)

// Page holds pagination parameters.
type Page struct {
	Size  int
	Token string // opaque cursor; empty for first page
}

// PageResult holds a page of results with a next-page cursor.
type PageResult[T any] struct {
	Items     []T
	NextToken string // empty if no more pages
}

// RunStore persists and queries WES runs.
type RunStore interface {
	Create(ctx context.Context, run *domain.Run) error
	Get(ctx context.Context, runID string) (*domain.Run, error)
	GetStatus(ctx context.Context, runID string) (domain.State, error)
	List(ctx context.Context, page Page) (*PageResult[*domain.Run], error)
	UpdateState(ctx context.Context, runID string, from, to domain.State) error
	SetError(ctx context.Context, runID string, to domain.State, summary string) error
	CountByState(ctx context.Context) (map[domain.State]int, error)
	// Batch D additions:
	ListActiveRuns(ctx context.Context, limit int) ([]*domain.Run, error)
	SetBackendRef(ctx context.Context, runID string, ref string) error
	SetBackendState(ctx context.Context, runID string, state json.RawMessage) error
	SetWorkspaceBinding(ctx context.Context, runID string, binding *domain.WorkspaceBinding) error
}

// TaskStore persists and queries TES tasks.
type TaskStore interface {
	Create(ctx context.Context, task *domain.Task) error
	Get(ctx context.Context, taskID string) (*domain.Task, error)
	GetByTESID(ctx context.Context, tesTaskID string) (*domain.Task, error)
	List(ctx context.Context, page Page) (*PageResult[*domain.Task], error)
	ListByRunID(ctx context.Context, runID string, page Page) (*PageResult[*domain.Task], error)
	ListActiveTasks(ctx context.Context, limit int) ([]*domain.Task, error)
	UpdateState(ctx context.Context, taskID string, from, to domain.State) error
	SetExitCode(ctx context.Context, taskID string, code int32) error
	SetBackendRef(ctx context.Context, taskID string, ref string) error
	SetBackendAssignment(ctx context.Context, taskID, backendName, ref string, route json.RawMessage) error
	SetBackendState(ctx context.Context, taskID string, state json.RawMessage) error
	SetOutputLogs(ctx context.Context, taskID string, logs json.RawMessage) error
}

// DispatchClaim is an undispatched task that a dispatcher has claimed.
type DispatchClaim struct {
	TaskID string
	// Attempt counts submissions of the task, this one included (from 1).
	Attempt int
}

// DispatchQueue hands undispatched tasks (QUEUED, no backend_ref) to
// dispatchers. Claims are leases: a task whose dispatcher stops before
// submitting it is claimed again once the lease expires.
type DispatchQueue interface {
	// ClaimForDispatch claims up to limit undispatched tasks, oldest
	// first, that no live lease holds and whose retry delay has passed.
	ClaimForDispatch(ctx context.Context, limit int, lease time.Duration) ([]DispatchClaim, error)
	// DeferDispatch releases the claim and delays the next attempt.
	DeferDispatch(ctx context.Context, taskID string, notBefore time.Time) error
}

// EventStore persists and queries domain events.
type EventStore interface {
	Append(ctx context.Context, event *domain.Event) error
	ListByRunID(ctx context.Context, runID string) ([]*domain.Event, error)
}

// ErrNotFound is returned when the requested entity does not exist.
var ErrNotFound = errors.New("not found")

// RunCursor is a keyset position in runs ordered by finished_at and run ID;
// the zero value is the start.
type RunCursor struct {
	FinishedAt time.Time
	RunID      string
}

// RetentionStore finds the runs whose data has expired and records that
// their data is gone (package retention). It ignores run ownership.
type RetentionStore interface {
	// ListExpiredRuns returns up to limit terminal runs that finished before
	// finishedBefore and still have their data, ordered by finished_at and
	// run ID, after the cursor. Every run returned has FinishedAt set.
	ListExpiredRuns(ctx context.Context, finishedBefore time.Time, after RunCursor, limit int) ([]*domain.Run, error)
	// SetDataDeleted records that the run's data is gone.
	SetDataDeleted(ctx context.Context, runID string) error
	// Get reads a run; a missing run is ErrNotFound.
	Get(ctx context.Context, runID string) (*domain.Run, error)
}

// Leases hands out leases of cluster-wide background jobs, so that one
// server replica at a time runs each job.
type Leases interface {
	// Claim takes the lease name for holder for ttl, if it is free, expired
	// or holder's already, and reports whether holder holds it now.
	Claim(ctx context.Context, name, holder string, ttl time.Duration) (bool, error)
}
