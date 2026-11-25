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
	"github.com/anuragxxd/conquest/internal/repository"
	"github.com/anuragxxd/conquest/internal/repository/pg"
)

// memRuns returns the runs it holds; any other run is pg.ErrNotFound (as
// for a run that does not exist or that another owner's scope hides).
type memRuns struct {
	repository.RunStore // unused methods panic
	runs                map[string]*domain.Run
	err                 error
}

func (m *memRuns) Get(_ context.Context, id string) (*domain.Run, error) {
	if m.err != nil {
		return nil, m.err
	}
	if r, ok := m.runs[id]; ok {
		return r, nil
	}
	return nil, pg.ErrNotFound
}

func (m *memRuns) GetStatus(ctx context.Context, id string) (domain.State, error) {
	r, err := m.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return r.State, nil
}

func TestCreateTaskWithRunTag(t *testing.T) {
	own := &domain.Run{RunID: "run-own", State: domain.StateRunning}
	body := `{"executors":[{"image":"ubuntu:24.04","command":["true"]}],"tags":{"conquest_run_id":"%s"}}`
	tests := []struct {
		name     string
		runID    string
		storeErr error
		want     int
		created  bool
	}{
		{"readable run", "run-own", nil, http.StatusOK, true},
		{"unreadable run", "run-foreign", nil, http.StatusNotFound, false},
		{"store failure", "run-own", errors.New("db down"), http.StatusInternalServerError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasks := &memTasks{tasks: map[string]*domain.Task{}}
			h := NewHandler(&memRuns{runs: map[string]*domain.Run{own.RunID: own}, err: tt.storeErr}, tasks, memEvents{}, nil)
			h.SetDispatcher(&notifier{})
			r := chi.NewRouter()
			h.Routes(r)
			w := httptest.NewRecorder()

			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ga4gh/tes/v1/tasks",
				strings.NewReader(strings.Replace(body, "%s", tt.runID, 1))))

			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body)
			}
			if got := len(tasks.tasks) == 1; got != tt.created {
				t.Fatalf("task created = %v, want %v", got, tt.created)
			}
		})
	}
}
