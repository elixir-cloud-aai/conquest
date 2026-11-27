// Package controller implements the business logic orchestration layer.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/anuragxxd/conquest/internal/adapter"
	"github.com/anuragxxd/conquest/internal/backend"
	"github.com/anuragxxd/conquest/internal/domain"
	"github.com/anuragxxd/conquest/internal/objstore"
	"github.com/anuragxxd/conquest/internal/repository"
	"github.com/anuragxxd/conquest/internal/runbucket"
	"github.com/anuragxxd/conquest/internal/storage"
)

// ---------------------------------------------------------------------------
// Controller-level types (no dependency on api/wes or api/tes)
// ---------------------------------------------------------------------------

// RunRequest is the controller's view of a parsed WES submission.
type RunRequest struct {
	WorkflowType             string            `json:"workflow_type"`
	WorkflowTypeVersion      string            `json:"workflow_type_version"`
	WorkflowURL              string            `json:"workflow_url"`
	WorkflowParams           json.RawMessage   `json:"workflow_params,omitempty"`
	WorkflowEngine           string            `json:"workflow_engine,omitempty"`
	WorkflowEngineVersion    string            `json:"workflow_engine_version,omitempty"`
	WorkflowEngineParameters json.RawMessage   `json:"workflow_engine_parameters,omitempty"`
	Tags                     map[string]string `json:"tags,omitempty"`
}

// RunLog is the controller's assembled run response.
type RunLog struct {
	Run         *domain.Run
	TaskLogsURL string
	Outputs     json.RawMessage
}

// RunListResult holds a page of runs.
type RunListResult struct {
	Runs      []*domain.Run
	NextToken string
}

// TaskListResult holds a page of tasks.
type TaskListResult struct {
	Tasks     []*domain.Task
	NextToken string
}

// StateCounts maps states to counts.
type StateCounts map[domain.State]int

// ---------------------------------------------------------------------------
// RunController
// ---------------------------------------------------------------------------

// RunController handles WES run lifecycle operations.
type RunController struct {
	runs        repository.RunStore
	tasks       repository.TaskStore
	events      repository.EventStore
	layout      storage.Layout
	adapters    *adapter.Registry
	runBackend  backend.RunBackend  // nil if no runner backend configured
	taskBackend backend.TaskBackend // nil if no task backend configured
	artifacts   objstore.Client
	runBuckets  runbucket.Provisioner // nil: every run uses layout.Bucket
	runObjects  RunObjects            // nil: artifacts, in the run's bucket
	log         *slog.Logger
}

// RunObjects gives the server a run's bucket and a client with the run's
// credentials for it; runbucket.Access satisfies it.
type RunObjects interface {
	Client(ctx context.Context, runID string) (string, runbucket.Client, error)
}

// NewRunController creates a RunController.
func NewRunController(
	runs repository.RunStore,
	tasks repository.TaskStore,
	events repository.EventStore,
	layout storage.Layout,
	adapters *adapter.Registry,
	log *slog.Logger,
) *RunController {
	return &RunController{
		runs:     runs,
		tasks:    tasks,
		events:   events,
		layout:   layout,
		adapters: adapters,
		log:      log,
	}
}

// SetRunBackend configures the run-level execution backend.
func (c *RunController) SetRunBackend(be backend.RunBackend) {
	c.runBackend = be
}

// SetTaskBackend configures the task-level execution backend.
func (c *RunController) SetTaskBackend(be backend.TaskBackend) {
	c.taskBackend = be
}

// SetArtifactStore configures the optional artifact store used to read
// persisted run-level artifacts such as workflow outputs. SetRunObjects, which
// the server uses, takes precedence.
func (c *RunController) SetArtifactStore(store objstore.Client) {
	c.artifacts = store
}

// SetRunBuckets configures where each new run's objects live and which
// credentials its runner gets (internal/runbucket). Without it, every run
// uses the layout's bucket and the run backend's configured Secret.
func (c *RunController) SetRunBuckets(p runbucket.Provisioner) {
	c.runBuckets = p
}

// SetRunObjects makes the controller read a run's objects, such as its
// workflow outputs, with the run's credentials (internal/runbucket), instead
// of the artifact store's.
func (c *RunController) SetRunObjects(o RunObjects) {
	c.runObjects = o
}

// Layout returns the storage layout used by this controller.
func (c *RunController) Layout() storage.Layout {
	return c.layout
}

// LayoutOf returns the layout of a run's objects: the controller's, in the
// bucket of the run's stored run root. Runs without a readable run root get
// the controller's layout.
func (c *RunController) LayoutOf(run *domain.Run) storage.Layout {
	layout := c.layout
	if run == nil {
		return layout
	}
	if bucket, err := runbucket.BucketOf(run.RunRootURI); err == nil {
		layout.Bucket = bucket
	}
	return layout
}

// RunLayout returns the layout of the run's objects (LayoutOf).
func (c *RunController) RunLayout(ctx context.Context, runID string) (storage.Layout, error) {
	run, err := c.runs.Get(ctx, runID)
	if err != nil {
		return storage.Layout{}, err
	}
	return c.LayoutOf(run), nil
}

func (c *RunController) SupportedEngines() []string {
	if c.adapters == nil {
		return nil
	}
	return c.adapters.Names()
}

// SubmitRun validates and persists a new workflow run.
func (c *RunController) SubmitRun(ctx context.Context, req *RunRequest) (*domain.Run, error) {
	if req.WorkflowType == "" {
		return nil, &adapter.ValidationError{Field: "workflow_type", Msg: "required"}
	}
	if req.WorkflowTypeVersion == "" {
		return nil, &adapter.ValidationError{Field: "workflow_type_version", Msg: "required"}
	}
	if req.WorkflowURL == "" {
		return nil, &adapter.ValidationError{Field: "workflow_url", Msg: "required"}
	}
	if c.adapters == nil {
		return nil, fmt.Errorf("no engine adapters configured")
	}

	engine := adapter.InferEngine(req.WorkflowType, req.WorkflowEngine)
	runAdapter, ok := c.adapters.Get(engine)
	if !ok {
		return nil, &adapter.ValidationError{Field: "workflow_engine", Msg: fmt.Sprintf("unsupported engine %q", engine)}
	}

	runID := domain.NewID()
	submission := adapter.SubmissionRequest{
		WorkflowType:             req.WorkflowType,
		WorkflowTypeVersion:      req.WorkflowTypeVersion,
		WorkflowURL:              req.WorkflowURL,
		WorkflowParams:           req.WorkflowParams,
		WorkflowEngine:           engine,
		WorkflowEngineVersion:    req.WorkflowEngineVersion,
		WorkflowEngineParameters: req.WorkflowEngineParameters,
		Tags:                     req.Tags,
	}
	// Preparing a run only validates and builds the runner's command, so an
	// invalid request fails here, before the run gets a bucket.
	layout := c.layout
	prepared, err := runAdapter.PrepareRun(runID, submission, layout)
	if err != nil {
		return nil, err
	}

	// Until the run is stored, a failure (Ensure's included: it may have made
	// half a bucket) leaves nothing that refers to the run's bucket: delete
	// it again. A failed Create keeps the bucket: the run may have been
	// stored before the error.
	keepBucket := false
	defer func() {
		if !keepBucket {
			c.deleteRunBucket(runID)
		}
	}()
	runLayout, storageSecret, err := c.ensureRunBucket(ctx, runID)
	if err != nil {
		return nil, err
	}
	if runLayout != layout {
		layout = runLayout
		if prepared, err = runAdapter.PrepareRun(runID, submission, layout); err != nil {
			return nil, err
		}
	}
	prepared.RunnerSpec.StorageSecretName = storageSecret

	run := domain.NewRunWithID(
		runID,
		req.WorkflowType,
		req.WorkflowTypeVersion,
		prepared.WorkflowEngine,
		prepared.WorkflowEngineVersion,
		prepared.RequestPayload,
		"", // set below
		layout.CurrentVersion(),
		"", // set below
	)
	run.OutputPrefix = layout.OutputsDirURI(run.RunID)
	run.RunRootURI = layout.RunRootURI(run.RunID)
	run.ResolvedWorkflowRef = prepared.ResolvedWorkflowRef

	keepBucket = true
	if err := c.runs.Create(ctx, run); err != nil {
		if layout.Bucket != c.layout.Bucket {
			c.log.Warn("run may not have been stored; its bucket is kept", "run_id", runID, "bucket", layout.Bucket)
		}
		return nil, fmt.Errorf("create run: %w", err)
	}

	event := domain.NewEvent(run.RunID, "", domain.EventRunCreated, prepared.RequestPayload)
	if err := c.events.Append(ctx, event); err != nil {
		c.log.Error("failed to append run.created event", "run_id", run.RunID, "err", err)
	}

	// Launch the runner Job if a run backend is configured.
	if c.runBackend != nil {
		submission, err := c.runBackend.SubmitRun(ctx, run, &prepared.RunnerSpec)
		if err != nil {
			// Run was persisted but submission failed. Mark as SYSTEM_ERROR.
			if setErr := c.runs.SetError(ctx, run.RunID, domain.StateSystemError, "runner submission failed: "+err.Error()); setErr != nil {
				c.log.Error("failed to persist run submission error", "run_id", run.RunID, "err", setErr)
			}
			run.ErrorSummary = "runner submission failed: " + err.Error()
			run.State = domain.StateSystemError
			c.log.Error("runner submission failed", "run_id", run.RunID, "err", err)
			return run, nil // return the run even on submission failure
		}
		if submission == nil || submission.BackendRef == "" {
			if setErr := c.runs.SetError(ctx, run.RunID, domain.StateSystemError, "runner submission returned no backend reference"); setErr != nil {
				c.log.Error("failed to persist missing backend ref error", "run_id", run.RunID, "err", setErr)
			}
			run.ErrorSummary = "runner submission returned no backend reference"
			run.State = domain.StateSystemError
			return run, nil
		}
		// Persist workspace binding if the backend provisioned one.
		if submission.Workspace != nil {
			if err := c.runs.SetWorkspaceBinding(ctx, run.RunID, submission.Workspace); err != nil {
				c.log.Error("failed to persist workspace binding", "run_id", run.RunID, "err", err)
				// Non-fatal: the run can still proceed, but cleanup may be impaired.
			}
			run.Workspace = submission.Workspace
		}
		if err := c.persistSubmittedRun(ctx, run, submission.BackendRef); err != nil {
			c.log.Error("failed to persist submitted run state", "run_id", run.RunID, "backend_ref", submission.BackendRef, "err", err)
			if cancelErr := c.runBackend.CancelRun(context.Background(), &domain.Run{RunID: run.RunID, BackendRef: submission.BackendRef}); cancelErr != nil {
				c.log.Error("failed to clean up runner after persistence error", "run_id", run.RunID, "backend_ref", submission.BackendRef, "err", cancelErr)
			}
			if setErr := c.runs.SetError(ctx, run.RunID, domain.StateSystemError, "failed to persist runner submission: "+err.Error()); setErr != nil {
				c.log.Error("failed to persist runner submission persistence error", "run_id", run.RunID, "err", setErr)
			}
			run.BackendRef = submission.BackendRef
			run.ErrorSummary = "failed to persist runner submission: " + err.Error()
			run.State = domain.StateSystemError
			return run, nil
		}
	}

	c.log.Info("run submitted",
		"run_id", run.RunID,
		"engine", prepared.WorkflowEngine,
		"workflow_url", req.WorkflowURL,
		"resolved_ref", prepared.ResolvedWorkflowRef,
		"backend_ref", run.BackendRef,
	)
	return run, nil
}

// ensureRunBucket returns the layout of a new run's objects and the Secret
// with its runner's S3 credentials ("" for the run backend's own).
func (c *RunController) ensureRunBucket(ctx context.Context, runID string) (storage.Layout, string, error) {
	layout := c.layout
	if c.runBuckets == nil {
		return layout, "", nil
	}
	bucket, err := c.runBuckets.Ensure(ctx, runID)
	if err != nil {
		return storage.Layout{}, "", fmt.Errorf("provision run bucket: %w", err)
	}
	// Fail closed: without a Secret, the runner would get the bucket-wide one.
	if bucket.Name == "" || bucket.SecretName == "" {
		return storage.Layout{}, "", fmt.Errorf("provision run bucket: provisioner returned no bucket or no Secret for run %s", runID)
	}
	layout.Bucket = bucket.Name
	return layout, bucket.SecretName, nil
}

// deleteRunBucket deletes the bucket of a run that was not created, best
// effort and on its own context: the failed request's may be done already.
func (c *RunController) deleteRunBucket(runID string) {
	if c.runBuckets == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), runBucketCleanupTimeout)
	defer cancel()
	if err := c.runBuckets.Delete(ctx, runID); err != nil {
		c.log.Error("failed to delete the bucket of a run that was not created", "run_id", runID, "err", err)
	}
}

// runBucketCleanupTimeout bounds deleteRunBucket.
const runBucketCleanupTimeout = 30 * time.Second

// GetRun returns the full run and a task-logs URL.
func (c *RunController) GetRun(ctx context.Context, runID string) (*RunLog, error) {
	run, err := c.runs.Get(ctx, runID)
	if err != nil {
		return nil, err
	}

	var outputs json.RawMessage
	if (c.artifacts != nil || c.runObjects != nil) && run.WorkflowEngine == "wdl" {
		outputs, err = c.loadWorkflowOutputs(ctx, run)
		if err != nil {
			c.log.Warn("failed to load workflow outputs artifact", "run_id", run.RunID, "err", err)
		}
	}

	return &RunLog{
		Run:         run,
		TaskLogsURL: fmt.Sprintf("/ga4gh/wes/v1/runs/%s/tasks", run.RunID),
		Outputs:     outputs,
	}, nil
}

// GetRunStatus returns the state of a run.
func (c *RunController) GetRunStatus(ctx context.Context, runID string) (domain.State, error) {
	return c.runs.GetStatus(ctx, runID)
}

// ListRuns returns a paginated list of runs.
func (c *RunController) ListRuns(ctx context.Context, pageSize int, pageToken string) (*RunListResult, error) {
	result, err := c.runs.List(ctx, repository.Page{Size: pageSize, Token: pageToken})
	if err != nil {
		return nil, err
	}
	return &RunListResult{Runs: result.Items, NextToken: result.NextToken}, nil
}

// CancelRun transitions the run to CANCELING and stops the runner/tasks.
func (c *RunController) CancelRun(ctx context.Context, runID string) error {
	run, err := c.runs.Get(ctx, runID)
	if err != nil {
		return err
	}

	if err := domain.ValidateRunTransition(run.State, domain.StateCanceling); err != nil {
		return err
	}

	if err := c.runs.UpdateState(ctx, runID, run.State, domain.StateCanceling); err != nil {
		return err
	}

	payload, _ := json.Marshal(domain.StateChangePayload{
		FromState: run.State,
		ToState:   domain.StateCanceling,
		Reason:    "user requested cancellation",
	})
	event := domain.NewEvent(runID, "", domain.EventRunCancelRequested, payload)
	if err := c.events.Append(ctx, event); err != nil {
		c.log.Error("failed to append cancel event", "run_id", runID, "err", err)
	}

	// Cancel the runner Job asynchronously.
	if c.runBackend != nil {
		go func() {
			if err := c.runBackend.CancelRun(context.Background(), run); err != nil {
				c.log.Error("failed to cancel runner", "run_id", runID, "err", err)
			}
		}()
	}

	// Cancel active tasks for this run asynchronously.
	if c.taskBackend != nil {
		go func() {
			tasks, err := c.tasks.ListByRunID(context.Background(), runID, repository.Page{Size: 1000})
			if err != nil {
				c.log.Error("failed to list tasks for cancellation", "run_id", runID, "err", err)
				return
			}
			for _, task := range tasks.Items {
				if task.State.IsTerminal() {
					continue
				}
				if err := c.taskBackend.Cancel(context.Background(), task); err != nil {
					c.log.Error("failed to cancel task backend", "run_id", runID, "task_id", task.TaskID, "err", err)
					continue
				}
				if err := c.tasks.UpdateState(context.Background(), task.TaskID, task.State, domain.StateCanceled); err != nil {
					c.log.Error("failed to mark task canceled", "run_id", runID, "task_id", task.TaskID, "err", err)
				}
			}
		}()
	}

	c.log.Info("run cancel requested", "run_id", runID, "from_state", run.State)
	return nil
}

// ListRunTasks returns paginated tasks for a run.
func (c *RunController) ListRunTasks(ctx context.Context, runID string, pageSize int, pageToken string) (*TaskListResult, error) {
	if _, err := c.runs.GetStatus(ctx, runID); err != nil {
		return nil, err
	}
	result, err := c.tasks.ListByRunID(ctx, runID, repository.Page{Size: pageSize, Token: pageToken})
	if err != nil {
		return nil, err
	}
	return &TaskListResult{Tasks: result.Items, NextToken: result.NextToken}, nil
}

// GetRunTask returns a single task for a run.
func (c *RunController) GetRunTask(ctx context.Context, runID, taskID string) (*domain.Task, error) {
	task, err := c.tasks.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.RunID != runID {
		return nil, fmt.Errorf("task %s does not belong to run %s", taskID, runID)
	}
	return task, nil
}

// ServiceStateCounts returns aggregate run counts by state.
func (c *RunController) ServiceStateCounts(ctx context.Context) (StateCounts, error) {
	return c.runs.CountByState(ctx)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// TimeStr formats a *time.Time to RFC3339 or returns empty string.
func TimeStr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (c *RunController) persistSubmittedRun(ctx context.Context, run *domain.Run, backendRef string) error {
	if err := c.runs.SetBackendRef(ctx, run.RunID, backendRef); err != nil {
		return fmt.Errorf("set backend ref: %w", err)
	}
	if err := c.runs.UpdateState(ctx, run.RunID, domain.StateQueued, domain.StateInitializing); err != nil {
		return fmt.Errorf("set initializing state: %w", err)
	}
	run.BackendRef = backendRef
	run.State = domain.StateInitializing
	return nil
}

func (c *RunController) loadWorkflowOutputs(ctx context.Context, run *domain.Run) (json.RawMessage, error) {
	layout := c.LayoutOf(run)
	var store objstore.Client = c.artifacts
	if c.runObjects != nil {
		bucket, client, err := c.runObjects.Client(ctx, run.RunID)
		if err != nil {
			return nil, err
		}
		layout.Bucket, store = bucket, client
	}
	reader, err := store.Get(ctx, layout.Bucket, layout.WorkflowOutputs(run.RunID))
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read workflow outputs: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var payload json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse workflow outputs JSON: %w", err)
	}
	return payload, nil
}
