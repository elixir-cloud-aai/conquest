package storagegw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/anuragxxd/conquest/internal/domain"
)

// runStore is the store of one run's bucket.
type runStore struct {
	presigner
	listed []string
}

func (s *runStore) ListPrefix(_ context.Context, bucket, prefix string) ([]string, error) {
	s.listed = append(s.listed, bucket+"/"+prefix)
	return []string{prefix + "a.txt"}, nil
}

const runARequest = `{
	"inputs": [
		{"url": "s3://run-a/run/in.txt", "path": "/work/in.txt"},
		{"url": "s3://run-a/run/dir", "path": "/work/dir", "type": "DIRECTORY"},
		{"url": "s3://run-b/run/in.txt", "path": "/work/other.txt"},
		{"url": "s3://conquest/run/in.txt", "path": "/work/shared.txt"}
	]
}`

// runStorageGateway serves task t1 of run a, whose bucket is run-a.
func runStorageGateway(t *testing.T, storageErr error) (*Gateway, *presigner, *runStore, http.Handler) {
	t.Helper()
	task := &domain.Task{TaskID: "t1", RunID: "a", State: domain.StateRunning, TaskRequestPayload: json.RawMessage(runARequest)}
	gw, shared, h := newGateway(t, task)
	store := &runStore{}
	gw.SetLister(failingLister{}) // enables listings; the run store must list
	gw.SetRunStorage(func(_ context.Context, task *domain.Task) (string, RunStore, error) {
		if storageErr != nil {
			return "", nil, storageErr
		}
		return "run-" + task.RunID, store, nil
	})
	return gw, shared, store, h
}

// failingLister fails if it is used.
type failingLister struct{}

func (failingLister) ListPrefix(context.Context, string, string) ([]string, error) {
	return nil, errors.New("the shared lister must not be used")
}

// A task reaches its run's bucket, presigned with the run's store, and no
// other bucket, the configured one included, although its request names them.
func TestRunStorage_ServesOnlyTheRunsBucketWithItsStore(t *testing.T) {
	gw, shared, store, h := runStorageGateway(t, nil)

	rec := do(t, h, http.MethodGet, gw.URL("t1", "run-a", "run/in.txt"))
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("own bucket: %d %s", rec.Code, rec.Body)
	}
	if len(store.calls) != 1 || store.calls[0] != "GET run-a/run/in.txt" || len(shared.calls) != 0 {
		t.Fatalf("presigned by run store %v, shared %v", store.calls, shared.calls)
	}

	for _, bucket := range []string{"run-b", "conquest"} {
		key := "run/in.txt"
		if rec := do(t, h, http.MethodGet, gw.URL("t1", bucket, key)); rec.Code != http.StatusForbidden {
			t.Errorf("bucket %s: %d, want 403", bucket, rec.Code)
		}
	}
	if len(store.calls) != 1 || len(shared.calls) != 0 {
		t.Fatalf("presigned for another bucket: run store %v, shared %v", store.calls, shared.calls)
	}
}

// Listings use the run's store.
func TestRunStorage_ListsWithTheRunsStore(t *testing.T) {
	gw, _, store, h := runStorageGateway(t, nil)
	rec := do(t, h, http.MethodGet, gw.URL("t1", "run-a", "run/dir/"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "run/dir/a.txt") {
		t.Fatalf("listing: %d %s", rec.Code, rec.Body)
	}
	if len(store.listed) != 1 || store.listed[0] != "run-a/run/dir/" {
		t.Fatalf("listed %v", store.listed)
	}
}

// Without SetLister, listings stay off with run storage too.
func TestRunStorage_NoListingsWithoutSetLister(t *testing.T) {
	task := &domain.Task{TaskID: "t1", RunID: "a", State: domain.StateRunning, TaskRequestPayload: json.RawMessage(runARequest)}
	gw, _, h := newGateway(t, task)
	store := &runStore{}
	gw.SetRunStorage(func(context.Context, *domain.Task) (string, RunStore, error) { return "run-a", store, nil })
	do(t, h, http.MethodGet, gw.URL("t1", "run-a", "run/dir/"))
	if len(store.listed) != 0 {
		t.Fatalf("listed %v without SetLister", store.listed)
	}
}

// If the run's storage can't be found, nothing is served.
func TestRunStorage_FailsClosed(t *testing.T) {
	gw, shared, store, h := runStorageGateway(t, errors.New("no run"))
	if rec := do(t, h, http.MethodGet, gw.URL("t1", "run-a", "run/in.txt")); rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, gw.URL("t1", "conquest", "run/in.txt")); rec.Code != http.StatusBadGateway {
		t.Fatalf("configured bucket: got %d, want 502", rec.Code)
	}
	if len(store.calls)+len(shared.calls) != 0 {
		t.Fatal("presigned without run storage")
	}
}
