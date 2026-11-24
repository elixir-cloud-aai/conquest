package k8s

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/anuragxxd/conquest/internal/domain"
)

func helperTestTask(t *testing.T) *domain.Task {
	t.Helper()
	task := domain.NewTask("run-123")
	task.TESTaskID = "tes-123"
	task.TaskRequestPayload = json.RawMessage(`{
		"name": "test-task",
		"executors": [{"image": "ubuntu:24.04", "command": ["echo", "hi"], "workdir": "/work"}],
		"resources": {"cpu_cores": 2, "ram_gb": 4}
	}`)
	return task
}

func helperTestConfig() Config {
	return Config{
		Namespace:     "services",
		ConquestImage: "example.com/conquest:latest",
		S3Bucket:      "conquest",
		S3Endpoint:    "http://minio:9000",
		S3Region:      "us-east-1",
	}
}

func TestBuildJobSpec_HelperContainersHaveNoResourcesByDefault(t *testing.T) {
	job, err := buildJobSpec(helperTestTask(t), helperTestConfig(), emptyDirWS())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, c := range job.Spec.Template.Spec.InitContainers {
		if len(c.Resources.Requests) != 0 || len(c.Resources.Limits) != 0 {
			t.Fatalf("%s resources = %+v, want none", c.Name, c.Resources)
		}
	}
}

func TestBuildJobSpec_AppliesHelperResourcesToInitContainers(t *testing.T) {
	cfg := helperTestConfig()
	cfg.HelperResources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}

	job, err := buildJobSpec(helperTestTask(t), cfg, emptyDirWS())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	initContainers := job.Spec.Template.Spec.InitContainers
	names := make([]string, 0, len(initContainers))
	for _, c := range initContainers {
		names = append(names, c.Name)
		if got := c.Resources.Limits.Cpu().String(); got != "500m" {
			t.Fatalf("%s cpu limit = %q, want 500m", c.Name, got)
		}
		if got := c.Resources.Limits.Memory().String(); got != "256Mi" {
			t.Fatalf("%s memory limit = %q, want 256Mi", c.Name, got)
		}
		if got := c.Resources.Requests.Cpu().String(); got != "100m" {
			t.Fatalf("%s cpu request = %q, want 100m", c.Name, got)
		}
		if got := c.Resources.Requests.Memory().String(); got != "128Mi" {
			t.Fatalf("%s memory request = %q, want 128Mi", c.Name, got)
		}
	}
	if got := strings.Join(names, ","); got != "copy-tools,stage-in" {
		t.Fatalf("init containers = %s, want copy-tools,stage-in", got)
	}

	// The executor keeps the resources of the TES request.
	executor := job.Spec.Template.Spec.Containers[0]
	if got := executor.Resources.Limits.Cpu().String(); got != "2" {
		t.Fatalf("executor cpu limit = %q, want 2", got)
	}
	if got := executor.Resources.Limits.Memory().String(); got != "4Gi" {
		t.Fatalf("executor memory limit = %q, want 4Gi", got)
	}

	// Each container gets its own copy, so a change to one pod spec never
	// leaks into the shared configuration or the other container.
	initContainers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("3")
	if got := initContainers[1].Resources.Limits.Cpu().String(); got != "500m" {
		t.Fatalf("stage-in cpu limit changed with copy-tools: %q", got)
	}
	if got := cfg.HelperResources.Limits.Cpu().String(); got != "500m" {
		t.Fatalf("configured cpu limit changed with the pod spec: %q", got)
	}
}

func TestBuildJobSpec_HelperResourcesKeepRestrictedPodSecurity(t *testing.T) {
	cfg := helperTestConfig()
	cfg.PodSecurity = restrictedPodSecurity
	cfg.HelperResources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
	}

	job, err := buildJobSpec(helperTestTask(t), cfg, emptyDirWS())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertRestrictedPod(t, job.Spec.Template)
}

func TestParseHelperResources(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string // limits.cpu, "" for none
		wantErr string
	}{
		{name: "empty", raw: "", want: ""},
		{name: "blank", raw: "  ", want: ""},
		{name: "empty object", raw: "{}", want: ""},
		{
			name: "requests and limits",
			raw:  `{"requests": {"cpu": "100m", "memory": "128Mi"}, "limits": {"cpu": "500m", "memory": "256Mi"}}`,
			want: "500m",
		},
		{name: "limits only", raw: `{"limits": {"cpu": "1"}}`, want: "1"},
		{name: "invalid JSON", raw: `{"limits":`, wantErr: "parse"},
		{name: "unknown field", raw: `{"limit": {"cpu": "1"}}`, wantErr: "unknown field"},
		{name: "invalid quantity", raw: `{"limits": {"cpu": "lots"}}`, wantErr: "parse"},
		{name: "trailing data", raw: `{} {}`, wantErr: "trailing"},
		{
			name:    "request above limit",
			raw:     `{"requests": {"memory": "1Gi"}, "limits": {"memory": "512Mi"}}`,
			wantErr: "memory request 1Gi exceeds its limit 512Mi",
		},
		{name: "negative", raw: `{"limits": {"cpu": "-1"}}`, wantErr: "negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseHelperResources(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			cpu := ""
			if q, ok := got.Limits[corev1.ResourceCPU]; ok {
				cpu = q.String()
			}
			if cpu != tc.want {
				t.Fatalf("cpu limit = %q, want %q", cpu, tc.want)
			}
		})
	}
}
