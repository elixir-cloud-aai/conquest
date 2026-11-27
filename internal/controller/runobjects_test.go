package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/anuragxxd/conquest/internal/adapter"
	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/runbucket"
	"github.com/anuragxxd/conquest/internal/storage"
)

// bucketClient holds the objects that one run's credentials reach.
type bucketClient struct {
	artifactStoreStub
}

func (bucketClient) Presign(context.Context, string, string, string, time.Duration) (*url.URL, error) {
	return nil, errors.New("not used")
}

// runObjectsStub gives each run a client that reaches only its bucket.
type runObjectsStub struct {
	buckets map[string]string // run ID -> bucket
	objects map[string]map[string][]byte
	asked   []string
}

func (s *runObjectsStub) Client(_ context.Context, runID string) (string, runbucket.Client, error) {
	s.asked = append(s.asked, runID)
	bucket, ok := s.buckets[runID]
	if !ok {
		return "", nil, errors.New("unknown run")
	}
	return bucket, &bucketClient{artifactStoreStub{objects: s.objects[bucket]}}, nil
}

func runObjectsController(runs *runStoreStub) *RunController {
	return NewRunController(runs, &taskStoreStub{}, &eventStoreStub{},
		storage.Layout{Bucket: "conquest", Version: "v1"}, adapter.NewRegistry(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Two runs in two buckets: each run's outputs are read from its own bucket
// with its own client.
func TestGetRun_ReadsOutputsFromTheRunsBucket(t *testing.T) {
	objects := &runObjectsStub{
		buckets: map[string]string{"a": "run-a", "b": "run-b"},
		objects: map[string]map[string][]byte{
			"run-a": {"run-a/v1/workflow-runs/a/metadata/workflow-outputs.json": []byte(`{"out":"a"}`)},
			"run-b": {"run-b/v1/workflow-runs/b/metadata/workflow-outputs.json": []byte(`{"out":"b"}`)},
		},
	}
	runs := &runStoreStub{}
	ctrl := runObjectsController(runs)
	ctrl.SetRunObjects(objects)
	// the shared artifact store must not be used
	ctrl.SetArtifactStore(&artifactStoreStub{objects: map[string][]byte{
		"conquest/v1/workflow-runs/a/metadata/workflow-outputs.json": []byte(`{"out":"shared"}`),
	}})

	for _, id := range []string{"a", "b"} {
		runs.getRun = &domain.Run{RunID: id, WorkflowEngine: "wdl", RunRootURI: "s3://run-" + id + "/v1/workflow-runs/" + id + "/"}
		result, err := ctrl.GetRun(context.Background(), id)
		if err != nil {
			t.Fatalf("GetRun(%s): %v", id, err)
		}
		if want := `{"out":"` + id + `"}`; string(result.Outputs) != want {
			t.Fatalf("GetRun(%s) outputs = %s, want %s", id, result.Outputs, want)
		}
	}
	if len(objects.asked) != 2 || objects.asked[0] != "a" || objects.asked[1] != "b" {
		t.Fatalf("clients asked for %v", objects.asked)
	}
}

// Without the run's client, no outputs are read, also not with the shared
// store.
func TestGetRun_NoOutputsWithoutTheRunsClient(t *testing.T) {
	runs := &runStoreStub{getRun: &domain.Run{RunID: "x", WorkflowEngine: "wdl", RunRootURI: "s3://conquest/v1/workflow-runs/x/"}}
	ctrl := runObjectsController(runs)
	ctrl.SetRunObjects(&runObjectsStub{})
	ctrl.SetArtifactStore(&artifactStoreStub{objects: map[string][]byte{
		"conquest/v1/workflow-runs/x/metadata/workflow-outputs.json": []byte(`{"out":"shared"}`),
	}})
	result, err := ctrl.GetRun(context.Background(), "x")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if result.Outputs != nil {
		t.Fatalf("outputs = %s, want none", result.Outputs)
	}
}

// A run whose data was deleted after the retention period is read without
// touching its (deleted) bucket.
func TestGetRun_ReadsNoOutputsOfARunWhoseDataIsGone(t *testing.T) {
	deleted := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	runs := &runStoreStub{getRun: &domain.Run{RunID: "a", WorkflowEngine: "wdl", RunRootURI: "s3://run-a/v1/workflow-runs/a/", DataDeletedAt: &deleted}}
	objects := &runObjectsStub{buckets: map[string]string{"a": "run-a"}}
	ctrl := runObjectsController(runs)
	ctrl.SetRunObjects(objects)
	result, err := ctrl.GetRun(context.Background(), "a")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if result.Outputs != nil || len(objects.asked) != 0 {
		t.Fatalf("outputs = %s, clients asked for %v; want neither", result.Outputs, objects.asked)
	}
}

// The artifact store alone reads from the run's bucket.
func TestGetRun_ArtifactStoreReadsTheRunsBucket(t *testing.T) {
	runs := &runStoreStub{getRun: &domain.Run{RunID: "a", WorkflowEngine: "wdl", RunRootURI: "s3://run-a/v1/workflow-runs/a/"}}
	ctrl := runObjectsController(runs)
	ctrl.SetArtifactStore(&artifactStoreStub{objects: map[string][]byte{
		"run-a/v1/workflow-runs/a/metadata/workflow-outputs.json": []byte(`{"out":"a"}`),
	}})
	result, err := ctrl.GetRun(context.Background(), "a")
	if err != nil || string(result.Outputs) != `{"out":"a"}` {
		t.Fatalf("GetRun = %+v, %v", result, err)
	}
}

func TestLayoutOf(t *testing.T) {
	ctrl := runObjectsController(&runStoreStub{})
	for root, want := range map[string]string{
		"s3://run-a/v1/workflow-runs/a/":    "run-a",
		"s3://conquest/v1/workflow-runs/a/": "conquest",
		"":                                  "conquest",
		"not a URI":                         "conquest",
	} {
		got := ctrl.LayoutOf(&domain.Run{RunID: "a", RunRootURI: root})
		if got.Bucket != want || got.Version != "v1" {
			t.Errorf("LayoutOf(%q) = %+v, want bucket %s", root, got, want)
		}
	}
	old := ctrl.LayoutOf(&domain.Run{RunID: "a", LayoutVersion: "v0"})
	if old.Version != "v0" || old.Bucket != "conquest" {
		t.Errorf("LayoutOf(stored version v0) = %+v, want version v0", old)
	}
	if got := ctrl.LayoutOf(nil); got.Bucket != "conquest" {
		t.Errorf("LayoutOf(nil) = %+v", got)
	}
}

func TestRun_ReadsTheStoredRun(t *testing.T) {
	runs := &runStoreStub{getRun: &domain.Run{RunID: "a", RunRootURI: "s3://run-a/v1/workflow-runs/a/"}}
	ctrl := runObjectsController(runs)
	run, err := ctrl.Run(context.Background(), "a")
	if err != nil || run.RunID != "a" {
		t.Fatalf("Run = %+v, %v", run, err)
	}
	got := ctrl.LayoutOf(run)
	if got.Bucket != "run-a" {
		t.Fatalf("LayoutOf = %+v", got)
	}
	if got.RunLogURI("a") != "s3://run-a/v1/workflow-runs/a/logs/run.log" {
		t.Fatalf("RunLogURI = %s", got.RunLogURI("a"))
	}
}
