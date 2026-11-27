//go:build integration

// Needs the identities run-a and run-b of testdata/s3.json, each allowed only
// its own bucket (isolation-run-a, isolation-run-b); see
// s3_integration_test.go for how to run it.
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
	"net/url"
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
	"github.com/anuragxxd/conquest/internal/runbucket"
	"github.com/anuragxxd/conquest/internal/storage"
	"github.com/anuragxxd/conquest/internal/storagegw"
)

// runIdentities are the keys of testdata/s3.json for runs a and b.
var runIdentities = map[string]runbucket.Credentials{
	"a": {AccessKeyID: "run-a-key", SecretAccessKey: "run-a-secret"},
	"b": {AccessKeyID: "run-b-key", SecretAccessKey: "run-b-secret"},
}

// identityBuckets gives run <ID> the bucket isolation-run-<ID> and its
// identity's keys.
type identityBuckets struct{ asked []string }

func (p *identityBuckets) Ensure(_ context.Context, runID string) (runbucket.Bucket, error) {
	return runbucket.Bucket{Name: "isolation-run-" + runID, SecretName: "run-" + runID}, nil
}

func (p *identityBuckets) Credentials(_ context.Context, runID string) (runbucket.Credentials, error) {
	p.asked = append(p.asked, runID)
	c, ok := runIdentities[runID]
	if !ok {
		return runbucket.Credentials{}, errors.New("no identity")
	}
	return c, nil
}

func (p *identityBuckets) Delete(context.Context, string) error { return nil }

type runRoots map[string]string

func (r runRoots) Get(_ context.Context, runID string) (*domain.Run, error) {
	root, ok := r[runID]
	if !ok {
		return nil, errors.New("not found")
	}
	return &domain.Run{RunID: runID, RunRootURI: root}, nil
}

// runBucket creates (or reuses) a run's bucket with the admin keys and
// empties it at the end.
func runBucket(t *testing.T, admin *minio.Client, bucket string) {
	t.Helper()
	ctx := context.Background()
	if err := admin.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		if exists, e := admin.BucketExists(ctx, bucket); e != nil || !exists {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for obj := range admin.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = admin.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{})
		}
	})
}

func minioAs(t *testing.T, c runbucket.Credentials) *minio.Client {
	t.Helper()
	endpoint := os.Getenv("CONQUEST_TEST_S3_ENDPOINT")
	mc, err := minio.New(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"), &minio.Options{
		Creds: credentials.NewStaticV4(c.AccessKeyID, c.SecretAccessKey, ""), Secure: strings.HasPrefix(endpoint, "https://"), Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return mc
}

// TestRunBuckets runs two runs in two buckets with two identities: a run's
// keys can't reach the other run's bucket, and the server (storage gateway,
// outputs reads) uses each run's own keys for its bucket.
func TestRunBuckets(t *testing.T) {
	admin, sharedClient, _ := s3Env(t) // skips without a test S3 server
	ctx := context.Background()
	layout := storage.Layout{Bucket: "conquest-shared", Version: "v1"}
	for _, id := range []string{"a", "b"} {
		runBucket(t, admin, "isolation-run-"+id)
	}
	rootA, rootB := layout.RunRoot("a"), layout.RunRoot("b")
	put(t, admin, "isolation-run-a", rootA+"in.txt", "input of a")
	put(t, admin, "isolation-run-b", rootB+"secret.txt", "secret of b")

	// the object store enforces the bound: a run's keys reach only its bucket
	runA := minioAs(t, runIdentities["a"])
	if _, ok := get(runA, "isolation-run-a", rootA+"in.txt"); !ok {
		t.Fatal("run a's keys can't read run a's bucket")
	}
	if got, ok := get(runA, "isolation-run-b", rootB+"secret.txt"); ok {
		t.Fatalf("run a's keys read run b's bucket: %q", got)
	}
	if _, err := runA.PutObject(ctx, "isolation-run-b", rootB+"planted.txt", strings.NewReader("x"), 1, minio.PutObjectOptions{}); err == nil {
		t.Fatal("run a's keys wrote into run b's bucket")
	}

	// the server's access to the runs' buckets
	newAccess := func(prov runbucket.Provisioner) *runbucket.Access {
		t.Helper()
		access, err := runbucket.NewAccess(runbucket.AccessConfig{
			Bucket:      layout.Bucket,
			Credentials: runbucket.Credentials{AccessKeyID: os.Getenv("CONQUEST_TEST_S3_ACCESS_KEY"), SecretAccessKey: os.Getenv("CONQUEST_TEST_S3_SECRET_KEY")},
			Endpoint:    os.Getenv("CONQUEST_TEST_S3_ENDPOINT"),
		}, runRoots{
			"a": "s3://isolation-run-a/" + rootA,
			"b": "s3://isolation-run-b/" + rootB,
		}, prov, func(ep string, c runbucket.Credentials) (runbucket.Client, error) {
			return objstore.NewS3Client(objstore.S3Config{
				Endpoint: strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://"), AccessKey: c.AccessKeyID, SecretKey: c.SecretAccessKey,
				Region: "us-east-1", UseSSL: strings.HasPrefix(ep, "https://"),
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		return access
	}
	access := newAccess(&identityBuckets{})

	// outputs reads: each run's client reads its own bucket, not the other
	for id, key := range map[string]string{"a": rootA + "in.txt", "b": rootB + "secret.txt"} {
		bucket, client, err := access.Client(ctx, id)
		if err != nil {
			t.Fatalf("Client(%s): %v", id, err)
		}
		if r, err := client.Get(ctx, bucket, key); err != nil {
			t.Errorf("run %s reads its bucket: %v", id, err)
		} else {
			_ = r.Close()
		}
	}
	_, clientA, _ := access.Client(ctx, "a")
	// the denial may come from Get or, for a lazy body, from the first read
	if r, err := clientA.Get(ctx, "isolation-run-b", rootB+"secret.txt"); err == nil {
		data, readErr := io.ReadAll(r)
		_ = r.Close()
		if readErr == nil {
			t.Fatalf("run a's client read run b's bucket: %q", data)
		}
	}

	// the storage gateway, wired with the runs' buckets
	put(t, admin, "isolation-run-a", rootA+"dir/x.txt", "x")
	put(t, admin, "isolation-run-a", rootA+"dir/sub/y.txt", "y")
	put(t, admin, "isolation-run-b", rootB+"dir/z.txt", "z")
	task := &domain.Task{TaskID: "t1", RunID: "a", State: domain.StateRunning}
	stolen := tes.Input{URL: "s3://isolation-run-b/" + rootB + "secret.txt", Path: "/work/secret.txt"}
	own := tes.Input{URL: "s3://isolation-run-a/" + rootA + "in.txt", Path: "/work/in.txt"}
	ownDir := tes.Input{URL: "s3://isolation-run-a/" + rootA + "dir", Path: "/work/dir", Type: "DIRECTORY"}
	stolenDir := tes.Input{URL: "s3://isolation-run-b/" + rootB + "dir", Path: "/work/bdir", Type: "DIRECTORY"}
	out := tes.Output{URL: "s3://isolation-run-a/" + rootA + "out.txt", Path: "/work/out.txt"}
	planted := tes.Output{URL: "s3://isolation-run-b/" + rootB + "planted.txt", Path: "/work/planted.txt"}
	task.TaskRequestPayload, _ = json.Marshal(tes.Task{
		Inputs:  []tes.Input{own, stolen, ownDir, stolenDir},
		Outputs: []tes.Output{out, planted},
	})
	gw, err := storagegw.New(storagegw.Config{
		PublicURL: "http://conquest.invalid",
		Secret:    bytes.Repeat([]byte("s"), 32),
		Buckets:   []string{layout.Bucket},
	}, tasks{"t1": task}, sharedClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// a fresh Access, so the provisioner is asked for every key the gateway uses
	gwProv := &identityBuckets{}
	gwAccess := newAccess(gwProv)
	gw.SetBound(isolation.RunBoundIn(layout, gwAccess.Bucket))
	gw.SetLister(refusingLister{}) // enables listings; the run's store must list
	gw.SetRunStorage(func(ctx context.Context, task *domain.Task) (string, storagegw.RunStore, error) {
		bucket, client, err := gwAccess.PresignClient(ctx, task.RunID)
		return bucket, client, err
	})
	router := chi.NewRouter()
	gw.Routes(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	// the redirect is presigned with run a's key
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).
		Get(gw.TaskRoot(srv.URL, "t1") + "/isolation-run-a/" + rootA + "in.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusTemporaryRedirect || loc == nil || !strings.HasPrefix(loc.Query().Get("X-Amz-Credential"), "run-a-key/") {
		t.Fatalf("redirect %d to %v: want one presigned with run a's key", resp.StatusCode, loc)
	}

	client := isolation.NewClient(gw.TaskRoot(srv.URL, "t1"), nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	workdir := t.TempDir()

	// stage-in from the run's bucket works; from the other run's, it doesn't
	own.Path = filepath.Join(workdir, "in.txt")
	if err := filer.StageIn(ctx, client, filer.StageManifest{Workdir: workdir, Inputs: []tes.Input{own}}, log); err != nil {
		t.Fatalf("stage-in from the run's bucket: %v", err)
	}
	if got, err := os.ReadFile(own.Path); err != nil || string(got) != "input of a" {
		t.Fatalf("staged %q, %v", got, err)
	}
	stolen.Path = filepath.Join(workdir, "secret.txt")
	if err := filer.StageIn(ctx, client, filer.StageManifest{Workdir: workdir, Inputs: []tes.Input{stolen}}, log); err == nil {
		t.Error("stage-in from another run's bucket: want an error")
	}

	// directory inputs are listed with the run's keys, only in its bucket
	ownDir.Path = filepath.Join(workdir, "dir")
	if err := filer.StageIn(ctx, client, filer.StageManifest{Workdir: workdir, Inputs: []tes.Input{ownDir}}, log); err != nil {
		t.Fatalf("stage-in of a directory in the run's bucket: %v", err)
	}
	for path, want := range map[string]string{"dir/x.txt": "x", "dir/sub/y.txt": "y"} {
		if got, err := os.ReadFile(filepath.Join(workdir, path)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	stolenDir.Path = filepath.Join(workdir, "bdir")
	if err := filer.StageIn(ctx, client, filer.StageManifest{Workdir: workdir, Inputs: []tes.Input{stolenDir}}, log); err == nil {
		t.Error("stage-in of a directory in another run's bucket: want an error")
	}

	// stage-out into the run's bucket works; into the other run's, it doesn't
	for _, o := range []*tes.Output{&out, &planted} {
		o.Path = filepath.Join(workdir, filepath.Base(o.Path))
		if err := os.WriteFile(o.Path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := filer.StageOut(ctx, client, filer.StageManifest{Workdir: workdir, Outputs: []tes.Output{out}}, log); err != nil {
		t.Fatalf("stage-out into the run's bucket: %v", err)
	}
	if _, ok := get(admin, "isolation-run-a", rootA+"out.txt"); !ok {
		t.Error("output missing in the run's bucket")
	}
	if _, err := filer.StageOut(ctx, client, filer.StageManifest{Workdir: workdir, Outputs: []tes.Output{planted}}, log); err == nil {
		t.Error("stage-out into another run's bucket: want an error")
	}
	if _, ok := get(admin, "isolation-run-b", rootB+"planted.txt"); ok {
		t.Error("object planted in another run's bucket")
	}

	// the gateway asked for run a's keys, and only those
	if len(gwProv.asked) == 0 {
		t.Fatal("the gateway never asked for the run's keys")
	}
	for _, id := range gwProv.asked {
		if id != "a" {
			t.Fatalf("keys of run %s used for run a's task (asked %v)", id, gwProv.asked)
		}
	}
}

// refusingLister fails every listing: the gateway must list with the run's
// store.
type refusingLister struct{}

func (refusingLister) ListPrefix(context.Context, string, string) ([]string, error) {
	return nil, errors.New("listed without the run's credentials")
}
