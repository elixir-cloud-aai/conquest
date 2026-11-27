package runbucket

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/objstore"
)

// RunLookup reads runs; repository.RunStore satisfies it.
type RunLookup interface {
	Get(ctx context.Context, runID string) (*domain.Run, error)
}

// Client reads, writes, lists and presigns objects; objstore.S3Client
// satisfies it.
type Client interface {
	objstore.Client
	Presign(ctx context.Context, method, bucket, key string, expires time.Duration) (*url.URL, error)
}

// NewClientFunc returns an S3 client for an endpoint (http(s)://host[:port])
// that signs with creds.
type NewClientFunc func(endpoint string, creds Credentials) (Client, error)

// AccessConfig configures Access.
type AccessConfig struct {
	// Bucket is the configured bucket (CONQUEST_S3_BUCKET). The server
	// reaches runs in it with Credentials, whatever the provisioner, so runs
	// from before a switch to per-run buckets stay readable.
	Bucket string
	// Credentials are the server's own S3 keys for Bucket.
	Credentials Credentials
	// Endpoint is the S3 endpoint as the server reaches it.
	Endpoint string
	// PublicEndpoint is the S3 endpoint in presigned URLs, as their users
	// (task pods, remote endpoints) reach it; empty means Endpoint.
	PublicEndpoint string
}

// maxCached bounds each of Access's caches; a full cache is emptied.
const maxCached = 4096

// Access gives the server a run's bucket and an S3 client with the run's
// credentials for it. It caches buckets and clients in memory; credentials
// never leave the clients.
type Access struct {
	cfg       AccessConfig
	runs      RunLookup
	prov      Provisioner
	newClient NewClientFunc

	mu      sync.Mutex
	buckets map[string]string // run ID -> bucket
	clients map[clientKey]Client
}

type clientKey struct {
	endpoint, bucket string
}

// NewAccess returns Access to the runs' buckets. prov gives the credentials
// of buckets other than cfg.Bucket; it may be nil if every run is in
// cfg.Bucket.
func NewAccess(cfg AccessConfig, runs RunLookup, prov Provisioner, newClient NewClientFunc) (*Access, error) {
	if cfg.Bucket == "" || cfg.Endpoint == "" {
		return nil, errors.New("run bucket access: bucket and S3 endpoint are required")
	}
	if runs == nil || newClient == nil {
		return nil, errors.New("run bucket access: run lookup and client factory are required")
	}
	if cfg.PublicEndpoint == "" {
		cfg.PublicEndpoint = cfg.Endpoint
	}
	return &Access{
		cfg:       cfg,
		runs:      runs,
		prov:      prov,
		newClient: newClient,
		buckets:   map[string]string{},
		clients:   map[clientKey]Client{},
	}, nil
}

// Bucket returns the bucket of the run's objects, the host of its stored run
// root URI.
func (a *Access) Bucket(ctx context.Context, runID string) (string, error) {
	if runID == "" {
		return "", errors.New("run bucket access: run ID is required")
	}
	a.mu.Lock()
	bucket, ok := a.buckets[runID]
	a.mu.Unlock()
	if ok {
		return bucket, nil
	}
	run, err := a.runs.Get(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("run bucket access: read run %s: %w", runID, err)
	}
	if bucket, err = BucketOf(run.RunRootURI); err != nil {
		return "", fmt.Errorf("run bucket access: run %s: %w", runID, err)
	}
	a.mu.Lock()
	if len(a.buckets) >= maxCached {
		clear(a.buckets)
	}
	a.buckets[runID] = bucket
	a.mu.Unlock()
	return bucket, nil
}

// Client returns the run's bucket and a client for it on the server's S3
// endpoint.
func (a *Access) Client(ctx context.Context, runID string) (string, Client, error) {
	return a.client(ctx, runID, a.cfg.Endpoint)
}

// PresignClient returns the run's bucket and a client for it on the public
// S3 endpoint, for presigned URLs.
func (a *Access) PresignClient(ctx context.Context, runID string) (string, Client, error) {
	return a.client(ctx, runID, a.cfg.PublicEndpoint)
}

func (a *Access) client(ctx context.Context, runID, endpoint string) (string, Client, error) {
	bucket, err := a.Bucket(ctx, runID)
	if err != nil {
		return "", nil, err
	}
	key := clientKey{endpoint: endpoint, bucket: bucket}
	a.mu.Lock()
	c, ok := a.clients[key]
	a.mu.Unlock()
	if ok {
		return bucket, c, nil
	}
	creds, err := a.credentials(ctx, runID, bucket)
	if err != nil {
		return "", nil, err
	}
	if c, err = a.newClient(endpoint, creds); err != nil {
		return "", nil, fmt.Errorf("run bucket access: S3 client for run %s: %w", runID, err)
	}
	a.mu.Lock()
	if len(a.clients) >= maxCached {
		clear(a.clients)
	}
	a.clients[key] = c
	a.mu.Unlock()
	return bucket, c, nil
}

func (a *Access) credentials(ctx context.Context, runID, bucket string) (Credentials, error) {
	if bucket == a.cfg.Bucket {
		if a.cfg.Credentials.Empty() {
			return Credentials{}, errors.New("run bucket access: no S3 credentials for the configured bucket")
		}
		return a.cfg.Credentials, nil
	}
	if a.prov == nil {
		return Credentials{}, fmt.Errorf("run bucket access: run %s is in bucket %q, but no provisioner gives its credentials", runID, bucket)
	}
	creds, err := a.prov.Credentials(ctx, runID)
	if err != nil {
		return Credentials{}, fmt.Errorf("run bucket access: credentials of run %s: %w", runID, err)
	}
	if creds.Empty() {
		return Credentials{}, fmt.Errorf("run bucket access: provisioner gave no credentials for run %s", runID)
	}
	return creds, nil
}
