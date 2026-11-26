package main

import (
	"context"
	"testing"

	"github.com/anuragxxd/conquest/internal/config"
	"github.com/anuragxxd/conquest/internal/runbucket"
)

func TestNewRunBuckets_SharedUsesTheConfiguredBucketAndSecret(t *testing.T) {
	p, err := newRunBuckets(&config.Config{
		RunBuckets:        runbucket.ModeShared,
		S3Bucket:          "conquest-workflows",
		StorageSecretName: "storage-secrets",
	})
	if err != nil {
		t.Fatalf("newRunBuckets: %v", err)
	}
	got, err := p.Ensure(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if want := (runbucket.Bucket{Name: "conquest-workflows", SecretName: "storage-secrets"}); got != want {
		t.Fatalf("Ensure = %+v, want %+v", got, want)
	}
}

func TestNewRunBuckets_RefusesUnknownModes(t *testing.T) {
	if _, err := newRunBuckets(&config.Config{RunBuckets: "per-run", S3Bucket: "b", StorageSecretName: "s"}); err == nil {
		t.Fatal("newRunBuckets accepted an unknown mode")
	}
}
