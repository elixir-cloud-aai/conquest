package retention

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/repository"
	"github.com/anuragxxd/conquest/internal/runbucket"
)

var start = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

// clock is a fake clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// store holds runs in memory, as the database would.
type store struct {
	runs    map[string]*domain.Run
	listErr error
	getErr  error
}

func newStore(runs ...*domain.Run) *store {
	s := &store{runs: map[string]*domain.Run{}}
	for _, r := range runs {
		s.runs[r.RunID] = r
	}
	return s
}

func (s *store) ListExpiredRuns(_ context.Context, before time.Time, after repository.RunCursor, limit int) ([]*domain.Run, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []*domain.Run
	for _, r := range s.runs {
		if r.DataDeletedAt != nil || r.FinishedAt == nil || !r.State.IsTerminal() || !r.FinishedAt.Before(before) {
			continue
		}
		if r.FinishedAt.Before(after.FinishedAt) || (r.FinishedAt.Equal(after.FinishedAt) && r.RunID <= after.RunID) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FinishedAt.Equal(*out[j].FinishedAt) {
			return out[i].FinishedAt.Before(*out[j].FinishedAt)
		}
		return out[i].RunID < out[j].RunID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *store) SetDataDeleted(_ context.Context, runID string) error {
	if r, ok := s.runs[runID]; ok && r.DataDeletedAt == nil {
		now := start
		r.DataDeletedAt = &now
	}
	return nil
}

func (s *store) Get(_ context.Context, runID string) (*domain.Run, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	r, ok := s.runs[runID]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return r, nil
}

// leases is a lease table shared by the replicas of a test.
type leases struct {
	mu      sync.Mutex
	clock   *clock
	holders map[string]string
	expires map[string]time.Time
	err     error
}

func newLeases(c *clock) *leases {
	return &leases{clock: c, holders: map[string]string{}, expires: map[string]time.Time{}}
}

func (l *leases) Claim(_ context.Context, name, holder string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return false, l.err
	}
	now := l.clock.Now()
	if h, ok := l.holders[name]; ok && h != holder && now.Before(l.expires[name]) {
		return false, nil
	}
	l.holders[name], l.expires[name] = holder, now.Add(ttl)
	return true, nil
}

// data records the runs whose data was deleted.
type data struct {
	mu       sync.Mutex
	deleted  []string
	fail     map[string]error
	unplaced map[string]bool // runs whose data CheckRunData can't place
}

func (d *data) CheckRunData(run *domain.Run) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.unplaced[run.RunID] {
		return errors.New("can't place the run's data")
	}
	return nil
}

func (d *data) DeleteRunData(_ context.Context, run *domain.Run) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.fail[run.RunID]; err != nil {
		return err
	}
	d.deleted = append(d.deleted, run.RunID)
	return nil
}

// cluster records the Kubernetes clean-up of runs.
type cluster struct {
	jobsDeleted []string
	cleaned     []string
	jobsErr     error
}

func (c *cluster) DeleteRunJobs(_ context.Context, runID string) error {
	c.jobsDeleted = append(c.jobsDeleted, runID)
	return c.jobsErr
}

func (c *cluster) Cleanup(_ context.Context, w *domain.WorkspaceBinding) error {
	c.cleaned = append(c.cleaned, w.ClaimName)
	return nil
}

// claims is a fake per-run provisioner with claims of runs.
type claims struct {
	runbucket.Provisioner
	owned   []runbucket.OwnedBucket
	deleted []string
	listErr error
}

func (c *claims) ListRunBuckets(context.Context) ([]runbucket.OwnedBucket, error) {
	return c.owned, c.listErr
}

func (c *claims) Delete(_ context.Context, runID string) error {
	c.deleted = append(c.deleted, runID)
	return nil
}

func finished(id string, state domain.State, at time.Time) *domain.Run {
	return &domain.Run{RunID: id, State: state, FinishedAt: &at}
}

func newSweeper(t *testing.T, st *store, ls *leases, d *data, c *clock, holder string) *Sweeper {
	t.Helper()
	s, err := New(st, ls, d, Config{
		Retention: 720 * time.Hour,
		Interval:  15 * time.Minute,
		BatchSize: 2,
		Holder:    holder,
		Now:       c.Now,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestSweep_DeletesTheDataOfRunsTerminalForLongerThanTheRetention(t *testing.T) {
	c := &clock{now: start}
	st := newStore(
		finished("old-complete", domain.StateComplete, start.Add(-721*time.Hour)),
		finished("old-error", domain.StateExecutorError, start.Add(-800*time.Hour)),
		finished("old-canceled", domain.StateCanceled, start.Add(-900*time.Hour)),
		finished("recent", domain.StateComplete, start.Add(-719*time.Hour)),
		&domain.Run{RunID: "running", State: domain.StateRunning},
	)
	d := &data{}
	s := newSweeper(t, st, newLeases(c), d, c, "a")

	got := s.SweepOnce(context.Background())
	if !got.Swept || got.Deleted != 3 || got.Failed != 0 {
		t.Fatalf("SweepOnce = %+v, want 3 runs deleted", got)
	}
	// oldest first, across batches
	if want := []string{"old-canceled", "old-error", "old-complete"}; !slices.Equal(d.deleted, want) {
		t.Fatalf("deleted %v, want %v", d.deleted, want)
	}
	for _, id := range []string{"old-complete", "old-error", "old-canceled"} {
		if st.runs[id].DataDeletedAt == nil {
			t.Errorf("run %s: data_deleted_at not set", id)
		}
	}
	for _, id := range []string{"recent", "running"} {
		if st.runs[id].DataDeletedAt != nil {
			t.Errorf("run %s: data deleted too early", id)
		}
	}

	// once the retention passes for the recent run, the next sweep takes it
	c.Advance(2 * time.Hour)
	got = s.SweepOnce(context.Background())
	if got.Deleted != 1 || d.deleted[len(d.deleted)-1] != "recent" {
		t.Fatalf("second sweep = %+v, deleted %v; want the recent run deleted", got, d.deleted)
	}
}

func TestSweep_ARunWhoseDataCantBePlacedCostsNoClusterCalls(t *testing.T) {
	c := &clock{now: start}
	stuck := finished("stuck", domain.StateComplete, start.Add(-900*time.Hour))
	stuck.Workspace = &domain.WorkspaceBinding{Kind: domain.WorkspaceKindPVC, ClaimName: "ws-stuck"}
	st := newStore(stuck)
	d := &data{unplaced: map[string]bool{"stuck": true}}
	s := newSweeper(t, st, newLeases(c), d, c, "a")
	k8s := &cluster{}
	s.SetJobs(k8s)
	s.SetWorkspace(k8s)

	if got := s.SweepOnce(context.Background()); got.Failed != 1 || got.Deleted != 0 {
		t.Fatalf("SweepOnce = %+v, want 1 failed", got)
	}
	if len(k8s.jobsDeleted) != 0 || len(k8s.cleaned) != 0 || len(d.deleted) != 0 {
		t.Fatalf("jobs %v, workspaces %v, data %v: want no calls", k8s.jobsDeleted, k8s.cleaned, d.deleted)
	}
	if st.runs["stuck"].DataDeletedAt != nil {
		t.Fatal("run whose data can't be placed is recorded as deleted")
	}
}

func TestSweep_ARunWhoseDeletionFailsIsKeptForTheNextSweep(t *testing.T) {
	c := &clock{now: start}
	st := newStore(
		finished("fails", domain.StateComplete, start.Add(-800*time.Hour)),
		finished("works", domain.StateComplete, start.Add(-750*time.Hour)),
	)
	d := &data{fail: map[string]error{"fails": errors.New("object store down")}}
	s := newSweeper(t, st, newLeases(c), d, c, "a")

	got := s.SweepOnce(context.Background())
	if got.Deleted != 1 || got.Failed != 1 {
		t.Fatalf("SweepOnce = %+v, want 1 deleted and 1 failed", got)
	}
	if st.runs["fails"].DataDeletedAt != nil {
		t.Fatal("run whose deletion failed is recorded as deleted")
	}
	if st.runs["works"].DataDeletedAt == nil {
		t.Fatal("a failing run held up the others")
	}

	delete(d.fail, "fails")
	c.Advance(15 * time.Minute)
	if got := s.SweepOnce(context.Background()); got.Deleted != 1 || st.runs["fails"].DataDeletedAt == nil {
		t.Fatalf("retry = %+v, want the failed run deleted", got)
	}
}

func TestSweep_DeletesTheRemainingJobsAndWorkspaceFirst(t *testing.T) {
	c := &clock{now: start}
	pvc := finished("pvc", domain.StateComplete, start.Add(-800*time.Hour))
	pvc.Workspace = &domain.WorkspaceBinding{Kind: domain.WorkspaceKindPVC, ClaimName: "ws-pvc"}
	st := newStore(pvc, finished("emptydir", domain.StateComplete, start.Add(-800*time.Hour)))
	k8s := &cluster{}
	s := newSweeper(t, st, newLeases(c), &data{}, c, "a")
	s.SetJobs(k8s)
	s.SetWorkspace(k8s)

	if got := s.SweepOnce(context.Background()); got.Deleted != 2 {
		t.Fatalf("SweepOnce = %+v", got)
	}
	sort.Strings(k8s.jobsDeleted)
	if !slices.Equal(k8s.jobsDeleted, []string{"emptydir", "pvc"}) {
		t.Errorf("jobs deleted of %v, want both runs", k8s.jobsDeleted)
	}
	if !slices.Equal(k8s.cleaned, []string{"ws-pvc"}) {
		t.Errorf("workspaces cleaned %v, want [ws-pvc]", k8s.cleaned)
	}

	// a failure keeps the run for the next sweep
	st = newStore(finished("x", domain.StateComplete, start.Add(-800*time.Hour)))
	k8s = &cluster{jobsErr: errors.New("API down")}
	d := &data{}
	s = newSweeper(t, st, newLeases(c), d, c, "a")
	s.SetJobs(k8s)
	if got := s.SweepOnce(context.Background()); got.Failed != 1 || st.runs["x"].DataDeletedAt != nil || len(d.deleted) != 0 {
		t.Fatalf("SweepOnce = %+v, data deleted %v; want the run kept", got, d.deleted)
	}
}

func TestSweep_OneReplicaAtATime(t *testing.T) {
	c := &clock{now: start}
	st := newStore(finished("old", domain.StateComplete, start.Add(-800*time.Hour)))
	ls := newLeases(c)
	d := &data{}
	a := newSweeper(t, st, ls, d, c, "a")
	b := newSweeper(t, st, ls, d, c, "b")

	if got := a.SweepOnce(context.Background()); !got.Swept {
		t.Fatal("replica a didn't sweep")
	}
	st.runs["new-old"] = finished("new-old", domain.StateComplete, start.Add(-800*time.Hour))
	if got := b.SweepOnce(context.Background()); got.Swept {
		t.Fatal("replica b swept while a holds the lease")
	}
	if st.runs["new-old"].DataDeletedAt != nil {
		t.Fatal("replica b deleted data while a holds the lease")
	}
	// the lease expires after the interval: b takes over (e.g. a is gone)
	c.Advance(15 * time.Minute)
	if got := b.SweepOnce(context.Background()); !got.Swept || got.Deleted != 1 {
		t.Fatalf("replica b after expiry = %+v", got)
	}
}

func TestSweep_LeaseErrorsSkipTheSweep(t *testing.T) {
	c := &clock{now: start}
	st := newStore(finished("old", domain.StateComplete, start.Add(-800*time.Hour)))
	ls := newLeases(c)
	ls.err = errors.New("db down")
	d := &data{}
	if got := newSweeper(t, st, ls, d, c, "a").SweepOnce(context.Background()); got.Swept || len(d.deleted) != 0 {
		t.Fatalf("SweepOnce = %+v, deleted %v; want no sweep without the lease", got, d.deleted)
	}
}

func TestSweep_DeletesOrphanedBucketsAfterTheGracePeriod(t *testing.T) {
	c := &clock{now: start}
	gone := start.Add(-time.Hour)
	withData := finished("stored", domain.StateRunning, start)
	dataGone := finished("data-gone", domain.StateComplete, start.Add(-800*time.Hour))
	dataGone.DataDeletedAt = &gone
	st := newStore(withData, dataGone)
	cl := &claims{owned: []runbucket.OwnedBucket{
		{RunID: "stored", Created: start.Add(-2 * time.Hour)},          // its run exists: kept
		{RunID: "data-gone", Created: start.Add(-900 * time.Hour)},     // its run's data is gone: deleted
		{RunID: "never-stored", Created: start.Add(-2 * time.Hour)},    // no run, older than the grace: deleted
		{RunID: "being-stored", Created: start.Add(-10 * time.Minute)}, // no run yet, within the grace: kept
	}}
	s := newSweeper(t, st, newLeases(c), &data{}, c, "a")
	s.SetOrphans(cl)

	got := s.SweepOnce(context.Background())
	sort.Strings(cl.deleted)
	if want := []string{"data-gone", "never-stored"}; !slices.Equal(cl.deleted, want) {
		t.Fatalf("orphans deleted %v, want %v", cl.deleted, want)
	}
	if got.Orphans != 2 {
		t.Fatalf("SweepOnce = %+v, want 2 orphans", got)
	}
}

func TestSweep_OrphanSweepKeepsBucketsWhenItCantReadTheRuns(t *testing.T) {
	c := &clock{now: start}
	st := newStore()
	st.getErr = errors.New("db down")
	cl := &claims{owned: []runbucket.OwnedBucket{{RunID: "x", Created: start.Add(-48 * time.Hour)}}}
	s := newSweeper(t, st, newLeases(c), &data{}, c, "a")
	s.SetOrphans(cl)
	s.SweepOnce(context.Background())
	if len(cl.deleted) != 0 {
		t.Fatalf("deleted %v although the runs couldn't be read", cl.deleted)
	}

	cl.listErr = errors.New("forbidden")
	st.getErr = nil
	c.Advance(15 * time.Minute)
	s.SweepOnce(context.Background()) // logs, doesn't panic
	if len(cl.deleted) != 0 {
		t.Fatalf("deleted %v", cl.deleted)
	}
}

// A sweep whose context ends (shutdown, or its own deadline before the
// lease expires) deletes no further runs.
func TestSweep_StopsWithTheContext(t *testing.T) {
	c := &clock{now: start}
	st := newStore(finished("old", domain.StateComplete, start.Add(-800*time.Hour)))
	d := &data{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := newSweeper(t, st, newLeases(c), d, c, "a").SweepOnce(ctx)
	if got.Deleted != 0 || len(d.deleted) != 0 || st.runs["old"].DataDeletedAt != nil {
		t.Fatalf("SweepOnce = %+v, deleted %v; want nothing after the context ended", got, d.deleted)
	}
}

func TestSweep_ListErrorsEndTheSweep(t *testing.T) {
	c := &clock{now: start}
	st := newStore()
	st.listErr = errors.New("db down")
	got := newSweeper(t, st, newLeases(c), &data{}, c, "a").SweepOnce(context.Background())
	if !got.Swept || got.Deleted != 0 {
		t.Fatalf("SweepOnce = %+v", got)
	}
}

func TestNew_ValidatesItsConfig(t *testing.T) {
	c := &clock{now: start}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, ls, d := newStore(), newLeases(c), &data{}
	for name, tc := range map[string]struct {
		st  repository.RetentionStore
		ls  repository.Leases
		d   RunData
		cfg Config
	}{
		"no retention": {st, ls, d, Config{}},
		"no store":     {nil, ls, d, Config{Retention: time.Hour}},
		"no leases":    {st, nil, d, Config{Retention: time.Hour}},
		"no data":      {st, ls, nil, Config{Retention: time.Hour}},
	} {
		if _, err := New(tc.st, tc.ls, tc.d, tc.cfg, log); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	s, err := New(st, ls, d, Config{Retention: time.Hour}, log)
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.Interval != DefaultInterval || s.cfg.BatchSize <= 0 || s.cfg.OrphanGrace != DefaultOrphanGrace || s.cfg.Holder == "" || s.cfg.Now == nil {
		t.Fatalf("defaults = %+v", s.cfg)
	}
}

func TestRun_SweepsAtStartAndStopsWithTheContext(t *testing.T) {
	c := &clock{now: start}
	st := newStore(finished("old", domain.StateComplete, start.Add(-800*time.Hour)))
	d := &data{}
	s := newSweeper(t, st, newLeases(c), d, c, "a")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		n := len(d.deleted)
		d.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run didn't sweep at start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't stop with its context")
	}
}
