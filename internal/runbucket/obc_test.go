package runbucket

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testNamespace = "conquest"
	testRunID     = "0193b6a2-7c1e-7f00-8000-000000000001"
	testOBCName   = "conquest-run-" + testRunID
)

// obcFixture is a fake API server with a stub bucket provisioner: once
// bind is called, it marks an OBC Bound and creates its ConfigMap and
// Secret, as Rook and other lib-bucket-provisioner drivers do.
type obcFixture struct {
	t    *testing.T
	dyn  *dynamicfake.FakeDynamicClient
	kube *kubefake.Clientset
	p    *OBC
}

func newOBCFixture(t *testing.T, cfg OBCConfig) *obcFixture {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{obcResource: "ObjectBucketClaimList"})
	kube := kubefake.NewSimpleClientset()
	if cfg.Namespace == "" {
		cfg.Namespace = testNamespace
	}
	if cfg.StorageClassName == "" {
		cfg.StorageClassName = "object-store-bucket"
	}
	if cfg.BindTimeout == 0 {
		cfg.BindTimeout = 2 * time.Second
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Millisecond
	}
	p, err := NewOBC(dyn, kube, cfg)
	if err != nil {
		t.Fatalf("NewOBC: %v", err)
	}
	return &obcFixture{t: t, dyn: dyn, kube: kube, p: p}
}

// bindOnCreate binds every OBC right after it is created, with the bucket
// name bucketPrefix + run ID.
func (f *obcFixture) bindOnCreate() {
	f.dyn.PrependReactor("create", "objectbucketclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		go f.bind(obj.GetName(), "bucket-"+obj.GetLabels()[RunIDLabel])
		return false, nil, nil
	})
}

// bind marks the OBC name Bound and creates its ConfigMap and Secret.
func (f *obcFixture) bind(name, bucket string) {
	ctx := context.Background()
	f.createAccess(name, map[string]string{"BUCKET_NAME": bucket, "BUCKET_HOST": "rgw.example.svc"},
		map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("key-" + bucket), "AWS_SECRET_ACCESS_KEY": []byte("secret-" + bucket)})
	for i := 0; i < 100; i++ {
		obj, err := f.dyn.Resource(obcResource).Namespace(testNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			time.Sleep(time.Millisecond)
			continue
		}
		_ = unstructured.SetNestedField(obj.Object, "Bound", "status", "phase")
		if _, err := f.dyn.Resource(obcResource).Namespace(testNamespace).Update(ctx, obj, metav1.UpdateOptions{}); err == nil {
			return
		}
	}
}

func (f *obcFixture) createAccess(name string, data map[string]string, secret map[string][]byte) {
	ctx := context.Background()
	_, _ = f.kube.CoreV1().ConfigMaps(testNamespace).Create(ctx,
		&v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}, Data: data}, metav1.CreateOptions{})
	_, _ = f.kube.CoreV1().Secrets(testNamespace).Create(ctx,
		&v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}, Data: secret}, metav1.CreateOptions{})
}

// putOBC stores an OBC as if created earlier, with the given labels and
// phase.
func (f *obcFixture) putOBC(name string, labels map[string]string, phase string) {
	f.t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetAPIVersion("objectbucket.io/v1alpha1")
	obj.SetKind("ObjectBucketClaim")
	obj.SetName(name)
	obj.SetNamespace(testNamespace)
	obj.SetLabels(labels)
	if phase != "" {
		_ = unstructured.SetNestedField(obj.Object, phase, "status", "phase")
	}
	if _, err := f.dyn.Resource(obcResource).Namespace(testNamespace).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		f.t.Fatalf("create OBC: %v", err)
	}
}

func (f *obcFixture) getOBC(name string) (*unstructured.Unstructured, error) {
	return f.dyn.Resource(obcResource).Namespace(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
}

func TestOBC_EnsureCreatesAClaimAndWaitsForItsBucket(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{StorageClassName: "rook-bucket", MaxSize: "10Gi", MaxObjects: "100000"})
	f.bindOnCreate()

	got, err := f.p.Ensure(context.Background(), testRunID)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if want := (Bucket{Name: "bucket-" + testRunID, SecretName: testOBCName}); got != want {
		t.Fatalf("Ensure = %+v, want %+v", got, want)
	}

	obj, err := f.getOBC(testOBCName)
	if err != nil {
		t.Fatalf("OBC not created: %v", err)
	}
	if obj.GetAPIVersion() != "objectbucket.io/v1alpha1" || obj.GetKind() != "ObjectBucketClaim" {
		t.Errorf("OBC type = %s %s", obj.GetAPIVersion(), obj.GetKind())
	}
	if obj.GetLabels()[RunIDLabel] != testRunID {
		t.Errorf("labels = %v, want %s=%s", obj.GetLabels(), RunIDLabel, testRunID)
	}
	for path, want := range map[string]string{
		"spec.generateBucketName":          "conquest-run-",
		"spec.storageClassName":            "rook-bucket",
		"spec.additionalConfig.maxSize":    "10Gi",
		"spec.additionalConfig.maxObjects": "100000",
	} {
		v, _, _ := unstructured.NestedString(obj.Object, strings.Split(path, ".")...)
		if v != want {
			t.Errorf("%s = %q, want %q", path, v, want)
		}
	}
	if _, found, _ := unstructured.NestedString(obj.Object, "spec", "bucketName"); found {
		t.Error("spec.bucketName is set: the provisioner must generate the name")
	}
}

func TestOBC_EnsureSetsNoQuotaUnlessConfigured(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	f.bindOnCreate()
	if _, err := f.p.Ensure(context.Background(), testRunID); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	obj, _ := f.getOBC(testOBCName)
	if _, found, _ := unstructured.NestedMap(obj.Object, "spec", "additionalConfig"); found {
		t.Errorf("spec.additionalConfig set without quota settings: %v", obj.Object["spec"])
	}
}

func TestOBC_EnsureAdoptsTheRunsOwnClaim(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	f.putOBC(testOBCName, map[string]string{RunIDLabel: testRunID}, "Bound")
	f.createAccess(testOBCName, map[string]string{"BUCKET_NAME": "existing"},
		map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("k"), "AWS_SECRET_ACCESS_KEY": []byte("s")})

	for i := 0; i < 2; i++ {
		got, err := f.p.Ensure(context.Background(), testRunID)
		if err != nil {
			t.Fatalf("Ensure #%d: %v", i+1, err)
		}
		if got.Name != "existing" || got.SecretName != testOBCName {
			t.Fatalf("Ensure #%d = %+v", i+1, got)
		}
	}
}

func TestOBC_EnsureRefusesAClaimThatIsNotTheRuns(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"unlabelled":    nil,
		"another run's": {RunIDLabel: "0193b6a2-7c1e-7f00-8000-000000000002"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOBCFixture(t, OBCConfig{})
			f.putOBC(testOBCName, labels, "Bound")
			f.createAccess(testOBCName, map[string]string{"BUCKET_NAME": "other"},
				map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("k"), "AWS_SECRET_ACCESS_KEY": []byte("s")})
			if got, err := f.p.Ensure(context.Background(), testRunID); err == nil {
				t.Fatalf("Ensure adopted a claim that isn't the run's: %+v", got)
			}
		})
	}
}

func TestOBC_EnsureRefusesAClaimBeingDeleted(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	f.putOBC(testOBCName, map[string]string{RunIDLabel: testRunID}, "Bound")
	f.createAccess(testOBCName, map[string]string{"BUCKET_NAME": "going"},
		map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("k"), "AWS_SECRET_ACCESS_KEY": []byte("s")})
	obj, _ := f.getOBC(testOBCName)
	now := metav1.Now()
	obj.SetDeletionTimestamp(&now)
	if _, err := f.dyn.Resource(obcResource).Namespace(testNamespace).Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.p.Ensure(context.Background(), testRunID); err == nil {
		t.Fatalf("Ensure adopted a claim being deleted: %+v", got)
	}
}

func TestOBC_EnsureTimesOutWithoutBinding(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{BindTimeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := f.p.Ensure(context.Background(), testRunID)
	if err == nil || !strings.Contains(err.Error(), "Bound") {
		t.Fatalf("Ensure = %v, want a timeout waiting for Bound", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Ensure took %v with a 50ms bind timeout", d)
	}
}

func TestOBC_EnsureStopsWhenTheClaimFails(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{BindTimeout: time.Minute})
	f.putOBC(testOBCName, map[string]string{RunIDLabel: testRunID}, "Failed")
	start := time.Now()
	if _, err := f.p.Ensure(context.Background(), testRunID); err == nil || !strings.Contains(err.Error(), "Failed") {
		t.Fatalf("Ensure = %v, want the Failed phase", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Ensure waited %v for a failed claim", d)
	}
}

func TestOBC_EnsureFollowsTheContext(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{BindTimeout: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.p.Ensure(ctx, testRunID); err == nil {
		t.Fatal("Ensure returned without binding or error")
	}
}

func TestOBC_EnsureRefusesIncompleteBuckets(t *testing.T) {
	for name, tc := range map[string]struct {
		data   map[string]string
		secret map[string][]byte
	}{
		"no bucket name":  {map[string]string{"BUCKET_HOST": "h"}, map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("k"), "AWS_SECRET_ACCESS_KEY": []byte("s")}},
		"no access key":   {map[string]string{"BUCKET_NAME": "b"}, map[string][]byte{"AWS_SECRET_ACCESS_KEY": []byte("s")}},
		"no secret key":   {map[string]string{"BUCKET_NAME": "b"}, map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("k")}},
		"no ConfigMap":    {nil, map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("k"), "AWS_SECRET_ACCESS_KEY": []byte("s")}},
		"no Secret":       {map[string]string{"BUCKET_NAME": "b"}, nil},
		"bad bucket name": {map[string]string{"BUCKET_NAME": "b/../c"}, map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("k"), "AWS_SECRET_ACCESS_KEY": []byte("s")}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOBCFixture(t, OBCConfig{})
			f.putOBC(testOBCName, map[string]string{RunIDLabel: testRunID}, "Bound")
			ctx := context.Background()
			if tc.data != nil {
				_, _ = f.kube.CoreV1().ConfigMaps(testNamespace).Create(ctx,
					&v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: testOBCName}, Data: tc.data}, metav1.CreateOptions{})
			}
			if tc.secret != nil {
				_, _ = f.kube.CoreV1().Secrets(testNamespace).Create(ctx,
					&v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testOBCName}, Data: tc.secret}, metav1.CreateOptions{})
			}
			if got, err := f.p.Ensure(ctx, testRunID); err == nil {
				t.Fatalf("Ensure = %+v, want an error", got)
			}
		})
	}
}

func TestOBC_RefusesBadRunIDs(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	for _, id := range []string{"", "Run-1", "run_1", "-run", "run-", "a/b", strings.Repeat("a", 51)} {
		if _, err := f.p.Ensure(context.Background(), id); err == nil {
			t.Errorf("Ensure(%q) accepted", id)
		}
		if _, err := f.p.Credentials(context.Background(), id); err == nil {
			t.Errorf("Credentials(%q) accepted", id)
		}
		if err := f.p.Delete(context.Background(), id); err == nil {
			t.Errorf("Delete(%q) accepted", id)
		}
	}
}

func TestOBC_CredentialsAreThoseOfTheRunsSecret(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	f.bindOnCreate()
	if _, err := f.p.Ensure(context.Background(), testRunID); err != nil {
		t.Fatal(err)
	}
	got, err := f.p.Credentials(context.Background(), testRunID)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	b := "bucket-" + testRunID
	if want := (Credentials{AccessKeyID: "key-" + b, SecretAccessKey: "secret-" + b}); got != want {
		t.Fatalf("Credentials = %v, want %v", got, want)
	}
}

func TestOBC_CredentialsFailWithoutTheRunsSecret(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	if _, err := f.p.Credentials(context.Background(), testRunID); err == nil {
		t.Fatal("Credentials without a Secret must fail")
	}
	f.createAccess(testOBCName, nil, map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("k")})
	if _, err := f.p.Credentials(context.Background(), testRunID); err == nil {
		t.Fatal("Credentials without a secret key must fail")
	}
}

func TestOBC_DeleteRemovesTheRunsClaim(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	f.bindOnCreate()
	if _, err := f.p.Ensure(context.Background(), testRunID); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Delete(context.Background(), testRunID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.getOBC(testOBCName); !apierrors.IsNotFound(err) {
		t.Fatalf("OBC after Delete: %v, want NotFound", err)
	}
	if err := f.p.Delete(context.Background(), testRunID); err != nil {
		t.Fatalf("Delete without a claim: %v, want nil", err)
	}
}

func TestOBC_DeleteKeepsClaimsThatAreNotTheRuns(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	f.putOBC(testOBCName, map[string]string{"app": "someone-else"}, "Bound")
	if err := f.p.Delete(context.Background(), testRunID); err == nil {
		t.Fatal("Delete removed a claim that isn't the run's")
	}
	if _, err := f.getOBC(testOBCName); err != nil {
		t.Fatalf("claim gone: %v", err)
	}
}

func TestOBC_DeleteReportsAPIErrors(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	f.putOBC(testOBCName, map[string]string{RunIDLabel: testRunID}, "Bound")
	f.dyn.PrependReactor("delete", "objectbucketclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if err := f.p.Delete(context.Background(), testRunID); err == nil {
		t.Fatal("Delete hid an API error")
	}
}

func TestOBC_EnsureWorksConcurrentlyForManyRuns(t *testing.T) {
	f := newOBCFixture(t, OBCConfig{})
	f.bindOnCreate()
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			b, err := f.p.Ensure(context.Background(), id)
			if err == nil && b.Name != "bucket-"+id {
				err = errors.New("wrong bucket " + b.Name)
			}
			errs <- err
		}("0193b6a2-7c1e-7f00-8000-00000000001" + string(rune('0'+i)))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestNewOBC_ValidatesItsConfig(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	var kube kubernetes.Interface = kubefake.NewSimpleClientset()
	good := OBCConfig{Namespace: "ns", StorageClassName: "sc", BindTimeout: time.Second}
	if _, err := NewOBC(dyn, kube, good); err != nil {
		t.Fatalf("NewOBC(good): %v", err)
	}
	for name, mut := range map[string]func(*OBCConfig){
		"no namespace":     func(c *OBCConfig) { c.Namespace = "" },
		"no storage class": func(c *OBCConfig) { c.StorageClassName = "" },
		"no bind timeout":  func(c *OBCConfig) { c.BindTimeout = 0 },
	} {
		c := good
		mut(&c)
		if _, err := NewOBC(dyn, kube, c); err == nil {
			t.Errorf("%s: NewOBC accepted %+v", name, c)
		}
	}
	if _, err := NewOBC(nil, kube, good); err == nil {
		t.Error("NewOBC accepted a nil dynamic client")
	}
	if _, err := NewOBC(dyn, nil, good); err == nil {
		t.Error("NewOBC accepted a nil clientset")
	}
}

func TestParseOBCConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	got, err := ParseOBCConfig(env(map[string]string{EnvOBCStorageClass: " rook-bucket "}))
	if err != nil {
		t.Fatalf("ParseOBCConfig: %v", err)
	}
	if got.StorageClassName != "rook-bucket" || got.BindTimeout != DefaultOBCBindTimeout || got.MaxSize != "" || got.MaxObjects != "" {
		t.Fatalf("defaults = %+v", got)
	}
	got, err = ParseOBCConfig(env(map[string]string{
		EnvOBCStorageClass: "sc", EnvOBCMaxSize: "10Gi", EnvOBCMaxObjects: "5000", EnvOBCBindTimeout: "5s",
	}))
	if err != nil {
		t.Fatalf("ParseOBCConfig: %v", err)
	}
	if got.MaxSize != "10Gi" || got.MaxObjects != "5000" || got.BindTimeout != 5*time.Second {
		t.Fatalf("parsed = %+v", got)
	}
	for name, m := range map[string]map[string]string{
		"no storage class":     {},
		"bad max size":         {EnvOBCStorageClass: "sc", EnvOBCMaxSize: "ten gigs"},
		"negative max size":    {EnvOBCStorageClass: "sc", EnvOBCMaxSize: "-1Gi"},
		"bad max objects":      {EnvOBCStorageClass: "sc", EnvOBCMaxObjects: "many"},
		"zero max objects":     {EnvOBCStorageClass: "sc", EnvOBCMaxObjects: "0"},
		"bad bind timeout":     {EnvOBCStorageClass: "sc", EnvOBCBindTimeout: "soon"},
		"zero bind timeout":    {EnvOBCStorageClass: "sc", EnvOBCBindTimeout: "0s"},
		"too long bind timout": {EnvOBCStorageClass: "sc", EnvOBCBindTimeout: "26s"},
	} {
		if got, err := ParseOBCConfig(env(m)); err == nil {
			t.Errorf("%s: ParseOBCConfig = %+v, want an error", name, got)
		}
	}
}
