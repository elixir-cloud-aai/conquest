package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/anuragxxd/conquest/internal/config"
	"github.com/anuragxxd/conquest/internal/objstore"
	"github.com/anuragxxd/conquest/internal/repository"
	"github.com/anuragxxd/conquest/internal/retention"
	"github.com/anuragxxd/conquest/internal/runbucket"
)

// newRunRetention returns the sweeper that deletes the data of runs older
// than cfg.RunRetention, or nil if retention is off. prov is the run bucket
// provisioner; creds are the server's own keys for the configured bucket.
// The caller adds the run backend's Jobs and workspace clean-up.
func newRunRetention(cfg *config.Config, creds runbucket.Credentials, prov runbucket.Provisioner,
	store repository.RetentionStore, leases repository.Leases, log *slog.Logger) (*retention.Sweeper, error) {
	if cfg.RunRetention <= 0 {
		return nil, nil
	}
	data, orphans, err := runDataDeleter(cfg, creds, prov)
	if err != nil {
		return nil, err
	}
	s, err := retention.New(store, leases, data, retention.Config{Retention: cfg.RunRetention}, log)
	if err != nil {
		return nil, err
	}
	if orphans != nil {
		s.SetOrphans(orphans)
	}
	return s, nil
}

// runDataDeleter returns the deleter of runs' data: the objects of runs in
// the configured bucket (also those from before a switch to per-run
// buckets) with the server's keys, and the buckets of runs with their own
// with prov, which also lists its buckets for the orphan sweep (nil if it
// can't).
func runDataDeleter(cfg *config.Config, creds runbucket.Credentials, prov runbucket.Provisioner) (*runbucket.DataDeleter, runbucket.BucketLister, error) {
	if creds.Empty() {
		return nil, nil, errors.New("CONQUEST_RUN_RETENTION needs AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY to delete runs' objects")
	}
	objects, err := objstore.NewS3Client(s3Config(cfg.S3Endpoint, cfg.S3Region, creds))
	if err != nil {
		return nil, nil, fmt.Errorf("run retention: S3 client: %w", err)
	}
	shared, err := runbucket.NewShared(cfg.S3Bucket, cfg.StorageSecretName)
	if err != nil {
		return nil, nil, err
	}
	shared.SetObjects(objects, cfg.LayoutVersion)
	var perRun runbucket.Provisioner
	var orphans runbucket.BucketLister
	if cfg.RunBuckets.PerRun() {
		perRun = prov
		orphans, _ = prov.(runbucket.BucketLister)
	}
	data, err := runbucket.NewDataDeleter(shared, perRun)
	if err != nil {
		return nil, nil, err
	}
	return data, orphans, nil
}
