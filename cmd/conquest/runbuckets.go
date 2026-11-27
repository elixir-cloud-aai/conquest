package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anuragxxd/conquest/internal/backend/remote"
	"github.com/anuragxxd/conquest/internal/backend/routing"
	"github.com/anuragxxd/conquest/internal/config"
	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/objstore"
	"github.com/anuragxxd/conquest/internal/runbucket"
	"github.com/anuragxxd/conquest/internal/storagegw"
)

// serverCredentials returns the server's own S3 keys for the configured
// bucket.
func serverCredentials() runbucket.Credentials {
	return runbucket.Credentials{
		AccessKeyID:     strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")),
		SecretAccessKey: strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY")),
	}
}

// newRunBuckets returns the run bucket provisioner that cfg.RunBuckets
// selects; creds are the server's own keys for the configured bucket.
func newRunBuckets(cfg *config.Config, creds runbucket.Credentials) (runbucket.Provisioner, error) {
	switch cfg.RunBuckets {
	case runbucket.ModeShared:
		p, err := runbucket.NewShared(cfg.S3Bucket, cfg.StorageSecretName)
		if err != nil {
			return nil, err
		}
		p.SetCredentials(creds)
		return p, nil
	default:
		return nil, fmt.Errorf("%s: unknown mode %q", runbucket.EnvMode, cfg.RunBuckets)
	}
}

// newRunAccess returns the server's access to the runs' buckets, or nil
// without an S3 endpoint.
func newRunAccess(cfg *config.Config, creds runbucket.Credentials, runs runbucket.RunLookup, prov runbucket.Provisioner) (*runbucket.Access, error) {
	if strings.TrimSpace(cfg.S3Endpoint) == "" {
		return nil, nil
	}
	if creds.Empty() {
		return nil, errors.New("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required")
	}
	return runbucket.NewAccess(runbucket.AccessConfig{
		Bucket:         cfg.S3Bucket,
		Credentials:    creds,
		Endpoint:       cfg.S3Endpoint,
		PublicEndpoint: cfg.S3PublicEndpoint,
	}, runs, prov, newS3Client(cfg.S3Region))
}

// newS3Client returns a factory of S3 clients in region.
func newS3Client(region string) runbucket.NewClientFunc {
	return func(endpoint string, creds runbucket.Credentials) (runbucket.Client, error) {
		return objstore.NewS3Client(objstore.S3Config{
			Endpoint:  strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"),
			AccessKey: creds.AccessKeyID,
			SecretKey: creds.SecretAccessKey,
			Region:    region,
			UseSSL:    strings.HasPrefix(endpoint, "https://"),
		})
	}
}

// gatewayRunStore presigns on the public S3 endpoint and lists on the
// server's, both with the run's credentials.
type gatewayRunStore struct {
	storagegw.Presigner
	storagegw.Lister
}

// gatewayRunStorage serves each task the bucket of its run, with the run's
// credentials. Tasks without a run keep the configured bucket and the
// server's credentials (shared); with storage isolation, the run bound lets
// them reach nothing. Standalone TES tasks have a stub run without a run
// root, which Access puts in the configured bucket. Presigner and lister
// are separate clients (public and server endpoint), each cached by Access.
func gatewayRunStorage(access *runbucket.Access, bucket string, shared storagegw.RunStore) storagegw.RunStorage {
	return func(ctx context.Context, task *domain.Task) (string, storagegw.RunStore, error) {
		if task.RunID == "" {
			return bucket, shared, nil
		}
		runBucket, presigner, err := access.PresignClient(ctx, task.RunID)
		if err != nil {
			return "", nil, err
		}
		_, lister, err := access.Client(ctx, task.RunID)
		if err != nil {
			return "", nil, err
		}
		return runBucket, gatewayRunStore{Presigner: presigner, Lister: lister}, nil
	}
}

// checkFederationRunBuckets refuses remote endpoints that reach Conquest's
// bucket with credentials of their own when runs get their own buckets:
// only storage gateway URLs follow a run's bucket and credentials.
func checkFederationRunBuckets(mode runbucket.Mode, fed *routing.Config) error {
	if !mode.PerRun() || fed == nil {
		return nil
	}
	for _, ep := range fed.Endpoints {
		if ep.S3URLStyle != remote.S3StyleGateway {
			return fmt.Errorf("%s=%s: endpoint %s must use s3_url_style %q: remote endpoints reach a run's own bucket only through the storage gateway",
				runbucket.EnvMode, mode, ep.Name, remote.S3StyleGateway)
		}
	}
	return nil
}
