package tes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/repository/pg"
	"github.com/anuragxxd/conquest/internal/runnerauth"
)

func (m *memTasks) GetByTESID(ctx context.Context, id string) (*domain.Task, error) {
	for _, t := range m.tasks {
		if t.TESTaskID == id {
			return t, nil
		}
	}
	return nil, pg.ErrNotFound
}

// callbackFixture is a handler with runner auth as main wires it, two
// active runs with one task each and a finished run.
type callbackFixture struct {
	router http.Handler
	tokens *runnerauth.Tokens
	tasks  *memTasks
}

func newCallbackFixture(t *testing.T, withAuth bool) *callbackFixture {
	t.Helper()
	runs := &memRuns{runs: map[string]*domain.Run{
		"run-a":    {RunID: "run-a", State: domain.StateRunning},
		"run-b":    {RunID: "run-b", State: domain.StateRunning},
		"run-done": {RunID: "run-done", State: domain.StateComplete},
	}}
	tasks := &memTasks{tasks: map[string]*domain.Task{
		"task-a": {TaskID: "task-a", TESTaskID: "task-a", RunID: "run-a", State: domain.StateRunning},
		"task-b": {TaskID: "task-b", TESTaskID: "task-b", RunID: "run-b", State: domain.StateRunning},
	}}
	tokens, err := runnerauth.New([]byte(strings.Repeat("k", runnerauth.MinSecretBytes)))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(runs, tasks, memEvents{}, nil)
	h.SetDispatcher(&notifier{})
	if withAuth {
		h.SetRunnerAuth(tokens.Middleware(func(ctx context.Context, runID string) (bool, error) {
			run, err := runs.Get(ctx, runID)
			if errors.Is(err, pg.ErrNotFound) {
				return false, runnerauth.ErrRunNotFound
			}
			if err != nil {
				return false, err
			}
			return run.State.IsTerminal(), nil
		}))
	}
	r := chi.NewRouter()
	h.Routes(r)
	h.InternalRoutes(r)
	return &callbackFixture{router: r, tokens: tokens, tasks: tasks}
}

func (f *callbackFixture) do(method, path, token string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if method == http.MethodPost && strings.HasSuffix(path, "/tasks") {
		body = strings.NewReader(`{"executors":[{"image":"ubuntu:24.04","command":["true"]}]}`)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

// The callback rows of the gi#18 design (section 5): a runner acts on its
// own run only, with its own run's token, while the run is active.
func TestRunnerCallbacks(t *testing.T) {
	f := newCallbackFixture(t, true)
	tokA := f.tokens.Token("run-a")
	const base = "/internal/runs/"
	cases := []struct {
		name, method, path, token string
		want                      int
	}{
		{"create, own token", http.MethodPost, base + "run-a/ga4gh/tes/v1/tasks", tokA, 200},
		{"create, no token", http.MethodPost, base + "run-a/ga4gh/tes/v1/tasks", "", 401},
		{"create, other run's token", http.MethodPost, base + "run-b/ga4gh/tes/v1/tasks", tokA, 401},
		{"create, user token", http.MethodPost, base + "run-a/ga4gh/tes/v1/tasks", "eyJhbGciOiJSUzI1NiJ9.e30.c2ln", 401},
		{"create, finished run", http.MethodPost, base + "run-done/ga4gh/tes/v1/tasks", f.tokens.Token("run-done"), 401},
		{"create, missing run", http.MethodPost, base + "run-x/ga4gh/tes/v1/tasks", f.tokens.Token("run-x"), 404},
		{"get own task", http.MethodGet, base + "run-a/ga4gh/tes/v1/tasks/task-a", tokA, 200},
		{"get own task, no token", http.MethodGet, base + "run-a/ga4gh/tes/v1/tasks/task-a", "", 401},
		{"get other run's task via own run", http.MethodGet, base + "run-a/ga4gh/tes/v1/tasks/task-b", tokA, 404},
		{"get other run's task via its run", http.MethodGet, base + "run-b/ga4gh/tes/v1/tasks/task-b", tokA, 401},
		{"cancel other run's task via own run", http.MethodPost, base + "run-a/ga4gh/tes/v1/tasks/task-b:cancel", tokA, 404},
		{"service-info, own token", http.MethodGet, base + "run-a/ga4gh/tes/v1/service-info", tokA, 200},
		{"service-info alias, own token", http.MethodGet, base + "run-a/ga4gh/tes/service-info", tokA, 200},
		{"service-info, no token", http.MethodGet, base + "run-a/ga4gh/tes/v1/service-info", "", 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if w := f.do(c.method, c.path, c.token); w.Code != c.want {
				t.Fatalf("status %d, want %d: %s", w.Code, c.want, w.Body)
			}
		})
	}
	if got := f.tasks.tasks["task-b"].State; got != domain.StateRunning {
		t.Fatalf("other run's task is %s after the refused cancel", got)
	}

	// cancelling its own task works
	if w := f.do(http.MethodPost, base+"run-a/ga4gh/tes/v1/tasks/task-a:cancel", tokA); w.Code != 200 {
		t.Fatalf("own cancel: status %d: %s", w.Code, w.Body)
	}
	if got := f.tasks.tasks["task-a"].State; got != domain.StateCanceled {
		t.Fatalf("own task is %s after cancel", got)
	}
}

// Without runner auth (no CONQUEST_RUNNER_SECRET), the callbacks stay open
// as before, but tasks of other runs are still not found.
func TestRunnerCallbacksWithoutAuth(t *testing.T) {
	f := newCallbackFixture(t, false)
	if w := f.do(http.MethodPost, "/internal/runs/run-a/ga4gh/tes/v1/tasks", ""); w.Code != 200 {
		t.Fatalf("create: status %d: %s", w.Code, w.Body)
	}
	if w := f.do(http.MethodGet, "/internal/runs/run-a/ga4gh/tes/v1/tasks/task-b", ""); w.Code != 404 {
		t.Fatalf("other run's task: status %d", w.Code)
	}
	if w := f.do(http.MethodGet, "/internal/runs/run-b/ga4gh/tes/v1/tasks/task-b", ""); w.Code != 200 {
		t.Fatalf("own task: status %d", w.Code)
	}
}

// The public routes don't take runner tokens' run scoping: they keep the
// task lookup by ID (owner scoping comes from the token middleware).
func TestPublicGetTaskUnaffected(t *testing.T) {
	f := newCallbackFixture(t, true)
	if w := f.do(http.MethodGet, "/ga4gh/tes/v1/tasks/task-b", ""); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}
