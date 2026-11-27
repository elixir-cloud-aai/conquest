package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/anuragxxd/conquest/internal/backend/remote"
	"github.com/anuragxxd/conquest/internal/backend/routing"
	"github.com/anuragxxd/conquest/internal/config"
	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/runbucket"
	"github.com/anuragxxd/conquest/internal/storage"
)

var testCreds = runbucket.Credentials{AccessKeyID: "server-key", SecretAccessKey: "server-secret"}

func TestNewRunBuckets_SharedUsesTheConfiguredBucketAndSecret(t *testing.T) {
	p, err := newRunBuckets(&config.Config{
		RunBuckets:        runbucket.ModeShared,
		S3Bucket:          "conquest-workflows",
		StorageSecretName: "storage-secrets",
	}, testCreds, nil)
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
	if creds, err := p.Credentials(context.Background(), "run-1"); err != nil || creds != testCreds {
		t.Fatalf("Credentials = %v, %v; want the server's", creds, err)
	}
}

func TestNewRunBuckets_RefusesUnknownModes(t *testing.T) {
	if _, err := newRunBuckets(&config.Config{RunBuckets: "per-run", S3Bucket: "b", StorageSecretName: "s"}, testCreds, nil); err == nil {
		t.Fatal("newRunBuckets accepted an unknown mode")
	}
}

func TestNewRunBuckets_OBC(t *testing.T) {
	cfg := &config.Config{
		RunBuckets:    runbucket.ModeOBC,
		S3Bucket:      "conquest-workflows",
		RunBucketsOBC: runbucket.OBCConfig{Namespace: "conquest", StorageClassName: "sc", BindTimeout: time.Second},
	}
	clients := func() (dynamic.Interface, kubernetes.Interface, error) {
		return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), kubefake.NewSimpleClientset(), nil
	}
	p, err := newRunBuckets(cfg, testCreds, clients)
	if err != nil {
		t.Fatalf("newRunBuckets: %v", err)
	}
	if _, ok := p.(*runbucket.OBC); !ok {
		t.Fatalf("newRunBuckets = %T, want *runbucket.OBC", p)
	}

	if _, err := newRunBuckets(cfg, testCreds, nil); err == nil {
		t.Fatal("obc without Kubernetes clients: want an error")
	}
	failing := func() (dynamic.Interface, kubernetes.Interface, error) {
		return nil, nil, errors.New("not in a cluster")
	}
	if _, err := newRunBuckets(cfg, testCreds, failing); err == nil || !strings.Contains(err.Error(), "not in a cluster") {
		t.Fatalf("obc outside a cluster: %v", err)
	}
}

func TestNewRunAccess(t *testing.T) {
	cfg := &config.Config{S3Bucket: "conquest", S3Endpoint: "http://s3.internal:8333", S3Region: "us-east-1"}
	if a, err := newRunAccess(&config.Config{S3Bucket: "conquest"}, testCreds, runsByRoot{}, nil); a != nil || err != nil {
		t.Fatalf("without endpoint: %v, %v; want nothing", a, err)
	}
	if _, err := newRunAccess(cfg, runbucket.Credentials{}, runsByRoot{}, nil); err == nil {
		t.Fatal("without credentials: want an error")
	}
	a, err := newRunAccess(cfg, testCreds, runsByRoot{"r1": "s3://conquest/v1/workflow-runs/r1/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	bucket, _, err := a.Client(context.Background(), "r1")
	if err != nil || bucket != "conquest" {
		t.Fatalf("Client = %s, %v", bucket, err)
	}
}

func TestCheckFederationRunBuckets(t *testing.T) {
	fed := func(styles ...string) *routing.Config {
		c := &routing.Config{}
		for _, s := range styles {
			c.Endpoints = append(c.Endpoints, remote.EndpointConfig{Name: "ep-" + s, S3URLStyle: s})
		}
		return c
	}
	perRun := runbucket.Mode("obc") // any mode that gives runs their own buckets
	for _, tc := range []struct {
		mode runbucket.Mode
		fed  *routing.Config
		ok   bool
	}{
		{runbucket.ModeShared, fed(remote.S3StyleBucket, remote.S3StyleEndpoint), true},
		{perRun, nil, true},
		{perRun, fed(remote.S3StyleGateway, remote.S3StyleGateway), true},
		{perRun, fed(remote.S3StyleGateway, remote.S3StyleBucket), false},
		{perRun, fed(remote.S3StyleEndpoint), false},
	} {
		if err := checkFederationRunBuckets(tc.mode, tc.fed); (err == nil) != tc.ok {
			t.Errorf("checkFederationRunBuckets(%s, %+v) = %v, want ok %v", tc.mode, tc.fed, err, tc.ok)
		}
	}
}

// runsByRoot maps run IDs to run roots.
type runsByRoot map[string]string

func (r runsByRoot) Get(_ context.Context, runID string) (*domain.Run, error) {
	root, ok := r[runID]
	if !ok {
		return nil, errors.New("not found")
	}
	return &domain.Run{RunID: runID, RunRootURI: root}, nil
}

type taskByID map[string]*domain.Task

func (t taskByID) Get(_ context.Context, id string) (*domain.Task, error) {
	if task, ok := t[id]; ok {
		return task, nil
	}
	return nil, errors.New("not found")
}

// perRunBuckets gives run <ID> the keys "key-<ID>".
type perRunBuckets struct{}

func (perRunBuckets) Ensure(_ context.Context, runID string) (runbucket.Bucket, error) {
	return runbucket.Bucket{Name: "run-" + runID, SecretName: "run-" + runID}, nil
}

func (perRunBuckets) Credentials(_ context.Context, runID string) (runbucket.Credentials, error) {
	return runbucket.Credentials{AccessKeyID: "key-" + runID, SecretAccessKey: "secret-" + runID}, nil
}

func (perRunBuckets) Delete(context.Context, string) error { return nil }

// gatewayTest wires the storage gateway as the server does, for task t1 of
// run a (bucket run-a) and task t0 without a run.
func gatewayTest(t *testing.T) (*config.Config, http.Handler, func(taskID, bucket, key string) string, remote.Storage) {
	t.Helper()
	cfg := &config.Config{
		S3Bucket:             "conquest",
		S3Endpoint:           "http://s3.internal:8333",
		S3PublicEndpoint:     "http://s3.example.org",
		S3Region:             "us-east-1",
		StorageIsolation:     true,
		StorageGatewaySecret: strings.Repeat("s", 32),
		InternalBaseURL:      "http://conquest.internal:8080",
	}
	t.Setenv("AWS_ACCESS_KEY_ID", testCreds.AccessKeyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", testCreds.SecretAccessKey)
	runs := runsByRoot{"a": "s3://run-a/v1/workflow-runs/a/", "b": "s3://run-b/v1/workflow-runs/b/", "stub": ""}
	access, err := newRunAccess(cfg, testCreds, runs, perRunBuckets{})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"inputs": [
		{"url": "s3://run-a/v1/workflow-runs/a/in.txt", "path": "/work/in.txt"},
		{"url": "s3://run-b/v1/workflow-runs/b/in.txt", "path": "/work/b.txt"},
		{"url": "s3://conquest/v1/workflow-runs/a/in.txt", "path": "/work/shared.txt"},
		{"url": "s3://conquest/v1/workflow-runs/stub/in.txt", "path": "/work/stub.txt"}
	]}`)
	tasks := taskByID{
		"t1": {TaskID: "t1", RunID: "a", State: domain.StateRunning, TaskRequestPayload: payload},
		"t0": {TaskID: "t0", State: domain.StateRunning, TaskRequestPayload: payload},
		// standalone TES task: its stub run has no run root
		"t2": {TaskID: "t2", RunID: "stub", State: domain.StateRunning, TaskRequestPayload: payload},
	}
	gw, st, err := newStorageGateway(cfg, storage.Layout{Bucket: "conquest", Version: "v1"}, tasks, access,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	gw.Routes(router)
	path := func(taskID, bucket, key string) string {
		return strings.TrimPrefix(gw.URL(taskID, bucket, key), cfg.InternalBaseURL)
	}
	return cfg, router, path, st
}

// The storage gateway presigns each task's objects in its run's bucket with
// the run's keys, and refuses Conquest's bucket and other runs' buckets.
func TestStorageGateway_PresignsWithTheRunsCredentials(t *testing.T) {
	_, router, path, _ := gatewayTest(t)
	get := func(taskID, bucket, key string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path(taskID, bucket, key), nil))
		return rec
	}

	rec := get("t1", "run-a", "v1/workflow-runs/a/in.txt")
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("own bucket: %d %s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://s3.example.org/run-a/v1/workflow-runs/a/in.txt?") || !strings.Contains(loc, "key-a%2F") {
		t.Fatalf("presigned URL %s: want the public endpoint and run a's key", loc)
	}
	for bucket, key := range map[string]string{"run-b": "v1/workflow-runs/b/in.txt", "conquest": "v1/workflow-runs/a/in.txt"} {
		if rec := get("t1", bucket, key); rec.Code != http.StatusForbidden {
			t.Errorf("bucket %s: %d, want 403", bucket, rec.Code)
		}
	}
	// with storage isolation, a task without a run reaches nothing
	if rec := get("t0", "conquest", "v1/workflow-runs/a/in.txt"); rec.Code != http.StatusForbidden {
		t.Errorf("task without a run: %d, want 403", rec.Code)
	}
	// a standalone TES task reaches its stub run's root in the configured
	// bucket, with the server's key, and nothing else
	rec = get("t2", "conquest", "v1/workflow-runs/stub/in.txt")
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusTemporaryRedirect || !strings.Contains(loc, "server-key%2F") {
		t.Errorf("standalone task: %d to %s, want a redirect with the server's key", rec.Code, loc)
	}
	if rec := get("t2", "conquest", "v1/workflow-runs/a/in.txt"); rec.Code != http.StatusForbidden {
		t.Errorf("standalone task, another run's root: %d, want 403", rec.Code)
	}
}

// Remote backends list a task's directory inputs only in its run's bucket.
func TestStorageGateway_ListsOnlyTheRunsBucket(t *testing.T) {
	_, _, _, st := gatewayTest(t)
	tl, ok := st.(remote.TaskLister)
	if !ok {
		t.Fatal("gateway storage lists without the task")
	}
	if _, err := tl.ListTaskPrefix(context.Background(), "t1", "run-b", "v1/workflow-runs/b/"); err == nil {
		t.Fatal("listing another run's bucket: want an error")
	}
	if _, err := tl.ListTaskPrefix(context.Background(), "missing", "run-a", "v1/"); err == nil {
		t.Fatal("listing for an unknown task: want an error")
	}
	// with storage isolation, listings stay below the task's run root: a
	// standalone task can't list the configured bucket or other runs' roots
	for _, prefix := range []string{"", "v1/", "v1/workflow-runs/a/"} {
		if _, err := tl.ListTaskPrefix(context.Background(), "t2", "conquest", prefix); err == nil || !strings.Contains(err.Error(), "outside the bound") {
			t.Errorf("standalone task listing conquest/%s: %v, want outside the bound", prefix, err)
		}
	}
	if _, err := tl.ListTaskPrefix(context.Background(), "t1", "run-a", "v1/"); err == nil || !strings.Contains(err.Error(), "outside the bound") {
		t.Errorf("listing above the run root: %v, want outside the bound", err)
	}
}
