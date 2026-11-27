package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/repository"
)

// ListExpiredRuns returns up to limit runs in a terminal state that finished
// before finishedBefore and still have their data, oldest first, after the
// keyset cursor after (zero: from the start). It ignores run ownership: the
// retention sweep reads every run.
func (s *RunStore) ListExpiredRuns(ctx context.Context, finishedBefore time.Time, after repository.RunCursor, limit int) ([]*domain.Run, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+runColumns+` FROM runs
		WHERE data_deleted_at IS NULL
		  AND finished_at IS NOT NULL AND finished_at < $1
		  AND state IN ('COMPLETE','EXECUTOR_ERROR','SYSTEM_ERROR','CANCELED','PREEMPTED')
		  AND (finished_at, run_id) > ($2, $3)
		ORDER BY finished_at ASC, run_id ASC
		LIMIT $4`,
		finishedBefore, after.FinishedAt, after.RunID, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired runs: %w", err)
	}
	defer rows.Close()
	var runs []*domain.Run
	for rows.Next() {
		r, err := scanRunFields(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// SetDataDeleted records that the run's data is gone; a run whose data is
// already recorded as gone keeps its first time. It ignores run ownership.
func (s *RunStore) SetDataDeleted(ctx context.Context, runID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE runs SET data_deleted_at = NOW()
		WHERE run_id = $1 AND data_deleted_at IS NULL`, runID)
	if err != nil {
		return fmt.Errorf("set data deleted of run %s: %w", runID, err)
	}
	return nil
}

// Leases implements repository.Leases with the leases table.
type Leases struct {
	pool *pgxpool.Pool
}

// NewLeases returns the leases of cluster-wide background jobs.
func NewLeases(pool *pgxpool.Pool) *Leases {
	return &Leases{pool: pool}
}

// Claim takes the lease name for holder for ttl, if it is free, expired or
// holder's already, and reports whether holder holds it now. Expiry uses
// the database's clock, so the replicas' clocks don't matter.
func (l *Leases) Claim(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("claim lease %s: ttl must be above 0", name)
	}
	tag, err := l.pool.Exec(ctx, `
		INSERT INTO leases (name, holder, expires_at)
		VALUES ($1, $2, NOW() + make_interval(secs => $3))
		ON CONFLICT (name) DO UPDATE
			SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
			WHERE leases.expires_at <= NOW() OR leases.holder = EXCLUDED.holder`,
		name, holder, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("claim lease %s: %w", name, err)
	}
	return tag.RowsAffected() == 1, nil
}
