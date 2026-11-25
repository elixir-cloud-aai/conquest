package config

import (
	"strings"
	"testing"
)

func TestLoad_ParsesRunnerImageMapAndPullSecrets(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_NEXTFLOW_RUNNER_IMAGES", `{"25.04.3":"example.com/runner:25.04.3"}`)
	t.Setenv("CONQUEST_WDL_RUNNER_IMAGES", `{"1.12.1":"example.com/wdl-runner:1.12.1"}`)
	t.Setenv("CONQUEST_WDL_ALLOW_EXTERNAL_IMPORTS", "true")
	t.Setenv("CONQUEST_TASK_REJECT_ON_MAX_VIOLATION", "true")
	t.Setenv("CONQUEST_IMAGE_PULL_SECRETS", "primary, mirror ")
	t.Setenv("CONQUEST_RECONCILER_INTERVAL", "11s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.NextflowRunnerImages["25.04.3"]; got != "example.com/runner:25.04.3" {
		t.Fatalf("runner image mapping = %q", got)
	}
	if got := cfg.WDLRunnerImages["1.12.1"]; got != "example.com/wdl-runner:1.12.1" {
		t.Fatalf("wdl runner image mapping = %q", got)
	}
	if !cfg.WDLAllowExternalImports {
		t.Fatal("expected WDL external import flag to be enabled")
	}
	if !cfg.TaskRejectOnMaxViolation {
		t.Fatal("expected task max-resource policy to reject when configured")
	}
	if len(cfg.ImagePullSecrets) != 2 {
		t.Fatalf("image pull secrets = %v", cfg.ImagePullSecrets)
	}
	if cfg.ReconcilerInterval.String() != "11s" {
		t.Fatalf("reconciler interval = %s", cfg.ReconcilerInterval)
	}
}

func TestLoad_SetsSnakemakeTaskImageDefault(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_SNAKEMAKE_RUNNER_IMAGE", "example.com/conquest-snakemake:latest")
	t.Setenv("CONQUEST_SNAKEMAKE_DEFAULT_DISK_MB", "12288")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SnakemakeTaskImage != cfg.SnakemakeRunnerImage {
		t.Fatalf("task image = %q, runner image = %q", cfg.SnakemakeTaskImage, cfg.SnakemakeRunnerImage)
	}
	if cfg.SnakemakeDefaultDiskMB != 12288 {
		t.Fatalf("snakemake default disk = %d", cfg.SnakemakeDefaultDiskMB)
	}
}

func TestLoad_RejectsInvalidRunnerImageMap(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_NEXTFLOW_RUNNER_IMAGES", `{invalid`)

	if _, err := Load(); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoad_RejectsInvalidWDLRunnerImageMap(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_WDL_RUNNER_IMAGES", `{invalid`)

	if _, err := Load(); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoad_RejectsMissingSnakemakeRunnerImage(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_SNAKEMAKE_TASK_IMAGE", "example.com/conquest-snakemake:task")

	if _, err := Load(); err == nil {
		t.Fatal("expected snakemake image validation error")
	}
}

func TestLoad_RejectsInvalidSnakemakeDefaultDisk(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_SNAKEMAKE_DEFAULT_DISK_MB", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected snakemake disk validation error")
	}
}

func TestLoad_AcceptsObjectFSWorkspaceMode(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_WORKSPACE_MODE", "objectfs")
	t.Setenv("CONQUEST_WORKSPACE_SIZE", "20Gi")
	t.Setenv("CONQUEST_STORAGE_SECRET_NAME", "storage-secrets")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WorkspaceMode != "objectfs" {
		t.Fatalf("workspace mode = %q", cfg.WorkspaceMode)
	}
}

func TestLoad_ObjectFSUsesExistingS3Config(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "my-bucket")
	t.Setenv("CONQUEST_WORKSPACE_MODE", "objectfs")
	t.Setenv("CONQUEST_WORKSPACE_SIZE", "50Gi")
	t.Setenv("CONQUEST_STORAGE_SECRET_NAME", "my-secrets")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ObjectFS reuses the existing S3 config.
	if cfg.S3Bucket != "my-bucket" {
		t.Fatalf("S3Bucket = %q", cfg.S3Bucket)
	}
	if cfg.WorkspaceSize != "50Gi" {
		t.Fatalf("WorkspaceSize = %q", cfg.WorkspaceSize)
	}
	if cfg.StorageSecretName != "my-secrets" {
		t.Fatalf("StorageSecretName = %q", cfg.StorageSecretName)
	}
}

func TestLoad_DefaultsToRestrictedPodSecurity(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PodSecurity != "restricted" {
		t.Fatalf("pod security = %q, want restricted", cfg.PodSecurity)
	}
	if cfg.PodRunAsUser != 1000 || cfg.PodRunAsGroup != 1000 {
		t.Fatalf("pod UID:GID = %d:%d, want 1000:1000", cfg.PodRunAsUser, cfg.PodRunAsGroup)
	}
}

func TestLoad_RejectsRootUIDForRestrictedPodSecurity(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_POD_RUN_AS_USER", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected root UID to be rejected for restricted pod security")
	}
}

func TestLoad_RejectsInvalidPodSecurity(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_POD_SECURITY", "privileged")

	if _, err := Load(); err == nil {
		t.Fatal("expected invalid pod security profile error")
	}
}

func TestLoad_RejectsInvalidWorkspaceMode(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_WORKSPACE_MODE", "invalid")

	if _, err := Load(); err == nil {
		t.Fatal("expected invalid workspace mode error")
	}
}

func TestLoad_FloorsTaskMemoryByDefault(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_SNAKEMAKE_RUNNER_IMAGE", "example.com/conquest-snakemake:latest")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TaskMinRAMGb != 0.125 {
		t.Fatalf("default task RAM floor = %v, want 0.125", cfg.TaskMinRAMGb)
	}

	t.Setenv("CONQUEST_TASK_MIN_RAM_GB", "0")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TaskMinRAMGb != 0 {
		t.Fatalf("task RAM floor with CONQUEST_TASK_MIN_RAM_GB=0 = %v, want 0", cfg.TaskMinRAMGb)
	}
}

func TestLoad_ReadsTaskHelperResources(t *testing.T) {
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TaskHelperResources != "" {
		t.Fatalf("default task helper resources = %q, want none", cfg.TaskHelperResources)
	}

	raw := `{"limits": {"cpu": "500m"}}`
	t.Setenv("CONQUEST_TASK_HELPER_RESOURCES", raw)
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TaskHelperResources != raw {
		t.Fatalf("task helper resources = %q, want %q", cfg.TaskHelperResources, raw)
	}
}

// clearAuthEnv unsets the auth settings for the test, so values exported
// in the environment don't leak in.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"CONQUEST_AUTH_ENABLED", "CONQUEST_AUTH_SERVICE_INFO_PUBLIC", "CONQUEST_AUTH_CONFIG"} {
		t.Setenv(key, "")
	}
}

func TestLoad_AuthDefaults(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthEnabled || !cfg.AuthServiceInfoPublic || cfg.AuthConfigFile != "/etc/conquest/auth/config.yaml" {
		t.Fatalf("auth = %v, service-info public %v, file %q", cfg.AuthEnabled, cfg.AuthServiceInfoPublic, cfg.AuthConfigFile)
	}
}

func TestLoad_AuthSettings(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
	t.Setenv("CONQUEST_S3_BUCKET", "conquest")
	t.Setenv("CONQUEST_AUTH_ENABLED", "true")
	t.Setenv("CONQUEST_AUTH_SERVICE_INFO_PUBLIC", "false")
	t.Setenv("CONQUEST_AUTH_CONFIG", "/config/auth.yaml")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.AuthEnabled || cfg.AuthServiceInfoPublic || cfg.AuthConfigFile != "/config/auth.yaml" {
		t.Fatalf("auth = %v, service-info public %v, file %q", cfg.AuthEnabled, cfg.AuthServiceInfoPublic, cfg.AuthConfigFile)
	}
}

func TestLoad_RejectsInvalidAuthSwitch(t *testing.T) {
	for _, key := range []string{"CONQUEST_AUTH_ENABLED", "CONQUEST_AUTH_SERVICE_INFO_PUBLIC"} {
		t.Run(key, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv("CONQUEST_POSTGRES_DSN", "postgres://example")
			t.Setenv("CONQUEST_S3_BUCKET", "conquest")
			t.Setenv(key, "ture")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("err = %v, want an error naming %s", err, key)
			}
		})
	}
}
