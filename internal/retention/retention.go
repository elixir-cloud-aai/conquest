// Package retention deletes the data of workflow runs once they have been
// in a terminal state for longer than the retention period
// (CONQUEST_RUN_RETENTION), so that per-run buckets and objects don't pile
// up.
//
// A Sweeper runs on every server replica; a lease in the database lets one
// replica at a time sweep. Each sweep takes the terminal runs that finished
// before now minus the retention period and still have their data, oldest
// first, and for each run deletes its remaining Jobs and workspace (PVC,
// PV), then its data (runbucket.DataDeleter: the objects below its run root
// in the configured bucket, or its own bucket), and records the time in
// data_deleted_at. The run itself stays: it still lists, and the WES API
// answers that its logs and outputs are gone. A run whose deletion fails
// stays as it is and is tried again in the next sweep.
//
// With a provisioner that lists the buckets it made (obc), a sweep also
// deletes orphaned buckets: those older than the grace period whose run
// was never stored (a submission that failed after the bucket was made)
// or whose run's data is recorded as deleted already.
package retention

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/repository"
	"github.com/anuragxxd/conquest/internal/runbucket"
)

// LeaseName is the lease that lets one replica at a time sweep.
const LeaseName = "run-retention"

// Defaults of Config.
const (
	DefaultInterval    = 15 * time.Minute
	DefaultBatchSize   = 100
	DefaultOrphanGrace = time.Hour
)

// RunData deletes a run's data; runbucket.DataDeleter satisfies it.
type RunData interface {
	DeleteRunData(ctx context.Context, run *domain.Run) error
}

// Jobs deletes a run's remaining Jobs; backend.RunBackend satisfies it.
type Jobs interface {
	DeleteRunJobs(ctx context.Context, runID string) error
}

// Workspace deletes a run's workspace (PVC, PV); the k8s backend's
// workspace strategies satisfy it.
type Workspace interface {
	Cleanup(ctx context.Context, binding *domain.WorkspaceBinding) error
}

// Orphans lists the buckets a provisioner made and deletes them;
// runbucket.BucketLister satisfies it.
type Orphans interface {
	ListRunBuckets(ctx context.Context) ([]runbucket.OwnedBucket, error)
	Delete(ctx context.Context, runID string) error
}

// Config configures a Sweeper. The server sets only Retention
// (CONQUEST_RUN_RETENTION); it uses the defaults of the rest, which the
// README documents.
type Config struct {
	// Retention is how long a run's data stays after the run ended
	// (required, above 0).
	Retention time.Duration
	// Interval is the time between sweeps, and the lease's (default
	// DefaultInterval).
	Interval time.Duration
	// BatchSize is how many runs a sweep reads at a time (default
	// DefaultBatchSize).
	BatchSize int
	// OrphanGrace is how old a bucket without a run must be before it is
	// deleted (default DefaultOrphanGrace): a submission stores its run
	// only after the bucket is made.
	OrphanGrace time.Duration
	// Holder names this replica in the lease (default: host name and a
	// random suffix).
	Holder string
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// Sweeper deletes the data of expired runs.
type Sweeper struct {
	store     repository.RetentionStore
	leases    repository.Leases
	data      RunData
	jobs      Jobs
	workspace Workspace
	orphans   Orphans
	cfg       Config
	log       *slog.Logger
}

// New returns a Sweeper that finds runs in store, takes the lease in leases
// and deletes runs' data with data.
func New(store repository.RetentionStore, leases repository.Leases, data RunData, cfg Config, log *slog.Logger) (*Sweeper, error) {
	switch {
	case cfg.Retention <= 0:
		return nil, errors.New("run retention: the retention period must be above 0")
	case store == nil || leases == nil || data == nil:
		return nil, errors.New("run retention: run store, leases and run data are required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.OrphanGrace <= 0 {
		cfg.OrphanGrace = DefaultOrphanGrace
	}
	if cfg.Holder == "" {
		cfg.Holder = holderName()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Sweeper{store: store, leases: leases, data: data, cfg: cfg, log: log}, nil
}

// holderName returns the host name (the pod's) and a random suffix.
func holderName() string {
	host, _ := os.Hostname()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return host + "-" + hex.EncodeToString(b)
}

// SetJobs makes sweeps delete a run's remaining Jobs first.
func (s *Sweeper) SetJobs(j Jobs) { s.jobs = j }

// SetWorkspace makes sweeps delete a run's remaining workspace first.
func (s *Sweeper) SetWorkspace(w Workspace) { s.workspace = w }

// SetOrphans makes sweeps delete orphaned buckets.
func (s *Sweeper) SetOrphans(o Orphans) { s.orphans = o }

// Run sweeps at once and then every interval until ctx is done.
func (s *Sweeper) Run(ctx context.Context) {
	s.log.Info("run retention started", "retention", s.cfg.Retention, "interval", s.cfg.Interval)
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		s.SweepOnce(ctx)
		select {
		case <-ctx.Done():
			s.log.Info("run retention stopped")
			return
		case <-ticker.C:
		}
	}
}

// Result says what a sweep did.
type Result struct {
	// Swept is false if another replica held the lease, or it couldn't be
	// taken.
	Swept bool
	// Deleted and Failed count runs whose data was deleted, and runs whose
	// deletion failed.
	Deleted, Failed int
	// Orphans counts deleted orphaned buckets.
	Orphans int
}

// SweepOnce sweeps if this replica gets the lease. A sweep ends before the
// lease does.
func (s *Sweeper) SweepOnce(ctx context.Context) Result {
	held, err := s.leases.Claim(ctx, LeaseName, s.cfg.Holder, s.cfg.Interval)
	if err != nil {
		s.log.Error("run retention: lease not taken", "err", err)
		return Result{}
	}
	if !held {
		return Result{}
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Interval*9/10)
	defer cancel()
	res := Result{Swept: true}
	s.sweepRuns(ctx, &res)
	if s.orphans != nil {
		s.sweepOrphans(ctx, &res)
	}
	if res.Deleted > 0 || res.Failed > 0 || res.Orphans > 0 {
		s.log.Info("run retention sweep", "deleted", res.Deleted, "failed", res.Failed, "orphaned_buckets_deleted", res.Orphans)
	}
	return res
}

// sweepRuns deletes the data of every expired run, batch by batch.
func (s *Sweeper) sweepRuns(ctx context.Context, res *Result) {
	before := s.cfg.Now().Add(-s.cfg.Retention)
	var after repository.RunCursor
	for ctx.Err() == nil {
		runs, err := s.store.ListExpiredRuns(ctx, before, after, s.cfg.BatchSize)
		if err != nil {
			s.log.Error("run retention: list expired runs", "err", err)
			return
		}
		for _, run := range runs {
			if ctx.Err() != nil {
				return
			}
			if err := s.deleteRun(ctx, run); err != nil {
				res.Failed++
				s.log.Error("run retention: run data not deleted; trying again in the next sweep", "run_id", run.RunID, "err", err)
				continue
			}
			res.Deleted++
			s.log.Info("run data deleted after the retention period", "run_id", run.RunID, "finished_at", run.FinishedAt)
		}
		if len(runs) < s.cfg.BatchSize {
			return
		}
		last := runs[len(runs)-1]
		if last.FinishedAt == nil { // the store broke its contract: stop
			s.log.Error("run retention: expired run without finished_at", "run_id", last.RunID)
			return
		}
		after = repository.RunCursor{FinishedAt: *last.FinishedAt, RunID: last.RunID}
	}
}

// deleteRun deletes the run's remaining Jobs, its workspace and its data,
// and records that the data is gone. Every step is idempotent, so a failed
// run is simply tried again.
func (s *Sweeper) deleteRun(ctx context.Context, run *domain.Run) error {
	// Jobs first: their pods hold the workspace PVC
	if s.jobs != nil {
		if err := s.jobs.DeleteRunJobs(ctx, run.RunID); err != nil {
			return fmt.Errorf("delete jobs: %w", err)
		}
	}
	if s.workspace != nil && run.Workspace != nil && run.Workspace.RequiresCleanup() {
		if err := s.workspace.Cleanup(ctx, run.Workspace); err != nil {
			return fmt.Errorf("delete workspace: %w", err)
		}
	}
	if err := s.data.DeleteRunData(ctx, run); err != nil {
		return fmt.Errorf("delete data: %w", err)
	}
	if err := s.store.SetDataDeleted(ctx, run.RunID); err != nil {
		return fmt.Errorf("record deletion: %w", err)
	}
	return nil
}

// sweepOrphans deletes the buckets older than the grace period whose run
// was never stored or whose data is recorded as deleted.
func (s *Sweeper) sweepOrphans(ctx context.Context, res *Result) {
	owned, err := s.orphans.ListRunBuckets(ctx)
	if err != nil {
		s.log.Error("run retention: list run buckets", "err", err)
		return
	}
	cutoff := s.cfg.Now().Add(-s.cfg.OrphanGrace)
	for _, b := range owned {
		if ctx.Err() != nil {
			return
		}
		if !b.Created.Before(cutoff) {
			continue // its submission may still be storing the run
		}
		run, getErr := s.store.Get(ctx, b.RunID)
		switch {
		case errors.Is(getErr, repository.ErrNotFound):
		case getErr != nil:
			s.log.Error("run retention: read the run of a bucket", "run_id", b.RunID, "err", getErr)
			continue
		case run.DataDeletedAt == nil:
			continue // the run's own bucket
		}
		if err := s.orphans.Delete(ctx, b.RunID); err != nil {
			s.log.Error("run retention: orphaned bucket not deleted", "run_id", b.RunID, "err", err)
			continue
		}
		res.Orphans++
		s.log.Info("orphaned run bucket deleted", "run_id", b.RunID, "created", b.Created, "run_stored", getErr == nil)
	}
}
