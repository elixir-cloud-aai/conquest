package k8s

import (
	"testing"

	"github.com/anuragxxd/conquest/internal/backend"
	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/runnerauth"
)

// emptyDirWS returns the default emptydir workspace strategy for tests.
func emptyDirWS() WorkspaceStrategy {
	return &emptyDirWorkspace{}
}

func TestBuildRunnerJobSpec_UsesProvidedRunnerSpec(t *testing.T) {
	run := domain.NewRun("NFL", "DSL2", "nextflow", "24.10.4", nil, "", "v1", "")

	job, err := buildRunnerJobSpec(run, RunConfig{
		Namespace:               "services",
		ServiceAccount:          "conquest",
		StorageSecretName:       "custom-storage",
		ImagePullSecrets:        []string{"registry-a", "registry-b"},
		EphemeralStorageRequest: "12Gi",
		EphemeralStorageLimit:   "24Gi",
	}, &backend.RunnerSpec{
		Image:   "example.com/conquest-nf-25.04.3-runner:latest",
		Command: []string{"/usr/local/bin/conquest"},
		Args:    []string{"runner", "nextflow", "--run-id", run.RunID},
		Env: []backend.EnvVar{
			{Name: "CONQUEST_WORKFLOW_PARAMS", Value: `{"hello":"world"}`},
			{Name: "NXF_PLUGINS_ALLOWED", Value: "nf-ga4gh,nf-amazon"},
		},
	}, emptyDirWS(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if got := container.Image; got != "example.com/conquest-nf-25.04.3-runner:latest" {
		t.Fatalf("runner image = %q", got)
	}
	if got := container.Command[0]; got != "/usr/local/bin/conquest" {
		t.Fatalf("runner command = %q", got)
	}
	if got := container.Args[0]; got != "runner" {
		t.Fatalf("runner arg[0] = %q", got)
	}

	if len(job.Spec.Template.Spec.ImagePullSecrets) != 2 {
		t.Fatalf("imagePullSecrets = %d, want 2", len(job.Spec.Template.Spec.ImagePullSecrets))
	}

	foundParams := false
	foundAllowedPlugins := false
	foundAccessKey := false
	for _, env := range container.Env {
		switch env.Name {
		case "CONQUEST_WORKFLOW_PARAMS":
			foundParams = env.Value == `{"hello":"world"}`
		case "NXF_PLUGINS_ALLOWED":
			foundAllowedPlugins = env.Value == "nf-ga4gh,nf-amazon"
		case "AWS_ACCESS_KEY_ID":
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
				foundAccessKey = env.ValueFrom.SecretKeyRef.Name == "custom-storage"
			}
		}
	}

	if !foundParams {
		t.Fatal("runner env missing CONQUEST_WORKFLOW_PARAMS")
	}
	if !foundAllowedPlugins {
		t.Fatal("runner env missing NXF_PLUGINS_ALLOWED")
	}
	if !foundAccessKey {
		t.Fatal("runner env missing AWS_ACCESS_KEY_ID from configured secret")
	}
	if got := container.Resources.Requests.StorageEphemeral().String(); got != "12Gi" {
		t.Fatalf("ephemeral request = %q", got)
	}
	if got := container.Resources.Limits.StorageEphemeral().String(); got != "24Gi" {
		t.Fatalf("ephemeral limit = %q", got)
	}
}

func TestBuildRunnerJobSpec_RequiresImage(t *testing.T) {
	run := domain.NewRun("NFL", "DSL2", "nextflow", "24.10.4", nil, "", "v1", "")

	_, err := buildRunnerJobSpec(run, RunConfig{}, &backend.RunnerSpec{Command: []string{"/usr/local/bin/conquest"}}, emptyDirWS(), nil)
	if err == nil {
		t.Fatal("expected error for missing runner image")
	}
}

func TestBuildRunnerJobSpec_OmitsNextflowHomeForSnakemake(t *testing.T) {
	run := domain.NewRun("SMK", "9.17.3", "snakemake", "9.17.3", nil, "", "v1", "")

	job, err := buildRunnerJobSpec(run, RunConfig{}, &backend.RunnerSpec{
		Image:   "example.com/conquest-smk-9.17.3-runner:latest",
		Command: []string{"/usr/local/bin/conquest"},
		Args:    []string{"runner", "snakemake"},
	}, emptyDirWS(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, mount := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.MountPath == "/.nextflow" {
			t.Fatal("snakemake runner should not mount /.nextflow")
		}
	}
}

func TestBuildRunnerJobSpec_PVCMountsWorkspace(t *testing.T) {
	run := domain.NewRun("SMK", "9.17.3", "snakemake", "9.17.3", nil, "", "v1", "")

	binding := &domain.WorkspaceBinding{
		Kind:       domain.WorkspaceKindPVC,
		ClaimName:  "conquest-workspace-test",
		AccessMode: domain.WorkspaceAccessModeRWX,
	}
	ws := &pvcWorkspaceRWX{}

	job, err := buildRunnerJobSpec(run, RunConfig{}, &backend.RunnerSpec{
		Image:   "example.com/conquest-smk-9.17.3-runner:latest",
		Command: []string{"/usr/local/bin/conquest"},
		Args:    []string{"runner", "snakemake"},
	}, ws, binding)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that /workspace is PVC-backed with subPath: shared.
	container := job.Spec.Template.Spec.Containers[0]
	var workspaceMount *string
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == "/workspace" {
			sp := mount.SubPath
			workspaceMount = &sp
		}
	}
	if workspaceMount == nil {
		t.Fatal("runner should mount /workspace")
	}
	if *workspaceMount != "shared" {
		t.Fatalf("workspace subPath = %q, want 'shared'", *workspaceMount)
	}

	// Check /tmp is still emptyDir.
	foundTmp := false
	for _, vol := range job.Spec.Template.Spec.Volumes {
		if vol.Name == "tmp" && vol.EmptyDir != nil {
			foundTmp = true
		}
	}
	if !foundTmp {
		t.Fatal("/tmp should remain emptyDir")
	}

	// Check CONQUEST_SHARED_WORKSPACE env var.
	foundSharedFlag := false
	for _, env := range container.Env {
		if env.Name == "CONQUEST_SHARED_WORKSPACE" && env.Value == "true" {
			foundSharedFlag = true
		}
	}
	if !foundSharedFlag {
		t.Fatal("PVC mode should inject CONQUEST_SHARED_WORKSPACE=true")
	}
}

func TestBuildRunnerJobSpec_EmptyDirNoSharedWorkspaceEnv(t *testing.T) {
	run := domain.NewRun("SMK", "9.17.3", "snakemake", "9.17.3", nil, "", "v1", "")

	job, err := buildRunnerJobSpec(run, RunConfig{}, &backend.RunnerSpec{
		Image:   "example.com/conquest-smk-9.17.3-runner:latest",
		Command: []string{"/usr/local/bin/conquest"},
		Args:    []string{"runner", "snakemake"},
	}, emptyDirWS(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	for _, env := range container.Env {
		if env.Name == "CONQUEST_SHARED_WORKSPACE" {
			t.Fatal("emptydir mode should not inject CONQUEST_SHARED_WORKSPACE")
		}
	}
}

func TestBuildRunnerJobSpec_RunnerToken(t *testing.T) {
	run := domain.NewRun("NFL", "DSL2", "nextflow", "24.10.4", nil, "", "v1", "")
	spec := &backend.RunnerSpec{Image: "runner:1", Command: []string{"/usr/local/bin/conquest"}}
	tokenEnv := func(cfg RunConfig) []string {
		job, err := buildRunnerJobSpec(run, cfg, spec, emptyDirWS(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var values []string
		for _, env := range job.Spec.Template.Spec.Containers[0].Env {
			if env.Name == runnerauth.EnvToken {
				values = append(values, env.Value)
			}
		}
		return values
	}

	if got := tokenEnv(RunConfig{}); len(got) != 0 {
		t.Fatalf("token without RunnerToken: %v", got)
	}
	var askedFor string
	got := tokenEnv(RunConfig{RunnerToken: func(id string) string { askedFor = id; return "tok-" + id }})
	if len(got) != 1 || got[0] != "tok-"+run.RunID || askedFor != run.RunID {
		t.Fatalf("token env %v (asked for %q), want the run's token once", got, askedFor)
	}
}
