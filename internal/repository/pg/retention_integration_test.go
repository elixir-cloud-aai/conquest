//go:build integration

package pg

import (
	"context"
	"testing"
	"time"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/repository"
)

// expiredIDs lists the IDs of all expired runs (finished before cutoff) in
// pages of two, through the keyset cursor.
func expiredIDs(t *testing.T, runs *RunStore, cutoff time.Time) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	var after repository.RunCursor
	for {
		page, err := runs.ListExpiredRuns(context.Background(), cutoff, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			return ids
		}
		for _, r := range page {
			if ids[r.RunID] {
				t.Fatalf("run %s listed twice", r.RunID)
			}
			ids[r.RunID] = true
		}
		last := page[len(page)-1]
		after = repository.RunCursor{FinishedAt: *last.FinishedAt, RunID: last.RunID}
	}
}

func TestRetentionStore(t *testing.T) {
	ctx := context.Background()
	runs, _ := testStores(t)
	newRun := func(to domain.State) *domain.Run {
		t.Helper()
		r := domain.NewRun("NFL", "DSL2", "nextflow", "25.10.4", []byte(`{}`), "s3://b/o/", "v1", "s3://b/r/")
		if err := runs.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		if to != domain.StateQueued {
			if err := runs.SetError(ctx, r.RunID, to, "test"); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	done1, done2, done3 := newRun(domain.StateSystemError), newRun(domain.StateExecutorError), newRun(domain.StateSystemError)
	active := newRun(domain.StateQueued)

	cutoff := time.Now().Add(time.Hour) // every finished run is past it
	got := expiredIDs(t, runs, cutoff)
	for _, r := range []*domain.Run{done1, done2, done3} {
		if !got[r.RunID] {
			t.Errorf("terminal run %s not listed", r.RunID)
		}
	}
	if got[active.RunID] {
		t.Error("active run listed")
	}
	if got := expiredIDs(t, runs, time.Now().Add(-time.Hour)); got[done1.RunID] {
		t.Error("run that finished after the cutoff listed")
	}

	// Recording the deletion takes the run off the list, once.
	if err := runs.SetDataDeleted(ctx, done1.RunID); err != nil {
		t.Fatal(err)
	}
	first, err := runs.Get(ctx, done1.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if first.DataDeletedAt == nil {
		t.Fatal("data_deleted_at not set")
	}
	if err := runs.SetDataDeleted(ctx, done1.RunID); err != nil {
		t.Fatal(err)
	}
	again, err := runs.Get(ctx, done1.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.DataDeletedAt.Equal(*first.DataDeletedAt) {
		t.Errorf("data_deleted_at moved from %s to %s", first.DataDeletedAt, again.DataDeletedAt)
	}
	if got := expiredIDs(t, runs, cutoff); got[done1.RunID] {
		t.Error("run whose data is gone listed")
	}
}

func TestLeases(t *testing.T) {
	ctx := context.Background()
	runs, _ := testStores(t)
	leases := NewLeases(runs.pool)
	name := "test-" + domain.NewID()

	claim := func(holder string, ttl time.Duration) bool {
		t.Helper()
		ok, err := leases.Claim(ctx, name, holder, ttl)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !claim("a", time.Hour) {
		t.Fatal("a didn't get a free lease")
	}
	if claim("b", time.Hour) {
		t.Fatal("b got a's live lease")
	}
	if !claim("a", 50*time.Millisecond) {
		t.Fatal("a couldn't renew its lease")
	}
	time.Sleep(100 * time.Millisecond)
	if !claim("b", time.Hour) {
		t.Fatal("b didn't get the expired lease")
	}
	if claim("a", time.Hour) {
		t.Fatal("a got b's live lease")
	}
	if _, err := leases.Claim(ctx, name, "a", 0); err == nil {
		t.Fatal("ttl 0 accepted")
	}
}
