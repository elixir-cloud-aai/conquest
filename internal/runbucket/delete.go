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

// DeleteRunData deletes the run's data. A run in the configured bucket
// (the host of its stored run root; runs without one, such as the stub runs
// of standalone TES tasks, are there too) loses the objects below its run
// root, which must be where the layout puts it; a run in a bucket of its
// own loses that bucket (perRun's Delete). It succeeds if the data is gone
// already.
func (d *DataDeleter) DeleteRunData(ctx context.Context, run *domain.Run) error {
	configured := d.shared.bucket.Name
	bucket := configured
	if run.RunRootURI != "" {
		var err error
		if bucket, err = BucketOf(run.RunRootURI); err != nil {
			return fmt.Errorf("run data of %s: %w", run.RunID, err)
		}
	}
	if bucket != configured {
		if d.perRun == nil {
			return fmt.Errorf("run data of %s: its bucket %q is not the configured one, and no provisioner gives runs buckets of their own", run.RunID, bucket)
		}
		return d.perRun.Delete(ctx, run.RunID)
	}
	version := run.LayoutVersion
	if version == "" {
		version = d.shared.layoutVersion
	}
	root, err := runRoot(version, run.RunID)
	if err != nil {
		return err
	}
	// delete only what the stored run root names
	if want := "s3://" + configured + "/" + root; run.RunRootURI != "" && run.RunRootURI != want {
		return fmt.Errorf("run data of %s: run root %q is not %q", run.RunID, run.RunRootURI, want)
	}
	return d.shared.deleteRoot(ctx, version, run.RunID)
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
