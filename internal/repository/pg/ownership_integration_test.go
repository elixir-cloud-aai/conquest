//go:build integration

package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/ownership"
	"github.com/anuragxxd/conquest/internal/repository"
)

func newTestRun() *domain.Run {
	return domain.NewRun("NFL", "DSL2", "nextflow", "25.10.4", []byte(`{}`), "s3://b/o/", "v1", "s3://b/r/")
}

// createTestTask stores a task of runID and cancels it, so that it never
// reaches the dispatch queue that TestDispatchQueue inspects.
func createTestTask(t *testing.T, tasks *TaskStore, runID string) *domain.Task {
	t.Helper()
	ctx := context.Background()
	task := domain.NewTask(runID)
	task.TESTaskID = task.TaskID
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.UpdateState(ctx, task.TaskID, domain.StateQueued, domain.StateCanceled); err != nil {
		t.Fatal(err)
	}
	return task
}

// ownerPrefix keeps owners unique per test run, so that the tests work on
// a database that already holds runs.
var ownerPrefix = "test|" + domain.NewID() + "|"

func TestOwnership_CreateStampsOwner(t *testing.T) {
	ctx := context.Background()
	runs, _ := testStores(t)
	alice := ownership.NewContext(ctx, ownership.Owner{ID: ownerPrefix + "alice", Context: []byte(`{"client_id": "web"}`)})

	run := newTestRun()
	run.Owner = "ignored" // the scope wins over what the caller set
	if err := runs.Create(alice, run); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != ownerPrefix+"alice" {
		t.Fatalf("owner = %q, want %q", got.Owner, ownerPrefix+"alice")
	}
	if string(got.AuthContext) != `{"client_id": "web"}` {
		t.Fatalf("auth_context = %s", got.AuthContext)
	}

	unowned := newTestRun()
	if err := runs.Create(ctx, unowned); err != nil {
		t.Fatal(err)
	}
	got, err = runs.Get(ctx, unowned.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "" || got.AuthContext != nil {
		t.Fatalf("unscoped run got owner %q, auth_context %s", got.Owner, got.AuthContext)
	}
}

func TestOwnership_ScopedReads(t *testing.T) {
	ctx := context.Background()
	runs, tasks := testStores(t)
	alice := ownership.NewContext(ctx, ownership.Owner{ID: ownerPrefix + "alice"})
	bob := ownership.NewContext(ctx, ownership.Owner{ID: ownerPrefix + "bob"})
	nobody := ownership.NewContext(ctx, ownership.Owner{})

	own := newTestRun()
	if err := runs.Create(alice, own); err != nil {
		t.Fatal(err)
	}
	legacy := newTestRun() // created without authentication: owner NULL
	if err := runs.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	ownTask := createTestTask(t, tasks, own.RunID)
	legacyTask := createTestTask(t, tasks, legacy.RunID)

	type access struct {
		name   string
		ctx    context.Context
		run    *domain.Run
		task   *domain.Task
		wantOK bool
	}
	cases := []access{
		{"owner", alice, own, ownTask, true},
		{"other owner", bob, own, ownTask, false},
		{"empty owner", nobody, own, ownTask, false},
		{"owner on legacy run", alice, legacy, legacyTask, false},
		{"empty owner on legacy run", nobody, legacy, legacyTask, false},
		{"unscoped", ctx, own, ownTask, true},
		{"unscoped on legacy run", ctx, legacy, legacyTask, true},
	}
	check := func(c access, what string, err error) {
		t.Helper()
		if c.wantOK && err != nil {
			t.Errorf("%s: %s: %v", c.name, what, err)
		}
		if !c.wantOK && !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %s: err = %v, want ErrNotFound", c.name, what, err)
		}
	}
	for _, c := range cases {
		_, err := runs.Get(c.ctx, c.run.RunID)
		check(c, "Get", err)
		_, err = runs.GetStatus(c.ctx, c.run.RunID)
		check(c, "GetStatus", err)
		_, err = tasks.Get(c.ctx, c.task.TaskID)
		check(c, "task Get", err)
		_, err = tasks.GetByTESID(c.ctx, c.task.TESTaskID)
		check(c, "task GetByTESID", err)

		page, err := tasks.ListByRunID(c.ctx, c.run.RunID, repository.Page{Size: 10})
		if err != nil {
			t.Fatalf("%s: ListByRunID: %v", c.name, err)
		}
		if got := len(page.Items); (got == 1) != c.wantOK {
			t.Errorf("%s: ListByRunID returned %d tasks", c.name, got)
		}
	}

	// Writes stay unscoped: the state change goes through for internal callers.
	if err := runs.UpdateState(ctx, own.RunID, domain.StateQueued, domain.StateInitializing); err != nil {
		t.Fatal(err)
	}
}

func TestOwnership_ListPagesPerOwner(t *testing.T) {
	ctx := context.Background()
	runs, tasks := testStores(t)
	owner := ownerPrefix + "lister"
	scoped := ownership.NewContext(ctx, ownership.Owner{ID: owner})
	other := ownership.NewContext(ctx, ownership.Owner{ID: ownerPrefix + "other"})

	want := map[string]bool{}
	wantTasks := map[string]bool{}
	for i := 0; i < 5; i++ {
		run := newTestRun()
		if err := runs.Create(scoped, run); err != nil {
			t.Fatal(err)
		}
		want[run.RunID] = true
		task := createTestTask(t, tasks, run.RunID)
		wantTasks[task.TaskID] = true

		// interleave someone else's and an ownerless run
		for _, c := range []context.Context{other, ctx} {
			if err := runs.Create(c, newTestRun()); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(time.Millisecond) // distinct created_at for a stable keyset
	}

	got := map[string]bool{}
	token := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("too many pages")
		}
		page, err := runs.List(scoped, repository.Page{Size: 2, Token: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Items {
			if r.Owner != owner {
				t.Fatalf("listed run %s of owner %q", r.RunID, r.Owner)
			}
			got[r.RunID] = true
		}
		if token = page.NextToken; token == "" {
			break
		}
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d runs, want %d", len(got), len(want))
	}

	gotTasks := map[string]bool{}
	token = ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("too many pages")
		}
		page, err := tasks.List(scoped, repository.Page{Size: 2, Token: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range page.Items {
			if !wantTasks[task.TaskID] {
				t.Fatalf("listed foreign task %s", task.TaskID)
			}
			gotTasks[task.TaskID] = true
		}
		if token = page.NextToken; token == "" {
			break
		}
	}
	if len(gotTasks) != len(wantTasks) {
		t.Fatalf("listed %d tasks, want %d", len(gotTasks), len(wantTasks))
	}

	// An owner without runs gets an empty first page.
	empty, err := runs.List(ownership.NewContext(ctx, ownership.Owner{ID: ownerPrefix + "none"}), repository.Page{Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Items) != 0 || empty.NextToken != "" {
		t.Fatalf("owner without runs listed %d runs, next %q", len(empty.Items), empty.NextToken)
	}
}

func TestOwnership_ListByRunIDPages(t *testing.T) {
	ctx := context.Background()
	runs, tasks := testStores(t)
	owner := ownership.NewContext(ctx, ownership.Owner{ID: ownerPrefix + "pager"})
	other := ownership.NewContext(ctx, ownership.Owner{ID: ownerPrefix + "pager-other"})

	run := newTestRun()
	if err := runs.Create(owner, run); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		createTestTask(t, tasks, run.RunID)
		time.Sleep(time.Millisecond)
	}

	count := func(c context.Context) int {
		t.Helper()
		n, token := 0, ""
		for pages := 0; ; pages++ {
			if pages > 5 {
				t.Fatal("too many pages")
			}
			page, err := tasks.ListByRunID(c, run.RunID, repository.Page{Size: 2, Token: token})
			if err != nil {
				t.Fatal(err)
			}
			n += len(page.Items)
			if token = page.NextToken; token == "" {
				return n
			}
		}
	}
	if n := count(owner); n != 5 {
		t.Fatalf("owner paged %d tasks, want 5", n)
	}
	if n := count(ctx); n != 5 {
		t.Fatalf("unscoped paged %d tasks, want 5", n)
	}
	if n := count(other); n != 0 {
		t.Fatalf("other owner paged %d tasks, want 0", n)
	}
}
