package k8s

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/isolation"
)

// taskStorageEnv returns how a task pod's filer reaches object storage: the
// S3 settings and the bucket-wide credentials, or, with cfg.TaskStorageURL,
// only the task's storage gateway URL and no credentials.
func taskStorageEnv(task *domain.Task, cfg Config) (settings, credentials []corev1.EnvVar) {
	if cfg.TaskStorageURL != nil {
		return []corev1.EnvVar{{Name: isolation.EnvTaskStorageURL, Value: cfg.TaskStorageURL(task.TaskID)}}, nil
	}
	return []corev1.EnvVar{
		{Name: "CONQUEST_S3_ENDPOINT", Value: cfg.S3Endpoint},
		{Name: "CONQUEST_S3_BUCKET", Value: cfg.S3Bucket},
		{Name: "CONQUEST_S3_REGION", Value: cfg.S3Region},
	}, awsSecretEnvVars(cfg.StorageSecretName)
}
