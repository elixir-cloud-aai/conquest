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
	// onEnsure, if set, runs in Ensure (e.g. to use up the request's time).
	onEnsure func()
}

func (s *runBucketsStub) Ensure(_ context.Context, runID string) (runbucket.Bucket, error) {
	s.ensured = append(s.ensured, runID)
	if s.onEnsure != nil {
		s.onEnsure()
	}
	if s.ensureErr != nil {
		return runbucket.Bucket{}, s.ensureErr
	}
	return runbucket.Bucket{Name: "conquest-run-" + runID, SecretName: "conquest-run-" + runID}, nil
}

func (s *runBucketsStub) Credentials(context.Context, string) (runbucket.Credentials, error) {
	return runbucket.Credentials{}, errors.New("not used")
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
	if runs.createRun != nil || runBackend.spec != nil {
		t.Fatal("run was persisted or submitted without a bucket")
	}
	if !slices.Equal(buckets.deleted, buckets.ensured) {
		t.Fatalf("deleted %v, want the half-made bucket %v deleted", buckets.deleted, buckets.ensured)
	}
}

// A request whose time ran out while its bucket was made stores no run (its
// client would never learn the run ID) and deletes the bucket again.
func TestSubmitRun_StoresNoRunAfterTheRequestEnded(t *testing.T) {
	runs := &runStoreStub{}
	runAdapter := &runAdapterStub{prepared: preparedNextflowRun()}
	runBackend := &runBackendStub{submission: &backend.RunSubmission{BackendRef: "runner-job-1"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buckets := &runBucketsStub{onEnsure: cancel}

	_, err := runBucketsController(runs, runAdapter, runBackend, buckets).SubmitRun(ctx, nextflowRequest)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if runs.createRun != nil || runBackend.spec != nil {
		t.Fatal("run was persisted or submitted after its request ended")
	}
	if len(buckets.ensured) != 1 || !slices.Equal(buckets.deleted, buckets.ensured) {
		t.Fatalf("deleted %v, want the bucket %v deleted", buckets.deleted, buckets.ensured)
	}
}

// An invalid request fails before the run gets a bucket.
func TestSubmitRun_InvalidRequestGetsNoBucket(t *testing.T) {
	buckets := &runBucketsStub{}
	runAdapter := &runAdapterStub{err: &adapter.ValidationError{Field: "workflow_url", Msg: "bad"}}
	runBackend := &runBackendStub{submission: &backend.RunSubmission{BackendRef: "runner-job-1"}}

	if _, err := runBucketsController(&runStoreStub{}, runAdapter, runBackend, buckets).SubmitRun(context.Background(), nextflowRequest); err == nil {
		t.Fatal("expected an error")
	}
	if len(buckets.ensured) != 0 || len(buckets.deleted) != 0 {
		t.Fatalf("ensured %v, deleted %v, want no bucket for an invalid request", buckets.ensured, buckets.deleted)
	}
}

// A run that fails to prepare in its own bucket gets its bucket deleted.
func TestSubmitRun_DeletesTheBucketIfTheRunFailsToPrepareInIt(t *testing.T) {
	buckets := &runBucketsStub{}
	runAdapter := &runAdapterStub{prepared: preparedNextflowRun(), failInBucketPrefix: "conquest-run-"}
	runBackend := &runBackendStub{submission: &backend.RunSubmission{BackendRef: "runner-job-1"}}

	if _, err := runBucketsController(&runStoreStub{}, runAdapter, runBackend, buckets).SubmitRun(context.Background(), nextflowRequest); err == nil {
		t.Fatal("expected an error")
	}
	if len(buckets.ensured) != 1 || !slices.Equal(buckets.deleted, buckets.ensured) {
		t.Fatalf("ensured %v, deleted %v, want the run's bucket deleted", buckets.ensured, buckets.deleted)
	}
	if runBackend.spec != nil {
		t.Fatal("runner was submitted")
	}
}

// A failed Create may still have stored the run: its bucket is kept.
func TestSubmitRun_KeepsTheBucketIfStoringTheRunFails(t *testing.T) {
	buckets := &runBucketsStub{}
	runBackend := &runBackendStub{submission: &backend.RunSubmission{BackendRef: "runner-job-1"}}

	if _, err := runBucketsController(&runStoreStub{createErr: errors.New("db down")}, &runAdapterStub{prepared: preparedNextflowRun()}, runBackend, buckets).SubmitRun(context.Background(), nextflowRequest); err == nil {
		t.Fatal("expected an error")
	}
	if len(buckets.ensured) != 1 || len(buckets.deleted) != 0 {
		t.Fatalf("ensured %v, deleted %v, want the bucket kept", buckets.ensured, buckets.deleted)
	}
	if runBackend.spec != nil {
		t.Fatal("runner was submitted")
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

// fixedBucketStub returns the same bucket for every run.
type fixedBucketStub struct {
	bucket  runbucket.Bucket
	deleted []string
}

func (s *fixedBucketStub) Ensure(context.Context, string) (runbucket.Bucket, error) {
	return s.bucket, nil
}

func (s *fixedBucketStub) Credentials(context.Context, string) (runbucket.Credentials, error) {
	return runbucket.Credentials{}, errors.New("not used")
}

func (s *fixedBucketStub) Delete(_ context.Context, runID string) error {
	s.deleted = append(s.deleted, runID)
	return nil
}

// A provisioner result without a bucket or Secret fails closed: the runner
// must never fall back to the bucket-wide Secret.
func TestSubmitRun_RefusesAnIncompleteBucket(t *testing.T) {
	for name, bucket := range map[string]runbucket.Bucket{
		"no Secret": {Name: "conquest-run-x"},
		"no bucket": {SecretName: "conquest-run-x"},
	} {
		t.Run(name, func(t *testing.T) {
			runs := &runStoreStub{}
			runBackend := &runBackendStub{submission: &backend.RunSubmission{BackendRef: "runner-job-1"}}
			buckets := &fixedBucketStub{bucket: bucket}

			if _, err := runBucketsController(runs, &runAdapterStub{prepared: preparedNextflowRun()}, runBackend, buckets).SubmitRun(context.Background(), nextflowRequest); err == nil {
				t.Fatal("expected an error")
			}
			if runs.createRun != nil || runBackend.spec != nil {
				t.Fatal("run was persisted or submitted with an incomplete bucket")
			}
			if len(buckets.deleted) != 1 {
				t.Fatalf("deleted %v, want the run's bucket deleted", buckets.deleted)
			}
		})
	}
}

// With the shared bucket the run is prepared once: the server's layout is
// already the run's.
func TestSubmitRun_SharedBucketPreparesTheRunOnce(t *testing.T) {
	shared, err := runbucket.NewShared("conquest", "storage-secrets")
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}
	runAdapter := &runAdapterStub{prepared: preparedNextflowRun()}
	runBackend := &runBackendStub{submission: &backend.RunSubmission{BackendRef: "runner-job-1"}}

	if _, err := runBucketsController(&runStoreStub{}, runAdapter, runBackend, shared).SubmitRun(context.Background(), nextflowRequest); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runAdapter.calls != 1 {
		t.Fatalf("PrepareRun calls = %d, want 1", runAdapter.calls)
	}
	if runBackend.spec == nil || runBackend.spec.StorageSecretName != "storage-secrets" {
		t.Fatalf("runner spec = %+v, want Secret storage-secrets", runBackend.spec)
	}
}
