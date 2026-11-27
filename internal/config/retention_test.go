package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoad_RunRetentionIsOffByDefault(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunRetention != 0 {
		t.Errorf("RunRetention = %s, want 0 (off)", cfg.RunRetention)
	}
}

func TestLoad_RunRetention(t *testing.T) {
	for in, want := range map[string]time.Duration{"720h": 720 * time.Hour, " 1h30m ": 90 * time.Minute, "0": 0, "0s": 0} {
		t.Run(in, func(t *testing.T) {
			t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
			t.Setenv("CONQUEST_S3_ENDPOINT", "http://s3.example:8333")
			t.Setenv("CONQUEST_RUN_RETENTION", in)

			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RunRetention != want {
				t.Errorf("RunRetention = %s, want %s", cfg.RunRetention, want)
			}
		})
	}
}

func TestLoad_RunRetentionRefusesBadValues(t *testing.T) {
	for _, in := range []string{"30d", "-1h", "forever"} {
		t.Run(in, func(t *testing.T) {
			t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
			t.Setenv("CONQUEST_S3_ENDPOINT", "http://s3.example:8333")
			t.Setenv("CONQUEST_RUN_RETENTION", in)

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CONQUEST_RUN_RETENTION") {
				t.Errorf("err = %v, want a CONQUEST_RUN_RETENTION error", err)
			}
		})
	}
}

// Deleting runs' objects needs the object store.
func TestLoad_RunRetentionNeedsTheS3Endpoint(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_RUN_RETENTION", "720h")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CONQUEST_S3_ENDPOINT") {
		t.Errorf("err = %v, want a CONQUEST_S3_ENDPOINT error", err)
	}
}
