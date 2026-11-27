//go:build integration

// See s3_integration_test.go for how to run it.
package isolation_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/runbucket"
	"github.com/anuragxxd/conquest/internal/storage"
)

// TestSharedDeleteRemovesOnlyTheRunsObjects deletes a run's data in the
// shared bucket (run retention): every object below its run root goes, in
// more than one DeleteObjects batch (1000 keys), and nothing else does.
func TestSharedDeleteRemovesOnlyTheRunsObjects(t *testing.T) {
	admin, client, bucket := s3Env(t) // skips without a test S3 server
	ctx := context.Background()
	layout := storage.Layout{Bucket: bucket, Version: "v1"}
	runA, runB := "run-a", "run-a2" // run-a2 shares run-a's prefix but not its root

	const many = 1001
	keys := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range keys {
				if _, err := admin.PutObject(ctx, bucket, k, strings.NewReader("x"), 1, minio.PutObjectOptions{}); err != nil {
					t.Error(err) // not Fatal: not the test's goroutine
				}
			}
		}()
	}
	for i := 0; i < many; i++ {
		keys <- fmt.Sprintf("%swork/nextflow/%04d.txt", layout.RunRoot(runA), i)
	}
	close(keys)
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	kept := []string{
		layout.RunRoot(runB) + "outputs/result.txt",
		"v1/workflow-runs/other.txt",
		"unrelated/" + runA + "/x.txt",
	}
	for _, k := range kept {
		put(t, admin, bucket, k, "keep")
	}

	shared, err := runbucket.NewShared(bucket, "storage-secrets")
	if err != nil {
		t.Fatal(err)
	}
	shared.SetObjects(client, "v1")
	if err := shared.Delete(ctx, runA); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	left := 0
	for obj := range admin.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: layout.RunRoot(runA), Recursive: true}) {
		if obj.Err != nil {
			t.Fatal(obj.Err)
		}
		left++
	}
	if left != 0 {
		t.Fatalf("%d objects of run %s left", left, runA)
	}
	for _, k := range kept {
		if _, ok := get(admin, bucket, k); !ok {
			t.Errorf("%s was deleted", k)
		}
	}

	// deleting again, and through the run's stored root, finds nothing to do
	data, err := runbucket.NewDataDeleter(shared, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.DeleteRunData(ctx, &domain.Run{RunID: runA, LayoutVersion: "v1", RunRootURI: layout.RunRootURI(runA)}); err != nil {
		t.Fatalf("DeleteRunData again: %v", err)
	}
}
