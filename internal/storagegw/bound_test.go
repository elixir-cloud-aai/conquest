package storagegw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/anuragxxd/conquest/internal/domain"
)

type lister struct {
	keys  []string
	calls []string
}

func (l *lister) ListPrefix(_ context.Context, bucket, prefix string) ([]string, error) {
	l.calls = append(l.calls, bucket+"/"+prefix)
	var out []string
	for _, k := range l.keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

func TestTaskRoot(t *testing.T) {
	gw, _, _ := newGateway(t, activeTask())

	root := gw.TaskRoot("http://conquest.ns.svc:8080/", "t1")

	if !strings.HasPrefix(root, "http://conquest.ns.svc:8080/storage/v1/t/t1/") || strings.HasSuffix(root, "/") {
		t.Fatalf("root = %s", root)
	}
	public := gw.URL("t1", "conquest", "run/x")
	if want := strings.TrimPrefix(root, "http://conquest.ns.svc:8080"); !strings.Contains(public, want+"/conquest/run/x") {
		t.Errorf("URL %s isn't below the task root %s", public, want)
	}
}

func TestBound(t *testing.T) {
	task := activeTask()
	task.RunID = "r1"
	task.TaskRequestPayload = json.RawMessage(`{
		"inputs": [
			{"url": "s3://conquest/runs/r1/work/in.txt", "path": "/work/in.txt"},
			{"url": "s3://conquest/runs/r2/outputs/secret.txt", "path": "/work/secret.txt"},
			{"url": "s3://conquest/runs/r10/x.txt", "path": "/work/x.txt"}
		],
		"outputs": [
			{"url": "s3://conquest/runs/r1/work/out.txt", "path": "/work/out.txt"},
			{"url": "s3://conquest/runs/r2/work/out.txt", "path": "/work/out2.txt"}
		]
	}`)
	gw, p, h := newGateway(t, task)
	gw.SetBound(func(_ context.Context, task *domain.Task) (string, error) {
		return "conquest/runs/" + task.RunID + "/", nil
	})

	for _, tc := range []struct {
		method, key string
		want        int
	}{
		{http.MethodGet, "runs/r1/work/in.txt", http.StatusTemporaryRedirect},
		{http.MethodPut, "runs/r1/work/out.txt", http.StatusTemporaryRedirect},
		{http.MethodGet, "runs/r2/outputs/secret.txt", http.StatusForbidden}, // named, but another run's
		{http.MethodPut, "runs/r2/work/out.txt", http.StatusForbidden},
		{http.MethodGet, "runs/r10/x.txt", http.StatusForbidden}, // run ID with r1 as prefix
	} {
		rec := do(t, h, tc.method, gw.URL("t1", "conquest", tc.key))
		if rec.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.key, rec.Code, tc.want)
		}
	}
	if len(p.calls) != 2 {
		t.Errorf("presigned = %v", p.calls)
	}
}

func TestBoundRefusesAll(t *testing.T) {
	for name, bound := range map[string]Bound{
		"empty":  func(context.Context, *domain.Task) (string, error) { return "", nil },
		"failed": func(context.Context, *domain.Task) (string, error) { return "", errors.New("boom") },
	} {
		gw, p, h := newGateway(t, activeTask())
		gw.SetBound(bound)
		rec := do(t, h, http.MethodGet, gw.URL("t1", "conquest", "run/work/ab/.command.sh"))
		if rec.Code < 400 || len(p.calls) != 0 {
			t.Errorf("%s: status = %d, presigned = %v", name, rec.Code, p.calls)
		}
	}
}

func TestList(t *testing.T) {
	task := activeTask()
	task.RunID = "run"
	gw, p, h := newGateway(t, task)
	l := &lister{keys: []string{"run/bundle/Snakefile", "run/bundle/rules/a.smk", "run/bundle-other/x"}}
	gw.SetLister(l)
	gw.SetBound(func(context.Context, *domain.Task) (string, error) { return "conquest/run/", nil })

	rec := do(t, h, http.MethodGet, gw.URL("t1", "conquest", "run/bundle/"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if want := []string{"run/bundle/Snakefile", "run/bundle/rules/a.smk"}; !reflect.DeepEqual(got.Keys, want) {
		t.Errorf("keys = %v, want %v", got.Keys, want)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || len(p.calls) != 0 {
		t.Errorf("headers = %v, presigned = %v", rec.Header(), p.calls)
	}

	for _, key := range []string{
		"run/bundle/rules/", // below a DIRECTORY input: allowed
	} {
		if rec := do(t, h, http.MethodGet, gw.URL("t1", "conquest", key)); rec.Code != http.StatusOK {
			t.Errorf("list %s: status = %d", key, rec.Code)
		}
	}
	for _, key := range []string{
		"run/",              // above the input
		"run/work/ab/",      // directory of a FILE input
		"run/results/dir/",  // an output
		"run/bundle-other/", // sibling
		"other-run/bundle/", // outside the bound
	} {
		if rec := do(t, h, http.MethodGet, gw.URL("t1", "conquest", key)); rec.Code != http.StatusForbidden {
			t.Errorf("list %s: status = %d, want 403", key, rec.Code)
		}
	}
}

func TestListNeedsLister(t *testing.T) {
	gw, p, h := newGateway(t, activeTask())

	// Without a lister a prefix is presigned like any key (upstream behaviour).
	rec := do(t, h, http.MethodGet, gw.URL("t1", "conquest", "run/bundle/"))

	if rec.Code != http.StatusTemporaryRedirect || len(p.calls) != 1 {
		t.Errorf("status = %d, presigned = %v", rec.Code, p.calls)
	}
}
