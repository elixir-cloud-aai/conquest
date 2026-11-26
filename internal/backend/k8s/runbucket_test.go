package k8s

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/anuragxxd/conquest/internal/backend"
	"github.com/anuragxxd/conquest/internal/domain"
)

// These tests hold the invariant that per-run buckets (internal/runbucket)
// rely on: workflow code in a runner or task pod can't reach another run's
// S3 credentials. Every run's credentials are a Secret in the same
// namespace, so a pod must get no ServiceAccount token (no API access) and
// no Secret but its own run's.

// podSecretNames returns every Secret a pod's containers can read: env and
// envFrom references, Secret and projected volumes, and inline CSI volumes'
// node publish Secrets. Image pull Secrets are left out: only the kubelet
// reads them.
func podSecretNames(spec corev1.PodSpec) []string {
	var names []string
	add := func(name string) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	containers := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	for _, c := range containers {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				add(e.ValueFrom.SecretKeyRef.Name)
			}
		}
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil {
				add(ef.SecretRef.Name)
			}
		}
	}
	for _, v := range spec.Volumes {
		switch {
		case v.Secret != nil:
			add(v.Secret.SecretName)
		case v.Projected != nil:
			for _, src := range v.Projected.Sources {
				if src.Secret != nil {
					add(src.Secret.Name)
				}
			}
		case v.CSI != nil && v.CSI.NodePublishSecretRef != nil:
			add(v.CSI.NodePublishSecretRef.Name)
		}
	}
	return names
}

// assertNoAPIToken fails if a pod can get a ServiceAccount token: automounted
// or through a projected volume.
func assertNoAPIToken(t *testing.T, spec corev1.PodSpec) {
	t.Helper()
	assertNoServiceAccountToken(t, spec)
	for _, v := range spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.ServiceAccountToken != nil {
				t.Fatalf("volume %s projects a ServiceAccount token", v.Name)
			}
		}
	}
}

type runnerPodCase struct {
	name     string
	engine   string
	binding  *domain.WorkspaceBinding
	ws       WorkspaceStrategy
	security PodSecurityConfig
}

func runnerPodCases() []runnerPodCase {
	pvc := &domain.WorkspaceBinding{
		Kind:       domain.WorkspaceKindPVC,
		ClaimName:  "conquest-workspace-run",
		AccessMode: domain.WorkspaceAccessModeRWX,
	}
	rwo := &domain.WorkspaceBinding{
		Kind:       domain.WorkspaceKindPVC,
		ClaimName:  "conquest-workspace-run",
		AccessMode: domain.WorkspaceAccessModeRWO,
	}
	objectfs := &domain.WorkspaceBinding{
		Kind:      domain.WorkspaceKindObjectFS,
		ClaimName: "conquest-workspace-run",
		PVName:    "conquest-objectfs-run",
	}
	var cases []runnerPodCase
	for _, engine := range []string{"nextflow", "snakemake", "wdl"} {
		for _, sec := range []struct {
			name string
			cfg  PodSecurityConfig
		}{{"restricted", restrictedPodSecurity}, {"baseline", PodSecurityConfig{}}} {
			cases = append(cases,
				runnerPodCase{engine + "/emptydir/" + sec.name, engine, nil, emptyDirWS(), sec.cfg},
				runnerPodCase{engine + "/pvc-rwx/" + sec.name, engine, pvc, &pvcWorkspaceRWX{}, sec.cfg},
				runnerPodCase{engine + "/pvc-rwo/" + sec.name, engine, rwo, &pvcWorkspaceRWO{}, sec.cfg},
				runnerPodCase{engine + "/objectfs/" + sec.name, engine, objectfs, &objectFSWorkspace{}, sec.cfg},
			)
		}
	}
	return cases
}

func TestRunnerPods_GetOnlyTheirOwnRunsSecretAndNoAPIToken(t *testing.T) {
	for _, tc := range runnerPodCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, own := range []string{"conquest-run-a", "conquest-run-b"} {
				run := domain.NewRun("WF", "1", tc.engine, "1", nil, "", "v1", "")
				job, err := buildRunnerJobSpec(run, RunConfig{
					Namespace:         "services",
					ServiceAccount:    "conquest",
					StorageSecretName: "storage-secrets",
					PodSecurity:       tc.security,
					RunnerToken:       func(string) string { return "token" },
				}, &backend.RunnerSpec{
					Image:             "example.com/conquest-runner:latest",
					Command:           []string{"/usr/local/bin/conquest"},
					Args:              []string{"runner", tc.engine},
					StorageSecretName: own,
				}, tc.ws, tc.binding)
				if err != nil {
					t.Fatalf("build runner job: %v", err)
				}
				spec := job.Spec.Template.Spec
				assertNoAPIToken(t, spec)
				if got := podSecretNames(spec); !slices.Equal(got, []string{own}) {
					t.Fatalf("runner pod reads Secrets %v, want only %s", got, own)
				}
			}
		})
	}
}

// Without a run Secret in the runner spec, the runner gets the configured
// one: the shared bucket's.
func TestRunnerPods_FallBackToTheConfiguredSecret(t *testing.T) {
	run := domain.NewRun("NFL", "DSL2", "nextflow", "24.10.4", nil, "", "v1", "")
	job, err := buildRunnerJobSpec(run, RunConfig{StorageSecretName: "storage-secrets"}, &backend.RunnerSpec{
		Image:   "example.com/conquest-runner:latest",
		Command: []string{"/usr/local/bin/conquest"},
	}, emptyDirWS(), nil)
	if err != nil {
		t.Fatalf("build runner job: %v", err)
	}
	if got := podSecretNames(job.Spec.Template.Spec); !slices.Equal(got, []string{"storage-secrets"}) {
		t.Fatalf("runner pod reads Secrets %v, want only storage-secrets", got)
	}
}

// The same invariant on the Jobs the backend creates in the API, for two
// runs side by side.
func TestRunBackend_SubmittedRunnerJobsKeepToTheirRunsSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	be := NewRunBackend(client, RunConfig{
		Namespace:         "conquest",
		ServiceAccount:    "conquest",
		StorageSecretName: "storage-secrets",
		Workspace:         WorkspaceConfig{Mode: "emptydir"},
		PodSecurity:       restrictedPodSecurity,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	want := map[string]string{}
	for _, secret := range []string{"conquest-run-a", "conquest-run-b"} {
		run := domain.NewRun("NFL", "DSL2", "nextflow", "24.10.4", nil, "", "v1", "")
		if _, err := be.SubmitRun(context.Background(), run, &backend.RunnerSpec{
			Image:             "example.com/conquest-runner:latest",
			Command:           []string{"/usr/local/bin/conquest"},
			StorageSecretName: secret,
		}); err != nil {
			t.Fatalf("submit run: %v", err)
		}
		want[run.RunID] = secret
	}

	jobs, err := client.BatchV1().Jobs("conquest").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != len(want) {
		t.Fatalf("got %d Jobs, want %d", len(jobs.Items), len(want))
	}
	for _, job := range jobs.Items {
		spec := job.Spec.Template.Spec
		assertNoAPIToken(t, spec)
		own := want[job.Labels[LabelRunID]]
		if got := podSecretNames(spec); !slices.Equal(got, []string{own}) {
			t.Fatalf("Job %s reads Secrets %v, want only %s", job.Name, got, own)
		}
	}
}

// Task pods of isolated deployments get no Secret at all and no API token,
// whatever the workspace.
func TestIsolatedTaskPods_GetNoSecretAndNoAPIToken(t *testing.T) {
	cases := []struct {
		name    string
		binding *domain.WorkspaceBinding
		ws      WorkspaceStrategy
	}{
		{"emptydir", nil, emptyDirWS()},
		{"pvc-rwx", &domain.WorkspaceBinding{
			Kind:       domain.WorkspaceKindPVC,
			ClaimName:  "conquest-workspace-run-123",
			AccessMode: domain.WorkspaceAccessModeRWX,
		}, &pvcWorkspaceRWX{}},
		{"objectfs", &domain.WorkspaceBinding{
			Kind:      domain.WorkspaceKindObjectFS,
			ClaimName: "conquest-workspace-run-123",
			PVName:    "conquest-objectfs-run-123",
		}, &objectFSWorkspace{}},
	}
	for _, tc := range cases {
		for name, sec := range map[string]PodSecurityConfig{"restricted": restrictedPodSecurity, "baseline": {}} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				task := domain.NewTask("run-123")
				task.TESTaskID = "tes-" + tc.name
				task.TaskRequestPayload = json.RawMessage(`{
					"executors": [{"image": "ubuntu:24.04", "command": ["true"], "workdir": "/work"}]
				}`)
				task.Workspace = tc.binding
				cfg := helperTestConfig()
				cfg.StorageSecretName = "storage-secrets"
				cfg.PodSecurity = sec
				cfg.TaskStorageURL = func(taskID string) string {
					return "http://conquest.ns.svc/storage/v1/t/" + taskID + "/sig"
				}

				job, err := buildJobSpec(task, cfg, tc.ws)
				if err != nil {
					t.Fatalf("build task job: %v", err)
				}
				spec := job.Spec.Template.Spec
				assertNoAPIToken(t, spec)
				if got := podSecretNames(spec); len(got) != 0 {
					t.Fatalf("isolated task pod reads Secrets %v, want none", got)
				}
			})
		}
	}
}
