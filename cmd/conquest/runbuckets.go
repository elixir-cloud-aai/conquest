package main

import (
	"fmt"

	"github.com/anuragxxd/conquest/internal/config"
	"github.com/anuragxxd/conquest/internal/runbucket"
)

// newRunBuckets returns the run bucket provisioner that cfg.RunBuckets
// selects.
func newRunBuckets(cfg *config.Config) (runbucket.Provisioner, error) {
	switch cfg.RunBuckets {
	case runbucket.ModeShared:
		return runbucket.NewShared(cfg.S3Bucket, cfg.StorageSecretName)
	default:
		return nil, fmt.Errorf("%s: unknown mode %q", runbucket.EnvMode, cfg.RunBuckets)
	}
}
