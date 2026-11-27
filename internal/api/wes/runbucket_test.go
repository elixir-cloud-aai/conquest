package wes

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/anuragxxd/conquest/internal/adapter"
	"github.com/anuragxxd/conquest/internal/controller"
	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/repository"
	"github.com/anuragxxd/conquest/internal/storage"
)

// runInOwnBucket is run a, stored in its own bucket run-a.
type runInOwnBucket struct{ wesRunStoreStub }

func (runInOwnBucket) Get(_ context.Context, runID string) (*domain.Run, error) {
	return &domain.Run{RunID: runID, State: domain.StateRunning, RunRootURI: "s3://run-" + runID + "/v1/workflow-runs/" + runID + "/"}, nil
}

type tasksOfRun struct{ wesTaskStoreStub }

func (tasksOfRun) ListByRunID(_ context.Context, runID string, _ repository.Page) (*repository.PageResult[*domain.Task], error) {
	return &repository.PageResult[*domain.Task]{Items: []*domain.Task{{TaskID: "t1", RunID: runID, Attempt: 1}}}, nil
}

func (tasksOfRun) Get(_ context.Context, taskID string) (*domain.Task, error) {
	return &domain.Task{TaskID: taskID, RunID: "a", Attempt: 1}, nil
}

// Log URIs name the run's own bucket, not the server's.
func TestLogURIsAreInTheRunsBucket(t *testing.T) {
	ctrl := controller.NewRunController(&runInOwnBucket{}, &tasksOfRun{}, &wesEventStoreStub{},
		storage.Layout{Bucket: "conquest", Version: "v1"}, adapter.NewRegistry(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := NewHandler(ctrl)
	r := chi.NewRouter()
	r.Get("/runs/{run_id}", h.GetRun)
	r.Get("/runs/{run_id}/tasks", h.ListRunTasks)
	r.Get("/runs/{run_id}/tasks/{task_id}", h.GetRunTask)

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

	var run RunLog
	get("/runs/a", &run)
	if want := "s3://run-a/v1/workflow-runs/a/logs/run.log"; run.RunLog == nil || run.RunLog.Stdout != want {
		t.Errorf("run log = %+v, want %s", run.RunLog, want)
	}
	layout := storage.Layout{Bucket: "run-a", Version: "v1"}
	want := layout.TaskLogURI("a", "t1", 1, 0)
	var tasks TaskListResponse
	get("/runs/a/tasks", &tasks)
	if len(tasks.TaskLogs) != 1 || tasks.TaskLogs[0].Stdout != want {
		t.Errorf("task logs = %+v, want %s", tasks.TaskLogs, want)
	}
	var task TaskLog
	get("/runs/a/tasks/t1", &task)
	if task.Stdout != want {
		t.Errorf("task log = %s, want %s", task.Stdout, want)
	}
}
