package config

import (
	"strings"
	"testing"
)

func TestLoad_StorageIsolationIsOffByDefault(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorageIsolation {
		t.Error("StorageIsolation = true by default")
	}
}

func TestLoad_StorageIsolation(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_ENDPOINT", "http://minio:9000")
	t.Setenv("CONQUEST_STORAGE_ISOLATION", "true")
	t.Setenv("CONQUEST_STORAGE_GATEWAY_SECRET", strings.Repeat("s", 32))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.StorageIsolation {
		t.Error("StorageIsolation = false")
	}
}

func TestLoad_StorageIsolationNeedsGatewaySecretAndS3(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no secret":    {"CONQUEST_S3_ENDPOINT": "http://minio:9000"},
		"short secret": {"CONQUEST_S3_ENDPOINT": "http://minio:9000", "CONQUEST_STORAGE_GATEWAY_SECRET": "short"},
		"no endpoint":  {"CONQUEST_STORAGE_GATEWAY_SECRET": strings.Repeat("s", 32)},
		"not a bool":   {"CONQUEST_STORAGE_ISOLATION": "yes please"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
			t.Setenv("CONQUEST_STORAGE_ISOLATION", "true")
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CONQUEST_STORAGE_ISOLATION") {
				t.Errorf("err = %v", err)
			}
		})
	}
}
