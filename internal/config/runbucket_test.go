package config

import (
	"strings"
	"testing"

	"github.com/anuragxxd/conquest/internal/runbucket"
)

func TestLoad_RunBucketsAreSharedByDefault(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunBuckets != runbucket.ModeShared {
		t.Errorf("RunBuckets = %q, want %q", cfg.RunBuckets, runbucket.ModeShared)
	}
}

func TestLoad_RunBuckets(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_RUN_BUCKETS", "shared")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunBuckets != runbucket.ModeShared {
		t.Errorf("RunBuckets = %q, want %q", cfg.RunBuckets, runbucket.ModeShared)
	}
}

func TestLoad_RunBucketsRefusesUnknownModes(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_RUN_BUCKETS", "per-run")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CONQUEST_RUN_BUCKETS") {
		t.Errorf("err = %v, want a CONQUEST_RUN_BUCKETS error", err)
	}
}

func TestLoad_RunBucketsOBC(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_ENDPOINT", "http://s3.example:8333")
	t.Setenv("CONQUEST_STORAGE_ISOLATION", "true")
	t.Setenv("CONQUEST_STORAGE_GATEWAY_SECRET", strings.Repeat("s", 32))
	t.Setenv("CONQUEST_K8S_NAMESPACE", "workflows")
	t.Setenv("CONQUEST_RUN_BUCKETS", "obc")
	t.Setenv("CONQUEST_RUN_BUCKETS_OBC_STORAGE_CLASS", "object-bucket")
	t.Setenv("CONQUEST_RUN_BUCKETS_OBC_MAX_SIZE", "10Gi")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunBuckets != runbucket.ModeOBC {
		t.Errorf("RunBuckets = %q, want %q", cfg.RunBuckets, runbucket.ModeOBC)
	}
	want := runbucket.OBCConfig{Namespace: "workflows", StorageClassName: "object-bucket", MaxSize: "10Gi",
		BindTimeout: runbucket.DefaultOBCBindTimeout, Instance: "conquest-workflows"}
	if cfg.RunBucketsOBC != want {
		t.Errorf("RunBucketsOBC = %+v, want %+v", cfg.RunBucketsOBC, want)
	}
}

func TestLoad_RunBucketsOBCRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"no storage class": {map[string]string{"CONQUEST_STORAGE_ISOLATION": "true"}, "CONQUEST_RUN_BUCKETS_OBC_STORAGE_CLASS"},
		"no isolation":     {map[string]string{"CONQUEST_RUN_BUCKETS_OBC_STORAGE_CLASS": "sc"}, "CONQUEST_STORAGE_ISOLATION"},
		"objectfs": {map[string]string{"CONQUEST_STORAGE_ISOLATION": "true", "CONQUEST_RUN_BUCKETS_OBC_STORAGE_CLASS": "sc",
			"CONQUEST_WORKSPACE_MODE": "objectfs"}, "objectfs"},
		"bad bind timeout": {map[string]string{"CONQUEST_STORAGE_ISOLATION": "true", "CONQUEST_RUN_BUCKETS_OBC_STORAGE_CLASS": "sc",
			"CONQUEST_RUN_BUCKETS_OBC_BIND_TIMEOUT": "5m"}, "CONQUEST_RUN_BUCKETS_OBC_BIND_TIMEOUT"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
			t.Setenv("CONQUEST_S3_ENDPOINT", "http://s3.example:8333")
			t.Setenv("CONQUEST_STORAGE_GATEWAY_SECRET", strings.Repeat("s", 32))
			t.Setenv("CONQUEST_RUN_BUCKETS", "obc")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %s", err, tc.want)
			}
		})
	}
}

func TestLoad_SharedRunBucketsIgnoreOBCSettings(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_RUN_BUCKETS_OBC_BIND_TIMEOUT", "not a duration")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunBucketsOBC != (runbucket.OBCConfig{}) {
		t.Errorf("RunBucketsOBC = %+v, want none with shared", cfg.RunBucketsOBC)
	}
}
