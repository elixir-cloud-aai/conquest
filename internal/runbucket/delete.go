package runbucket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anuragxxd/conquest/internal/domain"
)

// DataDeleter deletes runs' data wherever each run's data is: a run keeps
// its bucket when the provisioner changes, so a deployment can hold runs in
// the configured bucket and in buckets of their own.
type DataDeleter struct {
	shared *Shared
	perRun Provisioner
}

// NewDataDeleter returns a DataDeleter that deletes the objects of runs in
// the configured bucket with shared, and the buckets of other runs with
// perRun, the provisioner that gives runs buckets of their own (nil if
// there is none).
func NewDataDeleter(shared *Shared, perRun Provisioner) (*DataDeleter, error) {
	if shared == nil {
		return nil, errors.New("run data deleter: the shared bucket is required")
	}
	return &DataDeleter{shared: shared, perRun: perRun}, nil
}

// CheckRunData reports whether DeleteRunData can place the run's data. It
// deletes nothing and calls no object store, so callers can skip runs whose
// data no one may delete before doing other work for them.
func (d *DataDeleter) CheckRunData(run *domain.Run) error {
	_, _, err := d.place(run)
	return err
}

// DeleteRunData deletes the run's data. A run in the configured bucket
// (the host of its stored run root; runs without one, such as the stub runs
// of standalone TES tasks, are there too) loses the objects below its run
// root, which must be where the layout puts it; a run in a bucket of its
// own loses that bucket (perRun's Delete), if perRun may have made it. It
// succeeds if the data is gone already.
func (d *DataDeleter) DeleteRunData(ctx context.Context, run *domain.Run) error {
	owner, version, err := d.place(run)
	if err != nil {
		return err
	}
	if owner != nil {
		return owner.Delete(ctx, run.RunID)
	}
	return d.shared.deleteRoot(ctx, version, run.RunID)
}

// place finds the run's data: the provisioner that owns its bucket, or
// (owner nil) the layout version of its run root in the configured bucket.
func (d *DataDeleter) place(run *domain.Run) (BucketOwner, string, error) {
	configured := d.shared.bucket.Name
	bucket := configured
	if run.RunRootURI != "" {
		var err error
		if bucket, err = BucketOf(run.RunRootURI); err != nil {
			return nil, "", fmt.Errorf("run data of %s: %w", run.RunID, err)
		}
	}
	if bucket != configured {
		// Only a bucket the provisioner may have made: for any other (e.g.
		// the configured bucket before a rename), deleting a claim would
		// record the data as deleted while it stays.
		owner, ok := d.perRun.(BucketOwner)
		if !ok || !owner.OwnsBucket(bucket) {
			return nil, "", fmt.Errorf("run data of %s: its bucket %q is neither the configured one nor one the run bucket provisioner makes", run.RunID, bucket)
		}
		return owner, "", nil
	}
	version := run.LayoutVersion
	if version == "" {
		version = d.shared.layoutVersion
	}
	root, err := runRoot(version, run.RunID)
	if err != nil {
		return nil, "", err
	}
	// delete only what the stored run root names
	if want := "s3://" + configured + "/" + root; run.RunRootURI != "" && run.RunRootURI != want {
		return nil, "", fmt.Errorf("run data of %s: run root %q is not %q", run.RunID, run.RunRootURI, want)
	}
	return nil, version, nil
}

// BucketOwner is a Provisioner that tells which buckets it may have made;
// DataDeleter hands it only those.
type BucketOwner interface {
	Provisioner
	OwnsBucket(bucket string) bool
}

// OwnedBucket is a bucket that a provisioner made for a run.
type OwnedBucket struct {
	RunID   string
	Created time.Time
}

// BucketLister is a Provisioner that can list the buckets it made, so that
// those whose run was never stored can be found (package retention).
type BucketLister interface {
	Provisioner
	ListRunBuckets(ctx context.Context) ([]OwnedBucket, error)
}
