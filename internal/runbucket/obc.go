package runbucket

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// ModeOBC gives each run its own bucket through an ObjectBucketClaim.
const ModeOBC Mode = "obc"

// Settings of the obc provisioner.
const (
	// EnvOBCStorageClass is the StorageClass of the claims (required).
	EnvOBCStorageClass = "CONQUEST_RUN_BUCKETS_OBC_STORAGE_CLASS"
	// EnvOBCMaxSize is each run bucket's size quota (additionalConfig
	// maxSize), a Kubernetes quantity such as 10Gi. Empty: none.
	EnvOBCMaxSize = "CONQUEST_RUN_BUCKETS_OBC_MAX_SIZE"
	// EnvOBCMaxObjects is each run bucket's object quota (additionalConfig
	// maxObjects). Empty: none.
	EnvOBCMaxObjects = "CONQUEST_RUN_BUCKETS_OBC_MAX_OBJECTS"
	// EnvOBCBindTimeout is how long a run submission waits for its claim to
	// be Bound (a Go duration).
	EnvOBCBindTimeout = "CONQUEST_RUN_BUCKETS_OBC_BIND_TIMEOUT"
)

// DefaultOBCBindTimeout is the default of EnvOBCBindTimeout.
const DefaultOBCBindTimeout = 20 * time.Second

// MaxOBCBindTimeout bounds EnvOBCBindTimeout: the WES request that submits
// the run waits for the binding, ends within 25s and leaves Reserve (5s)
// of that to store the run, so a longer wait could never be used.
const MaxOBCBindTimeout = 20 * time.Second

// RunIDLabel labels each claim with its run's ID. Conquest adopts and
// deletes only claims that carry its run's ID.
const RunIDLabel = "conquest.io/run-id"

// InstanceLabel labels each claim with the Conquest instance that made it
// (OBCConfig.Instance), so that an instance lists only its own claims when
// it looks for claims without a run: other instances in the same namespace
// keep their runs in other databases.
const InstanceLabel = "conquest.io/instance"

// labelValuePattern admits Kubernetes label values (up to 63 characters).
var labelValuePattern = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)

// OBCNamePrefix starts the name of each run's claim, and so of the Secret
// and ConfigMap the bucket provisioner creates for it, and the bucket names
// the provisioner generates.
const OBCNamePrefix = "conquest-run-"

// obcResource is the ObjectBucketClaim API (lib-bucket-provisioner).
var obcResource = schema.GroupVersionResource{Group: "objectbucket.io", Version: "v1alpha1", Resource: "objectbucketclaims"}

// Phases of an ObjectBucketClaim (lib-bucket-provisioner).
const (
	obcPhaseBound  = "Bound"
	obcPhaseFailed = "Failed"
)

// Keys of the ConfigMap and Secret the bucket provisioner creates.
const (
	obcBucketNameKey = "BUCKET_NAME"
	obcAccessKeyKey  = "AWS_ACCESS_KEY_ID"
	obcSecretKeyKey  = "AWS_SECRET_ACCESS_KEY"
)

// runIDPattern admits run IDs that make valid claim names and label values.
var runIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// maxRunIDLen keeps OBCNamePrefix + run ID within the 63 characters of a
// label value (and of the bucket names some provisioners derive from it).
const maxRunIDLen = 63 - len(OBCNamePrefix)

// bucketNamePattern admits S3 bucket names (DNS-style).
var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// OBCConfig configures the obc provisioner.
type OBCConfig struct {
	// Namespace of the claims: Conquest's, where the runner pods run, since
	// they get the Secret of their claim.
	Namespace string
	// StorageClassName is the StorageClass of the claims. Its reclaim
	// policy decides whether deleting a claim deletes its bucket.
	StorageClassName string
	// MaxSize and MaxObjects are the per-bucket quotas
	// (spec.additionalConfig); empty: none.
	MaxSize    string
	MaxObjects string
	// BindTimeout bounds the wait for a claim to be Bound.
	BindTimeout time.Duration
	// PollInterval is how often Ensure checks the claim (default 500ms).
	PollInterval time.Duration
	// Reserve is the time Ensure leaves of its context's deadline for the
	// rest of the submission (storing the run, creating its runner Job):
	// it stops waiting that long before the deadline, and creates no claim
	// with less time left. Default DefaultOBCReserve.
	Reserve time.Duration
	// Instance names this Conquest in the InstanceLabel of its claims (the
	// server uses its configured bucket); empty: no label, and
	// ListRunBuckets fails.
	Instance string
	// ListPageSize is how many claims ListRunBuckets reads per request
	// (default 500).
	ListPageSize int64
}

// DefaultOBCReserve is the default of OBCConfig.Reserve.
const DefaultOBCReserve = 5 * time.Second

// ParseOBCConfig reads the obc provisioner's settings (all but Namespace)
// with getenv.
func ParseOBCConfig(getenv func(string) string) (OBCConfig, error) {
	c := OBCConfig{
		StorageClassName: strings.TrimSpace(getenv(EnvOBCStorageClass)),
		MaxSize:          strings.TrimSpace(getenv(EnvOBCMaxSize)),
		MaxObjects:       strings.TrimSpace(getenv(EnvOBCMaxObjects)),
		BindTimeout:      DefaultOBCBindTimeout,
	}
	if c.StorageClassName == "" {
		return OBCConfig{}, fmt.Errorf("%s=%s needs %s", EnvMode, ModeOBC, EnvOBCStorageClass)
	}
	if c.MaxSize != "" {
		q, err := resource.ParseQuantity(c.MaxSize)
		if err != nil || q.Sign() <= 0 {
			return OBCConfig{}, fmt.Errorf("%s must be a positive quantity such as 10Gi, got %q", EnvOBCMaxSize, c.MaxSize)
		}
	}
	if c.MaxObjects != "" {
		n, err := strconv.ParseInt(c.MaxObjects, 10, 64)
		if err != nil || n <= 0 {
			return OBCConfig{}, fmt.Errorf("%s must be a positive integer, got %q", EnvOBCMaxObjects, c.MaxObjects)
		}
	}
	if v := strings.TrimSpace(getenv(EnvOBCBindTimeout)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > MaxOBCBindTimeout {
			return OBCConfig{}, fmt.Errorf("%s must be a duration above 0 and at most %s, got %q", EnvOBCBindTimeout, MaxOBCBindTimeout, v)
		}
		c.BindTimeout = d
	}
	return c, nil
}

// OBC is the Provisioner that gives each run its own bucket through an
// ObjectBucketClaim (objectbucket.io/v1alpha1), as served by Rook/Ceph and
// other lib-bucket-provisioner drivers. The provisioner creates the bucket,
// an object store user that may reach only that bucket, and a Secret and a
// ConfigMap named after the claim, with the user's keys and the bucket's
// name. The runner pod gets that Secret, so the object store, not Conquest,
// keeps its workflow code to its own run's bucket.
//
// The bucket must be in the object store at CONQUEST_S3_ENDPOINT: the
// runner and the server reach every bucket there, and the ConfigMap's
// BUCKET_HOST is not used.
type OBC struct {
	dyn  dynamic.Interface
	kube kubernetes.Interface
	cfg  OBCConfig
}

var _ Provisioner = (*OBC)(nil)

// NewOBC returns the obc Provisioner, which manages claims with dyn and reads
// their Secrets and ConfigMaps with kube.
func NewOBC(dyn dynamic.Interface, kube kubernetes.Interface, cfg OBCConfig) (*OBC, error) {
	switch {
	case dyn == nil || kube == nil:
		return nil, errors.New("obc run buckets: Kubernetes clients are required")
	case cfg.Namespace == "":
		return nil, errors.New("obc run buckets: namespace is required")
	case cfg.StorageClassName == "":
		return nil, fmt.Errorf("obc run buckets: %s is required", EnvOBCStorageClass)
	case cfg.BindTimeout <= 0:
		return nil, errors.New("obc run buckets: bind timeout must be above 0")
	case !labelValuePattern.MatchString(cfg.Instance):
		return nil, fmt.Errorf("obc run buckets: instance %q is no label value", cfg.Instance)
	}
	if cfg.ListPageSize <= 0 {
		cfg.ListPageSize = 500
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.Reserve <= 0 {
		cfg.Reserve = DefaultOBCReserve
	}
	return &OBC{dyn: dyn, kube: kube, cfg: cfg}, nil
}

// claimName returns the name of the run's claim (and of its Secret and
// ConfigMap).
func claimName(runID string) (string, error) {
	if len(runID) > maxRunIDLen || !runIDPattern.MatchString(runID) {
		return "", fmt.Errorf("obc run buckets: run ID %q can't name a claim", runID)
	}
	return OBCNamePrefix + runID, nil
}

func (p *OBC) claims() dynamic.ResourceInterface {
	return p.dyn.Resource(obcResource).Namespace(p.cfg.Namespace)
}

// Ensure creates the run's claim, or adopts it if it exists and carries the
// run's ID, waits until it is Bound, and returns its bucket and Secret. It
// waits at most the bind timeout, and stops Reserve before ctx's deadline.
func (p *OBC) Ensure(ctx context.Context, runID string) (Bucket, error) {
	name, err := claimName(runID)
	if err != nil {
		return Bucket{}, err
	}
	if _, err := p.bindBudget(ctx, name); err != nil {
		return Bucket{}, err
	}
	if _, err := p.claims().Create(ctx, p.newClaim(name, runID), metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return Bucket{}, fmt.Errorf("obc run buckets: create claim %s: %w", name, err)
		}
		// A retry: adopt the claim only if it is this run's.
		existing, err := p.claims().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return Bucket{}, fmt.Errorf("obc run buckets: get claim %s: %w", name, err)
		}
		if err := ownClaim(existing, runID); err != nil {
			return Bucket{}, err
		}
	}
	// creating the claim took time too
	bindTimeout, err := p.bindBudget(ctx, name)
	if err != nil {
		return Bucket{}, err
	}
	if err := p.waitBound(ctx, name, runID, bindTimeout); err != nil {
		return Bucket{}, err
	}

	cm, err := p.kube.CoreV1().ConfigMaps(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Bucket{}, fmt.Errorf("obc run buckets: ConfigMap of claim %s: %w", name, err)
	}
	bucket := strings.TrimSpace(cm.Data[obcBucketNameKey])
	if !bucketNamePattern.MatchString(bucket) {
		return Bucket{}, fmt.Errorf("obc run buckets: ConfigMap of claim %s has no valid %s", name, obcBucketNameKey)
	}
	// The runner gets the Secret by secretKeyRef: check it is complete now,
	// not when the runner pod fails to start.
	if _, err := p.credentials(ctx, name); err != nil {
		return Bucket{}, err
	}
	return Bucket{Name: bucket, SecretName: name}, nil
}

// newClaim returns the claim for the run: the provisioner generates the
// bucket's name from OBCNamePrefix.
func (p *OBC) newClaim(name, runID string) *unstructured.Unstructured {
	spec := map[string]any{
		"generateBucketName": OBCNamePrefix,
		"storageClassName":   p.cfg.StorageClassName,
	}
	extra := map[string]any{}
	if p.cfg.MaxSize != "" {
		extra["maxSize"] = p.cfg.MaxSize
	}
	if p.cfg.MaxObjects != "" {
		extra["maxObjects"] = p.cfg.MaxObjects
	}
	if len(extra) > 0 {
		spec["additionalConfig"] = extra
	}
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetAPIVersion(obcResource.GroupVersion().String())
	obj.SetKind("ObjectBucketClaim")
	obj.SetName(name)
	obj.SetNamespace(p.cfg.Namespace)
	labels := map[string]string{
		RunIDLabel:                     runID,
		"app.kubernetes.io/managed-by": "conquest",
	}
	if p.cfg.Instance != "" {
		labels[InstanceLabel] = p.cfg.Instance
	}
	obj.SetLabels(labels)
	return obj
}

// ownClaim refuses a claim that doesn't carry the run's ID or is being
// deleted.
func ownClaim(obj *unstructured.Unstructured, runID string) error {
	if err := labelledFor(obj, runID); err != nil {
		return err
	}
	if obj.GetDeletionTimestamp() != nil {
		return fmt.Errorf("obc run buckets: claim %s is being deleted", obj.GetName())
	}
	return nil
}

// labelledFor refuses a claim that doesn't carry the run's ID: Conquest
// adopts and deletes only its own runs' claims.
func labelledFor(obj *unstructured.Unstructured, runID string) error {
	if obj.GetLabels()[RunIDLabel] != runID {
		return fmt.Errorf("obc run buckets: claim %s is not labelled %s=%s", obj.GetName(), RunIDLabel, runID)
	}
	return nil
}

// bindBudget returns how long Ensure may still wait for the claim: the bind
// timeout, but no later than Reserve before ctx's deadline.
func (p *OBC) bindBudget(ctx context.Context, name string) (time.Duration, error) {
	budget := p.cfg.BindTimeout
	if deadline, ok := ctx.Deadline(); ok {
		left := time.Until(deadline) - p.cfg.Reserve
		if left <= 0 {
			return 0, fmt.Errorf("obc run buckets: too little time left in the request for claim %s", name)
		}
		budget = min(budget, left)
	}
	return budget, nil
}

// waitBound waits until the claim is Bound, for at most timeout.
func (p *OBC) waitBound(ctx context.Context, name, runID string, timeout time.Duration) error {
	phase := ""
	err := wait.PollUntilContextTimeout(ctx, p.cfg.PollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		obj, err := p.claims().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("obc run buckets: get claim %s: %w", name, err)
		}
		if err := ownClaim(obj, runID); err != nil {
			return false, err
		}
		phase, _, _ = unstructured.NestedString(obj.Object, "status", "phase")
		switch phase {
		case obcPhaseBound:
			return true, nil
		case obcPhaseFailed:
			return false, fmt.Errorf("obc run buckets: claim %s is %s", name, obcPhaseFailed)
		}
		return false, nil
	})
	if err != nil && wait.Interrupted(err) {
		if phase == "" {
			phase = "no phase"
		}
		return fmt.Errorf("obc run buckets: claim %s not Bound within %s (%s): %w", name, timeout, phase, err)
	}
	return err
}

// Credentials returns the keys in the Secret of the run's claim.
func (p *OBC) Credentials(ctx context.Context, runID string) (Credentials, error) {
	name, err := claimName(runID)
	if err != nil {
		return Credentials{}, err
	}
	return p.credentials(ctx, name)
}

func (p *OBC) credentials(ctx context.Context, name string) (Credentials, error) {
	s, err := p.kube.CoreV1().Secrets(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Credentials{}, fmt.Errorf("obc run buckets: Secret of claim %s: %w", name, err)
	}
	c := Credentials{
		AccessKeyID:     strings.TrimSpace(string(s.Data[obcAccessKeyKey])),
		SecretAccessKey: strings.TrimSpace(string(s.Data[obcSecretKeyKey])),
	}
	if c.Empty() {
		return Credentials{}, fmt.Errorf("obc run buckets: Secret of claim %s lacks %s or %s", name, obcAccessKeyKey, obcSecretKeyKey)
	}
	return c, nil
}

// Delete deletes the run's claim if it carries the run's ID. Whether the
// bucket and its objects go with it is the StorageClass's reclaim policy
// (Delete: yes; Rook purges non-empty buckets).
func (p *OBC) Delete(ctx context.Context, runID string) error {
	name, err := claimName(runID)
	if err != nil {
		return err
	}
	obj, err := p.claims().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("obc run buckets: get claim %s: %w", name, err)
	}
	if err := labelledFor(obj, runID); err != nil {
		return err
	}
	// a claim already being deleted is deleted again (a no-op)
	uid := obj.GetUID()
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &propagation}
	if uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if err := p.claims().Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("obc run buckets: delete claim %s: %w", name, err)
	}
	return nil
}

var _ BucketLister = (*OBC)(nil)

// ListRunBuckets lists this instance's claims (InstanceLabel) that carry a
// run ID and aren't being deleted, with their run IDs and creation times.
// It fails without an instance: it would list other instances' claims.
func (p *OBC) ListRunBuckets(ctx context.Context) ([]OwnedBucket, error) {
	if p.cfg.Instance == "" {
		return nil, errors.New("obc run buckets: listing claims needs an instance")
	}
	opts := metav1.ListOptions{
		LabelSelector: RunIDLabel + "," + InstanceLabel + "=" + p.cfg.Instance,
		Limit:         p.cfg.ListPageSize,
	}
	var owned []OwnedBucket
	for {
		list, err := p.claims().List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("obc run buckets: list claims: %w", err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			runID := obj.GetLabels()[RunIDLabel]
			if obj.GetDeletionTimestamp() != nil || runID == "" {
				continue
			}
			owned = append(owned, OwnedBucket{RunID: runID, Created: obj.GetCreationTimestamp().Time})
		}
		if opts.Continue = list.GetContinue(); opts.Continue == "" {
			return owned, nil
		}
	}
}
