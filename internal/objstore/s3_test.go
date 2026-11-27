package objstore

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeS3 serves ListObjectsV2 and DeleteObjects for one bucket.
type fakeS3 struct {
	mu        sync.Mutex
	keys      []string
	requests  int
	listFails bool
	failKey   string // DeleteObjects reports an error for this key
	deleted   []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		if f.listFails {
			// not retried, unlike 5xx
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>no</Message></Error>`)
			return
		}
		prefix := q.Get("prefix")
		var b strings.Builder
		b.WriteString(`<ListBucketResult><Name>b</Name><IsTruncated>false</IsTruncated>`)
		for _, k := range f.keys {
			if strings.HasPrefix(k, prefix) {
				fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>1</Size></Contents>`, k)
			}
		}
		b.WriteString(`</ListBucketResult>`)
		fmt.Fprint(w, b.String())
	case r.Method == http.MethodPost && q.Has("delete"):
		var req struct {
			Objects []struct{ Key string } `xml:"Object"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = xml.Unmarshal(body, &req)
		var b strings.Builder
		b.WriteString(`<DeleteResult>`)
		for _, o := range req.Objects {
			if o.Key == f.failKey {
				fmt.Fprintf(&b, `<Error><Key>%s</Key><Code>AccessDenied</Code><Message>no</Message></Error>`, o.Key)
				continue
			}
			f.deleted = append(f.deleted, o.Key)
			fmt.Fprintf(&b, `<Deleted><Key>%s</Key></Deleted>`, o.Key)
		}
		b.WriteString(`</DeleteResult>`)
		fmt.Fprint(w, b.String())
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func fakeClient(t *testing.T, f *fakeS3) *S3Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := NewS3Client(S3Config{Endpoint: strings.TrimPrefix(srv.URL, "http://"), AccessKey: "k", SecretKey: "s", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDeletePrefix_DeletesOnlyTheKeysBelowThePrefix(t *testing.T) {
	f := &fakeS3{keys: []string{"v1/workflow-runs/a/x", "v1/workflow-runs/a/y/z", "v1/workflow-runs/a2/x", "other"}}
	if err := fakeClient(t, f).DeletePrefix(context.Background(), "bkt", "v1/workflow-runs/a/"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if len(f.deleted) != 2 || f.deleted[0] != "v1/workflow-runs/a/x" || f.deleted[1] != "v1/workflow-runs/a/y/z" {
		t.Fatalf("deleted %v", f.deleted)
	}
}

func TestDeletePrefix_RefusesAnEmptyPrefixWithoutARequest(t *testing.T) {
	f := &fakeS3{keys: []string{"x"}}
	if err := fakeClient(t, f).DeletePrefix(context.Background(), "bkt", ""); err == nil {
		t.Fatal("empty prefix accepted: it would empty the bucket")
	}
	if f.requests != 0 {
		t.Fatalf("%d requests sent", f.requests)
	}
}

func TestDeletePrefix_ReportsListErrors(t *testing.T) {
	f := &fakeS3{keys: []string{"p/x"}, listFails: true}
	if err := fakeClient(t, f).DeletePrefix(context.Background(), "bkt", "p/"); err == nil {
		t.Fatal("list error hidden")
	}
}

func TestDeletePrefix_ReportsDeleteErrors(t *testing.T) {
	f := &fakeS3{keys: []string{"p/x", "p/y"}, failKey: "p/x"}
	err := fakeClient(t, f).DeletePrefix(context.Background(), "bkt", "p/")
	if err == nil || !strings.Contains(err.Error(), "p/x") {
		t.Fatalf("err = %v, want the failed key named", err)
	}
}

func TestDeletePrefix_StopsWithTheContext(t *testing.T) {
	f := &fakeS3{keys: []string{"p/x"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fakeClient(t, f).DeletePrefix(ctx, "bkt", "p/"); err == nil {
		t.Fatal("cancelled context: want an error")
	}
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v after cancel", f.deleted)
	}
}
