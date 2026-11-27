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
