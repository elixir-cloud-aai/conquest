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
//
// Known limit: so far only the runner and the run's root follow the run's
// bucket. The server's own reads and writes (outputs, logs, the storage
// gateway) and task pods still use the configured bucket, so a provisioner
// that returns another bucket needs them to follow first.
package runbucket

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// EnvMode is the setting that selects the provisioner.
const EnvMode = "CONQUEST_RUN_BUCKETS"

// Mode names a provisioner.
type Mode string

// ModeShared puts every run into the one configured bucket (the default).
const ModeShared Mode = "shared"

// ParseMode parses the value of EnvMode. Empty means ModeShared.
func ParseMode(v string) (Mode, error) {
	switch Mode(strings.TrimSpace(v)) {
	case "", ModeShared:
		return ModeShared, nil
	default:
		return "", fmt.Errorf("%s must be %q, got %q", EnvMode, ModeShared, v)
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

// Provisioner gives runs their buckets.
type Provisioner interface {
	// Ensure returns the run's bucket, creating it first where the
	// provisioner creates buckets. Calling it again for the same run returns
	// the same bucket.
	Ensure(ctx context.Context, runID string) (Bucket, error)
	// Delete removes what Ensure created for the run, data included. It
	// succeeds if there is nothing to remove.
	Delete(ctx context.Context, runID string) error
}

// Shared is the Provisioner for one bucket that all runs share.
type Shared struct {
	bucket Bucket
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

// Delete does nothing: the shared bucket belongs to every run, and Ensure
// created nothing for this one.
func (s *Shared) Delete(context.Context, string) error {
	return nil
}
