//go:build integration

// Run against an S3 server that checks signatures, such as MinIO or SeaweedFS
// (as CI does):
//
//	docker run -d --rm -p 127.0.0.1:8333:8333 \
//	  -v $PWD/internal/isolation/testdata/s3.json:/etc/seaweedfs/s3.json:ro \
//	  chrislusf/seaweedfs:4.52 server -dir=/data -s3 -s3.config=/etc/seaweedfs/s3.json
//	CONQUEST_TEST_S3_ENDPOINT=http://127.0.0.1:8333 \
//	CONQUEST_TEST_S3_ACCESS_KEY=conquest-test CONQUEST_TEST_S3_SECRET_KEY=conquest-test-secret \
//	go test -tags integration ./internal/isolation/
package isolation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/anuragxxd/conquest/internal/api/tes"
	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/filer"
	"github.com/anuragxxd/conquest/internal/isolation"
	"github.com/anuragxxd/conquest/internal/objstore"
	"github.com/anuragxxd/conquest/internal/storage"
	"github.com/anuragxxd/conquest/internal/storagegw"
)

type tasks map[string]*domain.Task

func (t tasks) Get(_ context.Context, id string) (*domain.Task, error) {
	if task, ok := t[id]; ok {
		return task, nil
	}
	return nil, errors.New("not found")
}

// s3Env returns a client of the test S3 server and a fresh bucket in it.
func s3Env(t *testing.T) (*minio.Client, *objstore.S3Client, string) {
	t.Helper()
	endpoint := os.Getenv("CONQUEST_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("CONQUEST_TEST_S3_ENDPOINT not set")
	}
	access, secret := os.Getenv("CONQUEST_TEST_S3_ACCESS_KEY"), os.Getenv("CONQUEST_TEST_S3_SECRET_KEY")
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	secure := strings.HasPrefix(endpoint, "https://")
	mc, err := minio.New(host, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: secure, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := objstore.NewS3Client(objstore.S3Config{Endpoint: host, AccessKey: access, SecretKey: secret, Region: "us-east-1", UseSSL: secure})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "isolation-" + strings.ToLower(domain.NewID())
	ctx := context.Background()
	if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for obj := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = mc.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{})
		}
		_ = mc.RemoveBucket(ctx, bucket)
	})
	return mc, client, bucket
}

func put(t *testing.T, mc *minio.Client, bucket, key, data string) {
	t.Helper()
	if _, err := mc.PutObject(context.Background(), bucket, key, strings.NewReader(data), int64(len(data)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
}

func get(mc *minio.Client, bucket, key string) (string, bool) {
	obj, err := mc.GetObject(context.Background(), bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return "", false
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// TestTaskWithoutCredentials stages a task in and out through the gateway,
// as a task pod with storage isolation does, against a real S3 server.
func TestTaskWithoutCredentials(t *testing.T) {
	mc, s3, bucket := s3Env(t)
	layout := storage.Layout{Bucket: bucket, Version: "v1"}
	own := "s3://" + bucket + "/" + layout.RunRoot("r1")
	other := "s3://" + bucket + "/" + layout.RunRoot("r2")
	put(t, mc, bucket, layout.RunRoot("r1")+"work/ab/.command.sh", "echo hi")
	put(t, mc, bucket, layout.RunRoot("r1")+"workflow/bundle/main.nf", "workflow {}")
	put(t, mc, bucket, layout.RunRoot("r1")+"workflow/bundle/lib/a b.groovy", "a")
	put(t, mc, bucket, layout.RunRoot("r2")+"outputs/secret.txt", "secret")
	// the server must check signatures, or the test proves nothing
	if resp, err := http.Get(os.Getenv("CONQUEST_TEST_S3_ENDPOINT") + "/" + bucket + "/" + layout.RunRoot("r2") + "outputs/secret.txt"); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned GET: %v, %v; want 403", resp, err)
	}

	inputs := []tes.Input{
		{URL: own + "work/ab/.command.sh", Path: "/work/.command.sh"},
		{URL: own + "workflow/bundle", Path: "/work/wf", Type: "DIRECTORY"},
	}
	stolen := tes.Input{URL: other + "outputs/secret.txt", Path: "/work/secret.txt"}
	outputs := []tes.Output{
		{URL: own + "work/ab/.exitcode", Path: "/work/.exitcode"},
		{URL: own + "outputs/results", Path: "/work/results", Type: "DIRECTORY"},
	}
	planted := tes.Output{URL: other + "outputs/planted.txt", Path: "/work/planted.txt"}
	payload, _ := json.Marshal(tes.Task{
		Inputs:  append(append([]tes.Input{}, inputs...), stolen),
		Outputs: append(append([]tes.Output{}, outputs...), planted),
	})
	task := &domain.Task{TaskID: "t1", RunID: "r1", State: domain.StateRunning, TaskRequestPayload: payload}

	gw, err := storagegw.New(storagegw.Config{
		PublicURL: "http://conquest.invalid",
		Secret:    bytes.Repeat([]byte("s"), 32),
		Buckets:   []string{bucket},
	}, tasks{"t1": task}, s3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	gw.SetBound(isolation.RunBound(layout))
	gw.SetLister(s3)
	router := chi.NewRouter()
	gw.Routes(router)
	srv := httptest.NewServer(router)
	defer srv.Close()
	client := isolation.NewClient(gw.TaskRoot(srv.URL, "t1"), nil)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// stage-in: own inputs arrive, also below the DIRECTORY input
	workdir := t.TempDir()
	in := filer.StageManifest{Workdir: workdir}
	for _, i := range inputs {
		i.Path = filepath.Join(workdir, strings.TrimPrefix(i.Path, "/work/"))
		in.Inputs = append(in.Inputs, i)
	}
	if err := filer.StageIn(ctx, client, in, log); err != nil {
		t.Fatalf("stage-in: %v", err)
	}
	for path, want := range map[string]string{".command.sh": "echo hi", "wf/main.nf": "workflow {}", "wf/lib/a b.groovy": "a"} {
		if got, err := os.ReadFile(filepath.Join(workdir, path)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}

	// stage-in of another run's object fails, although the request names it
	stolen.Path = filepath.Join(workdir, "secret.txt")
	if err := filer.StageIn(ctx, client, filer.StageManifest{Workdir: workdir, Inputs: []tes.Input{stolen}}, log); err == nil {
		t.Error("stage-in of another run's object: want an error")
	}
	if data, err := os.ReadFile(stolen.Path); err == nil && len(data) > 0 {
		t.Errorf("another run's object was staged: %q", data)
	}

	// stage-out: own outputs are written
	if err := os.WriteFile(filepath.Join(workdir, ".exitcode"), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, ".command.log"), []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workdir, "results", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "results", "sub", "r.txt"), []byte("result"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filer.StageManifest{Workdir: workdir}
	for _, o := range outputs {
		o.Path = filepath.Join(workdir, strings.TrimPrefix(o.Path, "/work/"))
		out.Outputs = append(out.Outputs, o)
	}
	if _, err := filer.StageOut(ctx, client, out, log); err != nil {
		t.Fatalf("stage-out: %v", err)
	}
	for key, want := range map[string]string{
		layout.RunRoot("r1") + "work/ab/.exitcode":         "0",
		layout.RunRoot("r1") + "work/ab/.command.log":      "log", // control file next to a FILE output
		layout.RunRoot("r1") + "outputs/results/sub/r.txt": "result",
	} {
		if got, ok := get(mc, bucket, key); !ok || got != want {
			t.Errorf("%s = %q (%v), want %q", key, got, ok, want)
		}
	}

	// stage-out into another run fails and writes nothing
	if err := os.WriteFile(filepath.Join(workdir, "planted.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	planted.Path = filepath.Join(workdir, "planted.txt")
	if _, err := filer.StageOut(ctx, client, filer.StageManifest{Workdir: workdir, Outputs: []tes.Output{planted}}, log); err == nil {
		t.Error("stage-out into another run: want an error")
	}
	if _, ok := get(mc, bucket, layout.RunRoot("r2")+"outputs/planted.txt"); ok {
		t.Error("object planted in another run")
	}

	// after the task ended (and the grace period), the URL is dead
	task.State = domain.StateComplete
	finished := task.UpdatedAt
	task.FinishedAt = &finished
	if _, err := client.Get(ctx, bucket, layout.RunRoot("r1")+"work/ab/.command.sh"); err == nil || !strings.Contains(err.Error(), "410") {
		t.Errorf("after the task ended: %v", err)
	}
}
