package isolation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/storage"
	"github.com/anuragxxd/conquest/internal/storagegw"
)

// fakeS3 serves presigned requests from memory; a request without the
// presigner's signature is refused, as by S3.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte // "bucket/key"
	srv     *httptest.Server
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{objects: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sig") != "ok" || r.URL.Query().Get("method") != r.Method {
			http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
			return
		}
		obj := strings.TrimPrefix(r.URL.Path, "/")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			data, ok := f.objects[obj]
			if !ok {
				http.Error(w, "NoSuchKey", http.StatusNotFound)
				return
			}
			_, _ = w.Write(data)
		case http.MethodPut:
			if r.ContentLength < 0 {
				http.Error(w, "MissingContentLength", http.StatusLengthRequired)
				return
			}
			data, _ := io.ReadAll(r.Body)
			f.objects[obj] = data
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) Presign(_ context.Context, method, bucket, key string, _ time.Duration) (*url.URL, error) {
	u, _ := url.Parse(f.srv.URL)
	u.Path = "/" + bucket + "/" + key
	u.RawQuery = url.Values{"sig": {"ok"}, "method": {method}}.Encode()
	return u, nil
}

func (f *fakeS3) ListPrefix(_ context.Context, bucket, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for obj := range f.objects {
		if key, ok := strings.CutPrefix(obj, bucket+"/"); ok && strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

type tasks map[string]*domain.Task

func (t tasks) Get(_ context.Context, id string) (*domain.Task, error) {
	if task, ok := t[id]; ok {
		return task, nil
	}
	return nil, errors.New("not found")
}

var layout = storage.Layout{Bucket: "conquest", Version: "v1"}

const taskRequest = `{
	"inputs": [
		{"url": "s3://conquest/v1/workflow-runs/r1/work/ab/.command.sh", "path": "/work/.command.sh"},
		{"url": "s3://conquest/v1/workflow-runs/r1/workflow/bundle", "path": "/work/wf", "type": "DIRECTORY"},
		{"url": "s3://conquest/v1/workflow-runs/r2/outputs/secret.txt", "path": "/work/secret.txt"},
		{"url": "s3://conquest/v1/workflow-runs/r2/workflow/bundle", "path": "/work/wf2", "type": "DIRECTORY"}
	],
	"outputs": [
		{"url": "s3://conquest/v1/workflow-runs/r1/work/ab/.exitcode", "path": "/work/.exitcode"},
		{"url": "s3://conquest/v1/workflow-runs/r2/outputs/planted.txt", "path": "/work/planted.txt"}
	]
}`

// setup returns a client for task t1 of run r1, the fake S3 behind it, and
// the gateway's internal base URL.
func setup(t *testing.T) (*Client, *fakeS3) {
	t.Helper()
	s3 := newFakeS3(t)
	s3.objects["conquest/v1/workflow-runs/r1/work/ab/.command.sh"] = []byte("echo hi")
	s3.objects["conquest/v1/workflow-runs/r1/workflow/bundle/main.nf"] = []byte("workflow {}")
	s3.objects["conquest/v1/workflow-runs/r1/workflow/bundle/lib/a.groovy"] = []byte("a")
	s3.objects["conquest/v1/workflow-runs/r1/workflow/bundle-other/x"] = []byte("x")
	s3.objects["conquest/v1/workflow-runs/r2/outputs/secret.txt"] = []byte("secret")
	s3.objects["conquest/v1/workflow-runs/r2/workflow/bundle/main.nf"] = []byte("secret")

	task := &domain.Task{TaskID: "t1", RunID: "r1", State: domain.StateRunning, TaskRequestPayload: json.RawMessage(taskRequest)}
	gw, err := storagegw.New(storagegw.Config{
		PublicURL: "https://conquest.example.org",
		Secret:    []byte(strings.Repeat("s", 32)),
		Buckets:   []string{"conquest"},
	}, tasks{"t1": task}, s3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	gw.SetBound(RunBound(layout))
	gw.SetLister(s3)
	r := chi.NewRouter()
	gw.Routes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return NewClient(gw.TaskRoot(srv.URL, "t1"), nil), s3
}

func TestGet(t *testing.T) {
	c, _ := setup(t)

	rc, err := c.Get(context.Background(), "conquest", "v1/workflow-runs/r1/work/ab/.command.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if data, _ := io.ReadAll(rc); string(data) != "echo hi" {
		t.Errorf("data = %q", data)
	}
}

func TestPut(t *testing.T) {
	c, s3 := setup(t)
	key := "v1/workflow-runs/r1/work/ab/.exitcode"

	if err := c.Put(context.Background(), "conquest", key, strings.NewReader("0"), 1); err != nil {
		t.Fatal(err)
	}
	if got := string(s3.objects["conquest/"+key]); got != "0" {
		t.Errorf("stored %q", got)
	}
	if err := c.Put(context.Background(), "conquest", key, bytes.NewReader(nil), 0); err != nil {
		t.Errorf("empty object: %v", err)
	}
	if err := c.Put(context.Background(), "conquest", key, strings.NewReader("0"), -1); err == nil {
		t.Error("unknown size: want an error")
	}
}

func TestExists(t *testing.T) {
	c, _ := setup(t)

	ok, err := c.Exists(context.Background(), "conquest", "v1/workflow-runs/r1/work/ab/.command.sh")
	if err != nil || !ok {
		t.Errorf("existing input: %v, %v", ok, err)
	}
	ok, err = c.Exists(context.Background(), "conquest", "v1/workflow-runs/r1/workflow/bundle/missing")
	if err != nil || ok {
		t.Errorf("missing object below a DIRECTORY input: %v, %v", ok, err)
	}
}

func TestListPrefix(t *testing.T) {
	c, _ := setup(t)

	keys, err := c.ListPrefix(context.Background(), "conquest", "v1/workflow-runs/r1/workflow/bundle")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"v1/workflow-runs/r1/workflow/bundle/lib/a.groovy", "v1/workflow-runs/r1/workflow/bundle/main.nf"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %v, want %v (bundle-other must not be listed)", keys, want)
	}
}

// A task request may name any object; the run bound still keeps it in its run.
func TestOtherRunsAreRefused(t *testing.T) {
	c, s3 := setup(t)
	ctx := context.Background()

	if _, err := c.Get(ctx, "conquest", "v1/workflow-runs/r2/outputs/secret.txt"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("read of another run: %v", err)
	}
	if _, err := c.ListPrefix(ctx, "conquest", "v1/workflow-runs/r2/workflow/bundle"); err == nil {
		t.Error("listing of another run: want an error")
	}
	if err := c.Put(ctx, "conquest", "v1/workflow-runs/r2/outputs/planted.txt", strings.NewReader("x"), 1); err == nil {
		t.Error("write into another run: want an error")
	}
	if _, ok := s3.objects["conquest/v1/workflow-runs/r2/outputs/planted.txt"]; ok {
		t.Error("object planted in another run")
	}
}

func TestErrorsHideCapabilities(t *testing.T) {
	c, _ := setup(t)
	_, sig, _ := strings.Cut(strings.TrimPrefix(c.root, "http://"), "/storage/v1/t/t1/")

	_, err := c.Get(context.Background(), "conquest", "v1/workflow-runs/r2/outputs/secret.txt")
	if err == nil || strings.Contains(err.Error(), sig) {
		t.Errorf("error = %v", err)
	}
	unreachable := NewClient("http://127.0.0.1:1/storage/v1/t/t1/"+sig, nil)
	_, err = unreachable.Get(context.Background(), "conquest", "k")
	if err == nil || strings.Contains(err.Error(), sig) {
		t.Errorf("connection error = %v", err)
	}
}

func TestDeleteUnsupported(t *testing.T) {
	c, _ := setup(t)

	if err := c.Delete(context.Background(), "conquest", "k"); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
}

func TestClientFromEnv(t *testing.T) {
	t.Setenv(EnvTaskStorageURL, "")
	if _, ok := ClientFromEnv(); ok {
		t.Error("unset: want no client")
	}
	t.Setenv(EnvTaskStorageURL, "http://conquest.ns.svc/storage/v1/t/t1/sig/")
	c, ok := ClientFromEnv()
	if !ok || c.root != "http://conquest.ns.svc/storage/v1/t/t1/sig" {
		t.Errorf("client = %+v, %v", c, ok)
	}
}

func TestRunBound(t *testing.T) {
	bound := RunBound(layout)
	for _, tc := range []struct {
		task *domain.Task
		want string
	}{
		{&domain.Task{RunID: "r1"}, "conquest/v1/workflow-runs/r1/"},
		{&domain.Task{}, ""},
		{nil, ""},
	} {
		if got, err := bound(context.Background(), tc.task); err != nil || got != tc.want {
			t.Errorf("bound(%+v) = %q, %v; want %q", tc.task, got, err, tc.want)
		}
	}
	if got, _ := RunBound(storage.Layout{})(context.Background(), &domain.Task{RunID: "r1"}); got != "" {
		t.Errorf("no bucket: bound = %q", got)
	}
}

// errorServer answers every request with the status (and Location) given.
func errorServer(t *testing.T, status int, location, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL+"/storage/v1/t/t1/sig", nil)
}

func TestClientErrors(t *testing.T) {
	ctx := context.Background()
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "AccessDenied", http.StatusForbidden)
	}))
	defer s3.Close()

	for name, c := range map[string]*Client{
		"gateway refuses":         errorServer(t, http.StatusForbidden, "", "object not in the task's inputs or outputs"),
		"task ended":              errorServer(t, http.StatusGone, "", "task has ended"),
		"redirect without target": errorServer(t, http.StatusTemporaryRedirect, "", ""),
		"presigned URL refused":   errorServer(t, http.StatusTemporaryRedirect, s3.URL+"/b/k", ""),
	} {
		if _, err := c.Get(ctx, "b", "k"); err == nil {
			t.Errorf("%s: Get: want an error", name)
		}
		if err := c.Put(ctx, "b", "k", strings.NewReader("x"), 1); err == nil {
			t.Errorf("%s: Put: want an error", name)
		}
		if _, err := c.Exists(ctx, "b", "k"); err == nil {
			t.Errorf("%s: Exists: want an error", name)
		}
	}
	if _, err := errorServer(t, http.StatusOK, "", "not json").ListPrefix(ctx, "b", "p"); err == nil {
		t.Error("ListPrefix of malformed JSON: want an error")
	}
	if _, err := errorServer(t, http.StatusForbidden, "", "no").ListPrefix(ctx, "b", "p"); err == nil {
		t.Error("ListPrefix refused: want an error")
	}
}
