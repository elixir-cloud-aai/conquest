package runbucket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/objstore"
)

// runs maps run IDs to run roots and counts lookups.
type runs struct {
	roots map[string]string
	gets  int
}

func (r *runs) Get(_ context.Context, runID string) (*domain.Run, error) {
	r.gets++
	root, ok := r.roots[runID]
	if !ok {
		return nil, errors.New("not found")
	}
	return &domain.Run{RunID: runID, RunRootURI: root}, nil
}

// perRun gives run <ID> the keys "key-<ID>"/"secret-<ID>".
type perRun struct {
	err   error
	calls []string
}

func (p *perRun) Ensure(_ context.Context, runID string) (Bucket, error) {
	return Bucket{Name: "run-" + runID, SecretName: "run-" + runID}, nil
}

func (p *perRun) Credentials(_ context.Context, runID string) (Credentials, error) {
	p.calls = append(p.calls, runID)
	if p.err != nil {
		return Credentials{}, p.err
	}
	return Credentials{AccessKeyID: "key-" + runID, SecretAccessKey: "secret-" + runID}, nil
}

func (p *perRun) Delete(context.Context, string) error { return nil }

// fakeClient records how it was made.
type fakeClient struct {
	objstore.Client
	endpoint string
	creds    Credentials
}

func (f *fakeClient) Presign(context.Context, string, string, string, time.Duration) (*url.URL, error) {
	return nil, errors.New("not implemented")
}

func newAccess(t *testing.T, r *runs, p Provisioner) (*Access, *int) {
	t.Helper()
	made := 0
	a, err := NewAccess(AccessConfig{
		Bucket:         "conquest",
		Credentials:    Credentials{AccessKeyID: "server-key", SecretAccessKey: "server-secret"},
		Endpoint:       "http://s3.internal:8333",
		PublicEndpoint: "https://s3.example.org",
	}, r, p, func(endpoint string, creds Credentials) (Client, error) {
		made++
		return &fakeClient{endpoint: endpoint, creds: creds}, nil
	})
	if err != nil {
		t.Fatalf("NewAccess: %v", err)
	}
	return a, &made
}

func twoRuns() *runs {
	return &runs{roots: map[string]string{
		"a":   "s3://run-a/v1/workflow-runs/a/",
		"b":   "s3://run-b/v1/workflow-runs/b/",
		"old": "s3://conquest/v1/workflow-runs/old/",
		"bad": "/v1/workflow-runs/bad/",
		// stub run of a standalone TES task: no run root
		"stub": "",
		// a later provisioner might put two runs into one bucket
		"c1": "s3://run-c/v1/workflow-runs/c1/",
		"c2": "s3://run-c/v1/workflow-runs/c2/",
	}}
}

// Stub runs of standalone TES tasks have no run root: they are in the
// configured bucket, reached with the server's keys.
func TestAccess_RunsWithoutARunRootAreInTheConfiguredBucket(t *testing.T) {
	prov := &perRun{}
	a, _ := newAccess(t, twoRuns(), prov)
	bucket, c, err := a.Client(context.Background(), "stub")
	if err != nil {
		t.Fatal(err)
	}
	if f := c.(*fakeClient); bucket != "conquest" || f.creds.AccessKeyID != "server-key" || len(prov.calls) != 0 {
		t.Fatalf("Client(stub) = %s, %+v, provisioner asked %v", bucket, f, prov.calls)
	}
}

// Clients for buckets other than the configured one are per run: two runs
// in one such bucket never share keys.
func TestAccess_RunsSharingABucketKeepTheirOwnKeys(t *testing.T) {
	a, _ := newAccess(t, twoRuns(), &perRun{})
	for _, id := range []string{"c1", "c2"} {
		_, c, err := a.Client(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if f := c.(*fakeClient); f.creds.AccessKeyID != "key-"+id {
			t.Fatalf("Client(%s) keys %s", id, f.creds.AccessKeyID)
		}
	}
}

// A full cache is emptied and lookups go on.
func TestAccess_FullCacheIsEmptied(t *testing.T) {
	r := &runs{roots: map[string]string{}}
	for i := range maxCached + 1 {
		id := fmt.Sprint(i)
		r.roots[id] = "s3://run-" + id + "/v1/workflow-runs/" + id + "/"
	}
	a, _ := newAccess(t, r, &perRun{})
	for i := range maxCached + 1 {
		if _, _, err := a.Client(context.Background(), fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.buckets) > maxCached || len(a.clients) > maxCached {
		t.Fatalf("caches hold %d buckets, %d clients", len(a.buckets), len(a.clients))
	}
	if bucket, _, err := a.Client(context.Background(), "0"); err != nil || bucket != "run-0" {
		t.Fatalf("after emptying: %s, %v", bucket, err)
	}
}

// Each run's client gets its own bucket and the run's own keys.
func TestAccess_ClientUsesTheRunsBucketAndCredentials(t *testing.T) {
	prov := &perRun{}
	a, _ := newAccess(t, twoRuns(), prov)
	for _, id := range []string{"a", "b"} {
		bucket, c, err := a.Client(context.Background(), id)
		if err != nil {
			t.Fatalf("Client(%s): %v", id, err)
		}
		f := c.(*fakeClient)
		if bucket != "run-"+id || f.creds.AccessKeyID != "key-"+id || f.endpoint != "http://s3.internal:8333" {
			t.Fatalf("Client(%s) = %s, %+v", id, bucket, f)
		}
	}
}

// Presigning uses the public endpoint, with the run's keys.
func TestAccess_PresignClientUsesThePublicEndpoint(t *testing.T) {
	a, _ := newAccess(t, twoRuns(), &perRun{})
	bucket, c, err := a.PresignClient(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if f := c.(*fakeClient); bucket != "run-a" || f.endpoint != "https://s3.example.org" || f.creds.AccessKeyID != "key-a" {
		t.Fatalf("PresignClient = %s, %+v", bucket, f)
	}
}

// Runs in the configured bucket, such as runs from before a switch to
// per-run buckets, are reached with the server's keys, without asking the
// provisioner.
func TestAccess_RunsInTheConfiguredBucketUseTheServersCredentials(t *testing.T) {
	prov := &perRun{}
	a, _ := newAccess(t, twoRuns(), prov)
	bucket, c, err := a.Client(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if f := c.(*fakeClient); bucket != "conquest" || f.creds.AccessKeyID != "server-key" {
		t.Fatalf("Client(old) = %s, %+v", bucket, f)
	}
	if len(prov.calls) != 0 {
		t.Fatalf("provisioner asked for %v", prov.calls)
	}
}

// Buckets and clients are cached: one run lookup and one client per run and
// endpoint.
func TestAccess_Caches(t *testing.T) {
	r := twoRuns()
	prov := &perRun{}
	a, made := newAccess(t, r, prov)
	for range 3 {
		if _, _, err := a.Client(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
	}
	if r.gets != 1 || *made != 1 || len(prov.calls) != 1 {
		t.Fatalf("gets=%d clients=%d credentials=%d, want 1 each", r.gets, *made, len(prov.calls))
	}
}

func TestAccess_FailsClosed(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		prov  Provisioner
		runID string
		want  string
	}{
		"unknown run":             {&perRun{}, "missing", "read run"},
		"no run ID":               {&perRun{}, "", "run ID"},
		"run root without bucket": {&perRun{}, "bad", "not an s3://"},
		"no provisioner":          {nil, "a", "no provisioner"},
		"provisioner error":       {&perRun{err: errors.New("boom")}, "a", "boom"},
		"empty credentials":       {&emptyCreds{}, "a", "no credentials"},
	} {
		t.Run(name, func(t *testing.T) {
			a, made := newAccess(t, twoRuns(), tc.prov)
			if _, _, err := a.Client(ctx, tc.runID); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if *made != 0 {
				t.Fatal("made a client")
			}
		})
	}
}

type emptyCreds struct{ perRun }

func (*emptyCreds) Credentials(context.Context, string) (Credentials, error) {
	return Credentials{AccessKeyID: "key"}, nil
}

func TestAccess_ConfiguredBucketWithoutServerCredentials(t *testing.T) {
	a, err := NewAccess(AccessConfig{Bucket: "conquest", Endpoint: "http://s3"}, twoRuns(), &perRun{},
		func(string, Credentials) (Client, error) { return &fakeClient{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Client(context.Background(), "old"); err == nil {
		t.Fatal("want an error without the server's keys")
	}
}

func TestNewAccess_Validates(t *testing.T) {
	newClient := func(string, Credentials) (Client, error) { return &fakeClient{}, nil }
	for name, tc := range map[string]struct {
		cfg       AccessConfig
		runs      RunLookup
		newClient NewClientFunc
	}{
		"no bucket":         {AccessConfig{Endpoint: "http://s3"}, twoRuns(), newClient},
		"no endpoint":       {AccessConfig{Bucket: "b"}, twoRuns(), newClient},
		"no run lookup":     {AccessConfig{Bucket: "b", Endpoint: "http://s3"}, nil, newClient},
		"no client factory": {AccessConfig{Bucket: "b", Endpoint: "http://s3"}, twoRuns(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAccess(tc.cfg, tc.runs, nil, tc.newClient); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// Credentials never print or log their secret key.
func TestCredentials_HideTheSecretKey(t *testing.T) {
	c := Credentials{AccessKeyID: "AKIA", SecretAccessKey: "top-secret"}
	var logged strings.Builder
	slog.New(slog.NewTextHandler(&logged, nil)).Info("creds", "c", c)
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("creds", "c", c)
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %s %#v", c, c, c, c), logged.String()} {
		if strings.Contains(s, "top-secret") {
			t.Fatalf("secret key in %q", s)
		}
		if !strings.Contains(s, "AKIA") {
			t.Fatalf("access key missing in %q", s)
		}
	}
}

func TestBucketOf(t *testing.T) {
	for in, want := range map[string]string{
		"s3://conquest/v1/workflow-runs/r/": "conquest",
		"s3://run-a/":                       "run-a",
		"":                                  "",
		"s3:///v1/":                         "",
		"https://conquest/v1/":              "",
	} {
		got, err := BucketOf(in)
		if got != want || (err != nil) != (want == "") {
			t.Errorf("BucketOf(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestCheckDeployment(t *testing.T) {
	perRunMode := Mode("obc") // any mode that can give runs their own buckets
	for _, tc := range []struct {
		mode      Mode
		isolation bool
		workspace string
		ok        bool
	}{
		{ModeShared, false, "objectfs", true},
		{ModeShared, true, "emptydir", true},
		{perRunMode, true, "emptydir", true},
		{perRunMode, true, "pvc", true},
		{perRunMode, false, "emptydir", false},
		{perRunMode, true, "objectfs", false},
	} {
		if err := CheckDeployment(tc.mode, tc.isolation, tc.workspace); (err == nil) != tc.ok {
			t.Errorf("CheckDeployment(%s, %v, %s) = %v, want ok %v", tc.mode, tc.isolation, tc.workspace, err, tc.ok)
		}
	}
}

func TestShared_Credentials(t *testing.T) {
	p, err := NewShared("conquest", "storage-secrets")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Credentials(context.Background(), "r"); err == nil {
		t.Fatal("want an error without credentials")
	}
	want := Credentials{AccessKeyID: "k", SecretAccessKey: "s"}
	p.SetCredentials(want)
	if got, err := p.Credentials(context.Background(), "r"); err != nil || got != want {
		t.Fatalf("Credentials = %v, %v", got, err)
	}
}
