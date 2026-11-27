package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/anuragxxd/conquest/internal/config"
	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/repository"
	"github.com/anuragxxd/conquest/internal/runbucket"
)

type noRetentionStore struct{}

func (noRetentionStore) ListExpiredRuns(context.Context, time.Time, repository.RunCursor, int) ([]*domain.Run, error) {
	return nil, nil
}
func (noRetentionStore) SetDataDeleted(context.Context, string) error { return nil }
func (noRetentionStore) Get(context.Context, string) (*domain.Run, error) {
	return nil, nil
}

type noLeases struct{}

func (noLeases) Claim(context.Context, string, string, time.Duration) (bool, error) {
	return false, nil
}

func retentionConfig(mode runbucket.Mode, retention time.Duration) *config.Config {
	return &config.Config{
		RunBuckets:        mode,
		RunRetention:      retention,
		S3Bucket:          "conquest-workflows",
		S3Endpoint:        "http://s3.example:8333",
		StorageSecretName: "storage-secrets",
		LayoutVersion:     "v1",
	}
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestNewRunRetention_IsOffByDefault(t *testing.T) {
	s, err := newRunRetention(retentionConfig(runbucket.ModeShared, 0), testCreds, nil, noRetentionStore{}, noLeases{}, discard)
	if err != nil || s != nil {
		t.Fatalf("newRunRetention = %v, %v; want no sweeper", s, err)
	}
}

func TestNewRunRetention_NeedsTheServersCredentials(t *testing.T) {
	if _, err := newRunRetention(retentionConfig(runbucket.ModeShared, time.Hour), runbucket.Credentials{}, nil, noRetentionStore{}, noLeases{}, discard); err == nil {
		t.Fatal("retention without S3 credentials: want an error")
	}
}

func TestNewRunRetention_Shared(t *testing.T) {
	prov, err := runbucket.NewShared("conquest-workflows", "storage-secrets")
	if err != nil {
		t.Fatal(err)
	}
	s, err := newRunRetention(retentionConfig(runbucket.ModeShared, time.Hour), testCreds, prov, noRetentionStore{}, noLeases{}, discard)
	if err != nil || s == nil {
		t.Fatalf("newRunRetention = %v, %v", s, err)
	}
	_, orphans, err := runDataDeleter(retentionConfig(runbucket.ModeShared, time.Hour), testCreds, prov)
	if err != nil {
		t.Fatal(err)
	}
	if orphans != nil {
		t.Fatalf("orphans = %T, want none: the shared bucket has no buckets of runs", orphans)
	}
}

// With obc, runs in the configured bucket (from before the switch) and runs
// in their own buckets are deleted, and orphaned claims are swept.
func TestNewRunRetention_OBC(t *testing.T) {
	cfg := retentionConfig(runbucket.ModeOBC, time.Hour)
	prov, err := runbucket.NewOBC(dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), kubefake.NewSimpleClientset(),
		runbucket.OBCConfig{Namespace: "conquest", StorageClassName: "sc", BindTimeout: time.Second, Instance: "conquest-workflows"})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := newRunRetention(cfg, testCreds, prov, noRetentionStore{}, noLeases{}, discard); err != nil || s == nil {
		t.Fatalf("newRunRetention = %v, %v", s, err)
	}
	data, orphans, err := runDataDeleter(cfg, testCreds, prov)
	if err != nil {
		t.Fatal(err)
	}
	if orphans != prov {
		t.Fatalf("orphans = %v, want the obc provisioner", orphans)
	}
	// a run in a bucket of its own goes to the provisioner (no claim: done)
	if err := data.DeleteRunData(context.Background(), &domain.Run{RunID: "0193b6a2-7c1e-7f00-8000-000000000001",
		RunRootURI: "s3://conquest-run-x/v1/workflow-runs/0193b6a2-7c1e-7f00-8000-000000000001/"}); err != nil {
		t.Fatalf("DeleteRunData: %v", err)
	}
}
