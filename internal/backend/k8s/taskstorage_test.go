package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/anuragxxd/conquest/internal/isolation"
)

func storageEnvOf(c corev1.Container) map[string]corev1.EnvVar {
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		if strings.HasPrefix(e.Name, "AWS_") || strings.HasPrefix(e.Name, "CONQUEST_S3_") || e.Name == isolation.EnvTaskStorageURL {
			env[e.Name] = e
		}
	}
	return env
}

func TestBuildJobSpec_TaskPodsGetS3CredentialsByDefault(t *testing.T) {
	job, err := buildJobSpec(helperTestTask(t), helperTestConfig(), emptyDirWS())
	if err != nil {
		t.Fatal(err)
	}

	pod := job.Spec.Template.Spec
	for _, c := range []corev1.Container{pod.InitContainers[1], pod.Containers[0]} {
		env := storageEnvOf(c)
		if env["AWS_SECRET_ACCESS_KEY"].ValueFrom == nil || env["CONQUEST_S3_ENDPOINT"].Value != "http://minio:9000" {
			t.Errorf("%s: storage env = %v", c.Name, env)
		}
		if _, ok := env[isolation.EnvTaskStorageURL]; ok {
			t.Errorf("%s: has %s without isolation", c.Name, isolation.EnvTaskStorageURL)
		}
	}
}

func TestBuildJobSpec_IsolatedTaskPodsGetOnlyTheirGatewayURL(t *testing.T) {
	task := helperTestTask(t)
	cfg := helperTestConfig()
	cfg.StorageSecretName = "storage-secrets"
	cfg.TaskStorageURL = func(taskID string) string {
		return "http://conquest.ns.svc/storage/v1/t/" + taskID + "/sig"
	}

	job, err := buildJobSpec(task, cfg, emptyDirWS())
	if err != nil {
		t.Fatal(err)
	}

	pod := job.Spec.Template.Spec
	want := "http://conquest.ns.svc/storage/v1/t/" + task.TaskID + "/sig"
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		env := storageEnvOf(c)
		for name := range env {
			if name != isolation.EnvTaskStorageURL {
				t.Errorf("%s: has %s", c.Name, name)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				t.Errorf("%s: env %s comes from Secret %s", c.Name, e.Name, e.ValueFrom.SecretKeyRef.Name)
			}
		}
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil {
				t.Errorf("%s: envFrom Secret %s", c.Name, ef.SecretRef.Name)
			}
		}
		if c.Name == "copy-tools" {
			continue
		}
		if got := env[isolation.EnvTaskStorageURL].Value; got != want {
			t.Errorf("%s: %s = %q, want %q", c.Name, isolation.EnvTaskStorageURL, got, want)
		}
	}
	for _, v := range pod.Volumes {
		if v.Secret != nil {
			t.Errorf("volume %s mounts Secret %s", v.Name, v.Secret.SecretName)
		}
	}
}
