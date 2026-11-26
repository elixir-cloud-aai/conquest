package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/anuragxxd/conquest/internal/adapter"
	"github.com/anuragxxd/conquest/internal/backend"
	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/runbucket"
	"github.com/anuragxxd/conquest/internal/storage"
)

// runBucketsStub gives every run the bucket and Secret "conquest-run-<ID>".
type runBucketsStub struct {
	ensureErr error
	ensured   []string
	deleted   []string
}

func (s *runBucketsStub) Ensure(_ context.Context, runID string) (runbucket.Bucket, error) {
	s.ensured = append(s.ensured, runID)
	if s.ensureErr != nil {
		return runbucket.Bucket{}, s.ensureErr
	}
	return runbucket.Bucket{Name: "conquest-run-" + runID, SecretName: "conquest-run-" + runID}, nil
}

func (s *runBucketsStub) Delete(_ context.Context, runID string) error {
	s.deleted = append(s.deleted, runID)
	return nil
}

func runBucketsController(runs *runStoreStub, runAdapter *runAdapterStub, runBackend *runBackendStub, buckets runbucket.Provisioner) *RunController {
	ctrl := NewRunController(
		runs,
		&taskStoreStub{},
		&eventStoreStub{},
		storage.Layout{Bucket: "conquest", Version: "v1"},
		adapter.NewRegistry(runAdapter),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	ctrl.SetRunBackend(runBackend)
	ctrl.SetRunBuckets(buckets)
	return ctrl
}

func preparedNextflowRun() *adapter.PreparedRun {
	return &adapter.PreparedRun{
		WorkflowEngine:        "nextflow",
		WorkflowEngineVersion: "24.10.4",
		RequestPayload:        json.RawMessage(`{"workflow_url":"https://github.com/org/repo"}`),
		RunnerSpec: backend.RunnerSpec{
			Image:   "example.com/conquest-nf-24.10.4-runner:latest",
			Command: []string{"/usr/local/bin/conquest"},
		},
	}
}

var nextflowRequest = &RunRequest{
	WorkflowType:        "NFL",
	WorkflowTypeVersion: "DSL2",
	WorkflowURL:         "https://github.com/org/repo",
}

func TestSubmitRun_UsesTheRunsBucketAndSecret(t *testing.T) {
	runs := &runStoreStub{}
	runAdapter := &runAdapterStub{prepared: preparedNextflowRun()}
	runBackend := &runBackendStub{submission: &backend.RunSubmission{BackendRef: "runner-job-1"}}
	buckets := &runBucketsStub{}

	run, err := runBucketsController(runs, runAdapter, runBackend, buckets).SubmitRun(context.Background(), nextflowRequest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	own := "conquest-run-" + run.RunID
	if !slices.Equal(buckets.ensured, []string{run.RunID}) {
		t.Fatalf("ensured buckets for %v, want [%s]", buckets.ensured, run.RunID)
	}
	if runAdapter.layout.Bucket != own || runAdapter.layout.Version != "v1" {
		t.Fatalf("adapter layout = %+v, want bucket %s, version v1", runAdapter.layout, own)
	}
	if want := "s3://" + own + "/"; !strings.HasPrefix(run.RunRootURI, want) || !strings.HasPrefix(run.OutputPrefix, want) {
		t.Fatalf("run root %q, outputs %q, want both below %s", run.RunRootURI, run.OutputPrefix, want)
	}
	if runBackend.spec == nil || runBackend.spec.StorageSecretName != own {
		t.Fatalf("runner spec = %+v, want Secret %s", runBackend.spec, own)
	}
	if len(buckets.deleted) != 0 {
		t.Fatalf("deleted buckets %v of a submitted run", buckets.deleted)
	}
}

func TestSubmitRun_FailsWithoutCreatingTheRunIfTheBucketFails(t *testing.T) {
	runs := &runStoreStub{}
	runAdapter := &runAdapterStub{prepared: preparedNextflowRun()}
	runBackend := &runBackendStub{submission: &backend.RunSubmission{BackendRef: "runner-job-1"}}
	buckets := &runBucketsStub{ensureErr: errors.New("no bucket")}

	_, err := runBucketsController(runs, runAdapter, runBackend, buckets).SubmitRun(context.Background(), nextflowRequest)
	if err == nil || !strings.Contains(err.Error(), "no bucket") {
		t.Fatalf("err = %v, want the bucket error", err)
	}
	if runAdapter.called || runs.createRun != nil || runBackend.spec != nil {
		t.Fatal("run was prepared, persisted or submitted without a bucket")
	}
	if !slices.Equal(buckets.deleted, buckets.ensured) {
		t.Fatalf("deleted %v, want the half-made bucket %v deleted", buckets.deleted, buckets.ensured)
	}
}

func TestSubmitRun_DeletesTheBucketOfARunThatWasNotCreated(t *testing.T) {
	for name, tc := range map[string]struct {
		runs       *runStoreStub
		runAdapter *runAdapterStub
	}{
		"invalid request": {&runStoreStub{}, &runAdapterStub{err: &adapter.ValidationError{Field: "workflow_url", Msg: "bad"}}},
		"store fails":     {&runStoreStub{createErr: errors.New("db down")}, &runAdapterStub{prepared: preparedNextflowRun()}},
	} {
		t.Run(name, func(t *testing.T) {
			buckets := &runBucketsStub{}
			runBackend := &runBackendStub{submission: &backend.RunSubmission{BackendRef: "runner-job-1"}}

			if _, err := runBucketsController(tc.runs, tc.runAdapter, runBackend, buckets).SubmitRun(context.Background(), nextflowRequest); err == nil {
				t.Fatal("expected an error")
			}
			if len(buckets.ensured) != 1 || !slices.Equal(buckets.deleted, buckets.ensured) {
				t.Fatalf("ensured %v, deleted %v, want the run's bucket deleted", buckets.ensured, buckets.deleted)
			}
			if runBackend.spec != nil {
				t.Fatal("runner was submitted")
			}
		})
	}
}

// A run that was created keeps its bucket when the runner fails to start:
// its record and error are kept, like its data.
func TestSubmitRun_KeepsTheBucketOfACreatedRunWhoseRunnerFailed(t *testing.T) {
	runs := &runStoreStub{}
	runBackend := &runBackendStub{err: errors.New("quota exceeded")}
	buckets := &runBucketsStub{}

	run, err := runBucketsController(runs, &runAdapterStub{prepared: preparedNextflowRun()}, runBackend, buckets).SubmitRun(context.Background(), nextflowRequest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if run.State != domain.StateSystemError {
		t.Fatalf("run state = %s, want SYSTEM_ERROR", run.State)
	}
	if len(buckets.deleted) != 0 {
		t.Fatalf("deleted buckets %v of a created run", buckets.deleted)
	}
}
