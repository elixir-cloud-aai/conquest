// Package runbucket decides where each workflow run's objects live and which
// credentials its runner pod gets for them.
//
// The engines in the runner pod (Nextflow, Snakemake, miniwdl) talk to object
// storage with their own S3 clients, so the runner needs S3 credentials, and
// workflow code that runs next to the engine can read them. A Provisioner
// gives each run a bucket and the name of the Kubernetes Secret with the
// credentials for it. With per-run buckets, those credentials reach only the
// run's own bucket, and the object store, not Conquest, enforces the bound.
//
// Invariant for every Provisioner: a runner pod gets only its own run's
// Secret, by secretKeyRef, and no ServiceAccount token, so workflow code can't
// read another run's Secret through the Kubernetes API (internal/backend/k8s
// tests this).
//
// The Shared provisioner keeps today's behaviour: every run uses the one
// configured bucket, under its run root, with the bucket-wide credentials.
// The OBC provisioner gives each run its own bucket through an
// ObjectBucketClaim, with credentials for that bucket only.
//
// The server reaches a run's objects (workflow outputs, the storage
// gateway's presigned URLs and listings) in the run's bucket with the run's
// credentials (Access). A run's bucket is the host of its stored run root
// URI, so a run keeps its bucket when the provisioner changes.
//
// Task pods follow the run's bucket only through the storage gateway, and
// the objectfs workspace mounts the configured bucket: a provisioner that
// gives runs their own buckets needs storage isolation and another workspace
// mode (CheckDeployment).
package runbucket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/anuragxxd/conquest/internal/storage"
)

// EnvMode is the setting that selects the provisioner.
const EnvMode = "CONQUEST_RUN_BUCKETS"

// Mode names a provisioner.
type Mode string

// ModeShared puts every run into the one configured bucket (the default).
const ModeShared Mode = "shared"

// PerRun reports whether the mode can give a run a bucket other than the
// configured one.
func (m Mode) PerRun() bool {
	return m != ModeShared
}

// CheckDeployment refuses settings with which a run's own bucket would leak
// or be missed: without storage isolation, task pods get the bucket-wide S3
// credentials; the objectfs workspace mounts the configured bucket with the
// bucket-wide CSI Secret.
func CheckDeployment(mode Mode, storageIsolation bool, workspaceMode string) error {
	if !mode.PerRun() {
		return nil
	}
	if !storageIsolation {
		return fmt.Errorf("%s=%s needs CONQUEST_STORAGE_ISOLATION=true: without it, task pods get the bucket-wide S3 credentials", EnvMode, mode)
	}
	if workspaceMode == "objectfs" {
		return fmt.Errorf("%s=%s doesn't work with CONQUEST_WORKSPACE_MODE=objectfs: it mounts the configured bucket with the bucket-wide CSI Secret", EnvMode, mode)
	}
	return nil
}

// ParseMode parses the value of EnvMode. Empty means ModeShared.
func ParseMode(v string) (Mode, error) {
	switch Mode(strings.TrimSpace(v)) {
	case "", ModeShared:
		return ModeShared, nil
	case ModeOBC:
		return ModeOBC, nil
	default:
		return "", fmt.Errorf("%s must be %q or %q, got %q", EnvMode, ModeShared, ModeOBC, v)
	}
}

// Bucket is where a run's objects live and how its runner reaches them.
type Bucket struct {
	// Name is the S3 bucket. The run's objects lie below its run root in
	// it (internal/storage).
	Name string
	// SecretName is the Kubernetes Secret, in Conquest's namespace, with the
	// keys AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY that the run's runner
	// pod gets.
	SecretName string
}

// Credentials are S3 keys. They format and log without the secret key.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// Empty reports whether either key is missing.
func (c Credentials) Empty() bool {
	return c.AccessKeyID == "" || c.SecretAccessKey == ""
}

// String hides the secret key from fmt's %v and %s.
func (c Credentials) String() string {
	return fmt.Sprintf("{AccessKeyID:%s SecretAccessKey:REDACTED}", c.AccessKeyID)
}

// GoString hides the secret key from fmt's %#v.
func (c Credentials) GoString() string {
	return "runbucket.Credentials" + c.String()
}

// LogValue hides the secret key from log/slog.
func (c Credentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("access_key_id", c.AccessKeyID),
		slog.String("secret_access_key", "REDACTED"),
	)
}

// BucketOf returns the bucket of a stored run root URI,
// s3://<bucket>/<version>/workflow-runs/<run ID>/.
func BucketOf(runRootURI string) (string, error) {
	u, err := url.Parse(runRootURI)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return "", fmt.Errorf("run root %q is not an s3://<bucket>/ URI", runRootURI)
	}
	return u.Host, nil
}

// Provisioner gives runs their buckets.
type Provisioner interface {
	// Ensure returns the run's bucket, creating it first where the
	// provisioner creates buckets. Calling it again for the same run returns
	// the same bucket.
	Ensure(ctx context.Context, runID string) (Bucket, error)
	// Credentials returns the S3 keys for the run's bucket, as in the Secret
	// its runner gets, for the server's own reads and writes there.
	Credentials(ctx context.Context, runID string) (Credentials, error)
	// Delete removes the run's data: its objects, and what Ensure created
	// for it. It succeeds if there is nothing to remove.
	Delete(ctx context.Context, runID string) error
}

// Shared is the Provisioner for one bucket that all runs share.
type Shared struct {
	bucket Bucket
	creds  Credentials
	// objects deletes a run's objects (SetObjects); nil: Delete fails.
	objects       PrefixDeleter
	layoutVersion string
}

// PrefixDeleter deletes every object below a key prefix;
// objstore.S3Client satisfies it.
type PrefixDeleter interface {
	DeletePrefix(ctx context.Context, bucket, prefix string) error
}

var _ Provisioner = (*Shared)(nil)

// NewShared returns the Provisioner that gives every run the bucket name with
// the credentials in the Secret secretName.
func NewShared(name, secretName string) (*Shared, error) {
	if name == "" {
		return nil, errors.New("shared run bucket: bucket name is required")
	}
	if secretName == "" {
		return nil, errors.New("shared run bucket: Secret name is required")
	}
	return &Shared{bucket: Bucket{Name: name, SecretName: secretName}}, nil
}

// Ensure returns the shared bucket.
func (s *Shared) Ensure(_ context.Context, runID string) (Bucket, error) {
	if runID == "" {
		return Bucket{}, errors.New("shared run bucket: run ID is required")
	}
	return s.bucket, nil
}

// SetCredentials sets the keys Credentials returns: the server's own.
func (s *Shared) SetCredentials(c Credentials) {
	s.creds = c
}

// Credentials returns the server's keys for the shared bucket.
func (s *Shared) Credentials(context.Context, string) (Credentials, error) {
	if s.creds.Empty() {
		return Credentials{}, errors.New("shared run bucket: no S3 credentials configured")
	}
	return s.creds, nil
}

// SetObjects makes Delete delete a run's objects with objects, below the
// run's root in layout version layoutVersion (storage.Layout).
func (s *Shared) SetObjects(objects PrefixDeleter, layoutVersion string) {
	s.objects, s.layoutVersion = objects, layoutVersion
}

// Delete deletes the run's objects: everything below its run root in the
// shared bucket, which itself stays. It fails without SetObjects.
func (s *Shared) Delete(ctx context.Context, runID string) error {
	return s.deleteRoot(ctx, s.layoutVersion, runID)
}

// deleteRoot deletes everything below the run's root in layout version
// version.
func (s *Shared) deleteRoot(ctx context.Context, version, runID string) error {
	if s.objects == nil {
		return fmt.Errorf("shared run bucket: no object store to delete the objects of run %s", runID)
	}
	root, err := runRoot(version, runID)
	if err != nil {
		return err
	}
	if err := s.objects.DeletePrefix(ctx, s.bucket.Name, root); err != nil {
		return fmt.Errorf("shared run bucket: delete the objects of run %s: %w", runID, err)
	}
	return nil
}

// runRoot returns the key prefix of the run's root in layout version
// version. It refuses run IDs and versions that aren't one key segment of
// their own: their root could reach other runs' objects.
func runRoot(version, runID string) (string, error) {
	for _, v := range []string{version, runID} {
		if v == "" || v == "." || strings.Contains(v, "..") || strings.ContainsAny(v, `/\`) {
			return "", fmt.Errorf("run bucket: run %q in layout version %q has no run root of its own", runID, version)
		}
	}
	return storage.Layout{Version: version}.RunRoot(runID), nil
}
