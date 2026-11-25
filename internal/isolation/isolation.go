// Package isolation keeps object storage credentials out of task pods.
//
// Without it, every pod of a local task gets the bucket-wide S3 credentials
// (stage-in and the executor's stage-out need them), so any task, and thus
// any user who may submit a run, can read and write every run's objects.
//
// With CONQUEST_STORAGE_ISOLATION=true, task pods get no S3 credentials and
// no S3 endpoint. Each gets one capability URL of the storage gateway
// (internal/storagegw) instead, on Conquest's in-cluster address:
//
//	<internal base URL>/storage/v1/t/<task ID>/<HMAC>
//
// Its filer reads inputs, lists DIRECTORY inputs and writes outputs below
// that URL (Client), and the gateway answers with redirects to S3 URLs
// presigned for one object and method. What a task may reach is decided by
// the gateway from its stored request, as for remote endpoints, and is
// bounded to the task's run root (RunBound): a task request that names
// another run's objects is refused. The bound applies to the tasks of remote
// endpoints that use the gateway as well. URLs stop working shortly after the
// task ends.
//
// The bound trusts the task's run ID, which the runner callback API and the
// public TES API's conquest_run_id tag take from the caller; isolation is only
// as strong as the authentication of those APIs.
//
// The runner pods of the workflow engines are not covered: they still get
// the S3 credentials, because the engines read and write the run's work
// directory with their own S3 clients.
package isolation

import (
	"context"

	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/storage"
	"github.com/anuragxxd/conquest/internal/storagegw"
)

// EnvTaskStorageURL holds a task pod's capability URL of the storage gateway.
// When it is set, the filer uses it instead of S3 credentials.
const EnvTaskStorageURL = "CONQUEST_TASK_STORAGE_URL"

// RunBound confines tasks to their run's root in the layout's bucket,
// s3://<bucket>/<version>/workflow-runs/<run ID>/. A task without a run, or a
// layout without a bucket, may reach nothing.
func RunBound(layout storage.Layout) storagegw.Bound {
	return func(_ context.Context, task *domain.Task) (string, error) {
		if task == nil || task.RunID == "" || layout.Bucket == "" {
			return "", nil
		}
		return layout.Bucket + "/" + layout.RunRoot(task.RunID), nil
	}
}
