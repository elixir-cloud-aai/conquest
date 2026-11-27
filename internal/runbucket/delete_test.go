package runbucket

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/anuragxxd/conquest/internal/domain"
)

// prefixes records DeletePrefix calls.
type prefixes struct {
	deleted []string // bucket + "/" + prefix
	err     error
}

func (p *prefixes) DeletePrefix(_ context.Context, bucket, prefix string) error {
	p.deleted = append(p.deleted, bucket+"/"+prefix)
	return p.err
}

// runBuckets records Delete calls of a per-run provisioner.
type runBuckets struct {
	Provisioner
	deleted []string
	err     error
}

func (p *runBuckets) Delete(_ context.Context, runID string) error {
	p.deleted = append(p.deleted, runID)
	return p.err
}

// OwnsBucket claims the buckets that obc's would be named like.
func (p *runBuckets) OwnsBucket(bucket string) bool {
	return strings.HasPrefix(bucket, OBCNamePrefix)
}

func newTestShared(t *testing.T) (*Shared, *prefixes) {
	t.Helper()
	s, err := NewShared("conquest-workflows", "storage-secrets")
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}
	objects := &prefixes{}
	s.SetObjects(objects, "v1")
	return s, objects
}

func TestShared_DeleteDeletesTheObjectsUnderTheRunRoot(t *testing.T) {
	s, objects := newTestShared(t)
	if err := s.Delete(context.Background(), testRunID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	want := "conquest-workflows/v1/workflow-runs/" + testRunID + "/"
	if len(objects.deleted) != 1 || objects.deleted[0] != want {
		t.Fatalf("deleted %v, want [%s]", objects.deleted, want)
	}
	// the bucket stays
	got, err := s.Ensure(context.Background(), "run-b")
	if err != nil || got.Name != "conquest-workflows" {
		t.Fatalf("Ensure after Delete = %+v, %v", got, err)
	}
}

func TestShared_DeleteRefusesRunIDsThatAreNoSingleKeySegment(t *testing.T) {
	s, objects := newTestShared(t)
	for _, id := range []string{"", ".", "..", "a/b", "a\\b", "../x", "a..b"} {
		if err := s.Delete(context.Background(), id); err == nil {
			t.Errorf("Delete(%q) succeeded", id)
		}
	}
	if len(objects.deleted) != 0 {
		t.Fatalf("deleted %v", objects.deleted)
	}
}

func TestShared_DeleteFailsWithoutAnObjectStore(t *testing.T) {
	s, err := NewShared("conquest-workflows", "storage-secrets")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), testRunID); err == nil {
		t.Fatal("Delete without an object store succeeded: the run's objects would stay")
	}
}

func TestShared_DeleteReportsObjectStoreErrors(t *testing.T) {
	s, objects := newTestShared(t)
	objects.err = errors.New("boom")
	if err := s.Delete(context.Background(), testRunID); err == nil {
		t.Fatal("Delete hid the object store's error")
	}
}

func TestDataDeleter_DeletesEachRunWhereItsDataIs(t *testing.T) {
	s, objects := newTestShared(t)
	perRun := &runBuckets{}
	d, err := NewDataDeleter(s, perRun)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, run := range []*domain.Run{
		// in the configured bucket: its run root there, in its layout version
		{RunID: "run-shared", LayoutVersion: "v0", RunRootURI: "s3://conquest-workflows/v0/workflow-runs/run-shared/"},
		// no run root (stub runs of standalone TES tasks): the configured bucket
		{RunID: "run-stub"},
		// a bucket of its own: the provisioner's
		{RunID: "run-own", LayoutVersion: "v1", RunRootURI: "s3://conquest-run-abc/v1/workflow-runs/run-own/"},
	} {
		if err := d.CheckRunData(run); err != nil {
			t.Fatalf("CheckRunData(%s): %v", run.RunID, err)
		}
		if err := d.DeleteRunData(ctx, run); err != nil {
			t.Fatalf("DeleteRunData(%s): %v", run.RunID, err)
		}
	}
	wantPrefixes := []string{
		"conquest-workflows/v0/workflow-runs/run-shared/",
		"conquest-workflows/v1/workflow-runs/run-stub/",
	}
	if len(objects.deleted) != 2 || objects.deleted[0] != wantPrefixes[0] || objects.deleted[1] != wantPrefixes[1] {
		t.Fatalf("deleted prefixes %v, want %v", objects.deleted, wantPrefixes)
	}
	if len(perRun.deleted) != 1 || perRun.deleted[0] != "run-own" {
		t.Fatalf("provisioner deleted %v, want [run-own]", perRun.deleted)
	}
}

func TestDataDeleter_RefusesWhatItCantPlace(t *testing.T) {
	s, objects := newTestShared(t)
	ctx := context.Background()
	withoutPerRun, err := NewDataDeleter(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	perRun := &runBuckets{}
	t.Cleanup(func() {
		if len(perRun.deleted) != 0 {
			t.Errorf("provisioner deleted %v", perRun.deleted)
		}
	})
	withPerRun, err := NewDataDeleter(s, perRun)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		d   *DataDeleter
		run *domain.Run
	}{
		"unreadable run root": {withPerRun, &domain.Run{RunID: "r", RunRootURI: "http://x/"}},
		// the run root isn't where the layout puts it: delete nothing
		"unexpected run root": {withPerRun, &domain.Run{RunID: "r", LayoutVersion: "v1", RunRootURI: "s3://conquest-workflows/"}},
		"other run's root":    {withPerRun, &domain.Run{RunID: "r", LayoutVersion: "v1", RunRootURI: "s3://conquest-workflows/v1/workflow-runs/s/"}},
		// shared mode: another bucket is no one's to delete
		"other bucket, no per-run provisioner": {withoutPerRun, &domain.Run{RunID: "r", RunRootURI: "s3://conquest-run-abc/v1/workflow-runs/r/"}},
		// e.g. the configured bucket before a rename: no claim of the
		// provisioner holds it, so deleting one would delete nothing
		"bucket the provisioner didn't make": {withPerRun, &domain.Run{RunID: "r", RunRootURI: "s3://conquest-old/v1/workflow-runs/r/"}},
	} {
		if err := tc.d.CheckRunData(tc.run); err == nil {
			t.Errorf("%s: CheckRunData succeeded", name)
		}
		if err := tc.d.DeleteRunData(ctx, tc.run); err == nil {
			t.Errorf("%s: DeleteRunData succeeded", name)
		}
	}
	if len(objects.deleted) != 0 {
		t.Fatalf("deleted %v", objects.deleted)
	}
}

func TestNewDataDeleter_NeedsTheSharedBucket(t *testing.T) {
	if _, err := NewDataDeleter(nil, &runBuckets{}); err == nil {
		t.Fatal("NewDataDeleter without the shared bucket succeeded")
	}
}

func TestOBC_ClaimsCarryTheInstanceLabel(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{Instance: "conquest-workflows"})
	f.bindOnCreate()
	if _, err := f.p.Ensure(context.Background(), testRunID); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	obj, err := f.getOBC(testOBCName)
	if err != nil {
		t.Fatal(err)
	}
	if got := obj.GetLabels()[InstanceLabel]; got != "conquest-workflows" {
		t.Fatalf("%s = %q, want conquest-workflows", InstanceLabel, got)
	}
}

func TestOBC_ListRunBucketsListsThisInstancesClaims(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{Instance: "conquest-workflows", ListPageSize: 1})
	other := "0193b6a2-7c1e-7f00-8000-000000000002"
	third := "0193b6a2-7c1e-7f00-8000-000000000003"
	f.putOBC(testOBCName, map[string]string{RunIDLabel: testRunID, InstanceLabel: "conquest-workflows"}, "Bound")
	f.putOBC(OBCNamePrefix+other, map[string]string{RunIDLabel: other, InstanceLabel: "conquest-workflows"}, "Bound")
	// another Conquest's claim in the same namespace, and one from before
	// the instance label
	f.putOBC(OBCNamePrefix+third, map[string]string{RunIDLabel: third, InstanceLabel: "another-conquest"}, "Bound")
	f.putOBC(OBCNamePrefix+"0193b6a2-7c1e-7f00-8000-000000000004", map[string]string{RunIDLabel: "0193b6a2-7c1e-7f00-8000-000000000004"}, "Bound")

	got, err := f.p.ListRunBuckets(context.Background())
	if err != nil {
		t.Fatalf("ListRunBuckets: %v", err)
	}
	ids := map[string]bool{}
	for _, b := range got {
		ids[b.RunID] = true
	}
	if len(got) != 2 || !ids[testRunID] || !ids[other] {
		t.Fatalf("ListRunBuckets = %+v, want the claims of runs %s and %s", got, testRunID, other)
	}
}

func TestOBC_ListRunBucketsSkipsClaimsBeingDeletedAndReportsCreation(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{Instance: "conquest-workflows"})
	f.putOBC(testOBCName, map[string]string{RunIDLabel: testRunID, InstanceLabel: "conquest-workflows"}, "Bound")
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	obj, _ := f.getOBC(testOBCName)
	obj.SetCreationTimestamp(metav1.NewTime(created))
	if _, err := f.dyn.Resource(obcResource).Namespace(testNamespace).Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	gone := "0193b6a2-7c1e-7f00-8000-000000000002"
	f.putOBC(OBCNamePrefix+gone, map[string]string{RunIDLabel: gone, InstanceLabel: "conquest-workflows"}, "Bound")
	obj, _ = f.getOBC(OBCNamePrefix + gone)
	now := metav1.Now()
	obj.SetDeletionTimestamp(&now)
	if _, err := f.dyn.Resource(obcResource).Namespace(testNamespace).Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	got, err := f.p.ListRunBuckets(context.Background())
	if err != nil {
		t.Fatalf("ListRunBuckets: %v", err)
	}
	if len(got) != 1 || got[0].RunID != testRunID || !got[0].Created.Equal(created) {
		t.Fatalf("ListRunBuckets = %+v, want run %s created %s", got, testRunID, created)
	}
}

func TestOBC_ListRunBucketsNeedsAnInstance(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	if _, err := f.p.ListRunBuckets(context.Background()); err == nil {
		t.Fatal("ListRunBuckets without an instance succeeded: it would list other Conquests' claims")
	}
}

func TestOBC_ListRunBucketsReportsAPIErrors(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{Instance: "conquest-workflows"})
	f.dyn.PrependReactor("list", "objectbucketclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, err := f.p.ListRunBuckets(context.Background()); err == nil {
		t.Fatal("ListRunBuckets hid the API error")
	}
}

func TestNewOBC_RefusesAnInstanceThatIsNoLabelValue(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	if _, err := NewOBC(f.dyn, f.kube, OBCConfig{Namespace: testNamespace, StorageClassName: "x", BindTimeout: time.Second, Instance: "not a label"}); err == nil {
		t.Fatal("NewOBC accepted an instance that is no label value")
	}
}
