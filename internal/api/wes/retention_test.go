package wes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/anuragxxd/conquest/internal/adapter"
	"github.com/anuragxxd/conquest/internal/controller"
	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/repository"
	"github.com/anuragxxd/conquest/internal/runbucket"
	"github.com/anuragxxd/conquest/internal/storage"
)

var dataDeletedAt = time.Date(2026, 12, 27, 10, 0, 0, 0, time.UTC)

// expiredRun is a WDL run whose data was deleted after the retention
// period.
func expiredRun(runID string) *domain.Run {
	finished := dataDeletedAt.Add(-31 * 24 * time.Hour)
	return &domain.Run{
		RunID: runID, State: domain.StateComplete, WorkflowEngine: "wdl",
		RunRootURI: "s3://run-" + runID + "/v1/workflow-runs/" + runID + "/",
		FinishedAt: &finished, DataDeletedAt: &dataDeletedAt,
	}
}

type expiredRuns struct{ wesRunStoreStub }

func (expiredRuns) Get(_ context.Context, runID string) (*domain.Run, error) {
	return expiredRun(runID), nil
}

func (expiredRuns) List(context.Context, repository.Page) (*repository.PageResult[*domain.Run], error) {
	return &repository.PageResult[*domain.Run]{Items: []*domain.Run{expiredRun("a")}}, nil
}

// unreadableRuns read run states but not runs, as a database that fails
// between two queries would.
type unreadableRuns struct{ wesRunStoreStub }

func (unreadableRuns) Get(context.Context, string) (*domain.Run, error) {
	return nil, errors.New("database down")
}

// Task logs of a run that can't be read name no log URIs that may point to
// deleted objects: the request fails instead.
func TestTaskLogsFailWhenTheirRunCantBeRead(t *testing.T) {
	ctrl := controller.NewRunController(&unreadableRuns{}, &tasksOfRun{}, &wesEventStoreStub{},
		storage.Layout{Bucket: "conquest", Version: "v1"}, adapter.NewRegistry(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	NewHandler(ctrl).Routes(r)
	for _, path := range []string{"/ga4gh/wes/v1/runs/a/tasks", "/ga4gh/wes/v1/runs/a/tasks/t1"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "s3://") {
			t.Errorf("GET %s: %d %s, want 500 without log URIs", path, rec.Code, rec.Body)
		}
	}
}

// failingObjects fails the test if the server reads the deleted bucket.
type failingObjects struct{ t *testing.T }

func (o failingObjects) Client(context.Context, string) (string, runbucket.Client, error) {
	o.t.Error("the deleted run's bucket was read")
	return "", nil, context.Canceled
}

// A run whose data is gone still lists and reads (200, no outputs), and its
// logs say that the data expired instead of naming deleted objects.
func TestARunWhoseDataIsGoneAnswersThatItExpired(t *testing.T) {
	ctrl := controller.NewRunController(&expiredRuns{}, &tasksOfRun{}, &wesEventStoreStub{},
		storage.Layout{Bucket: "conquest", Version: "v1"}, adapter.NewRegistry(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctrl.SetRunObjects(failingObjects{t})
	h := NewHandler(ctrl)
	r := chi.NewRouter()
	h.Routes(r)

	get := func(path string, into any) {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatal(err)
		}
	}
	expired := func(what string, logs []string) {
		t.Helper()
		if len(logs) != 1 || !strings.Contains(logs[0], "deleted") || !strings.Contains(logs[0], dataDeletedAt.Format(time.RFC3339)) {
			t.Errorf("%s system_logs = %q, want one saying the data was deleted at %s", what, logs, dataDeletedAt.Format(time.RFC3339))
		}
	}

	var list RunListResponse
	get("/ga4gh/wes/v1/runs", &list)
	if len(list.Runs) != 1 || list.Runs[0].RunID != "a" || list.Runs[0].State != domain.StateComplete {
		t.Errorf("runs = %+v, want run a, COMPLETE", list.Runs)
	}

	var run RunLog
	get("/ga4gh/wes/v1/runs/a", &run)
	if run.State != domain.StateComplete || run.Outputs != nil {
		t.Errorf("run = %+v, want COMPLETE without outputs", run)
	}
	if run.RunLog == nil || run.RunLog.Stdout != "" || run.RunLog.EndTime == "" {
		t.Fatalf("run log = %+v, want its end time but no stdout URI", run.RunLog)
	}
	expired("run log", run.RunLog.SystemLogs)

	var tasks TaskListResponse
	get("/ga4gh/wes/v1/runs/a/tasks", &tasks)
	if len(tasks.TaskLogs) != 1 || tasks.TaskLogs[0].Stdout != "" {
		t.Fatalf("task logs = %+v, want one without stdout URI", tasks.TaskLogs)
	}
	expired("task log", tasks.TaskLogs[0].SystemLogs)

	var task TaskLog
	get("/ga4gh/wes/v1/runs/a/tasks/t1", &task)
	if task.Stdout != "" {
		t.Errorf("task log stdout = %q, want none", task.Stdout)
	}
	expired("task", task.SystemLogs)
}
