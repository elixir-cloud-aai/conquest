package wes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/anuragxxd/conquest/internal/adapter"
	"github.com/anuragxxd/conquest/internal/controller"
	"github.com/anuragxxd/conquest/internal/domain"
	nfengine "github.com/anuragxxd/conquest/internal/engine/nextflow"
	smkengine "github.com/anuragxxd/conquest/internal/engine/snakemake"
	wdlengine "github.com/anuragxxd/conquest/internal/engine/wdl"
	"github.com/anuragxxd/conquest/internal/repository/pg"
	"github.com/anuragxxd/conquest/internal/storage"
)

// Handler holds the WES HTTP handlers.
type Handler struct {
	ctrl *controller.RunController
	// base holds the GA4GH service-info base fields; zero: none.
	base ServiceInfoBase
	// submitTimeout bounds a run submission from the handler's start; zero:
	// only the request's context.
	submitTimeout time.Duration
}

// SetSubmitTimeout bounds each run submission, body included, to d from the
// handler's start. Keep it below the server's write timeout: a submission
// that outlasts that stores a run whose ID the client never gets. Run
// buckets that take time to make (obc) stop waiting in time for the rest.
func (h *Handler) SetSubmitTimeout(d time.Duration) {
	h.submitTimeout = d
}

// SetServiceInfo makes service-info answer with the GA4GH base fields b,
// with type org.ga4gh:wes:1.1.0; an empty b.ID leaves them out.
func (h *Handler) SetServiceInfo(b ServiceInfoBase) {
	if b.ID == "" {
		h.base = ServiceInfoBase{}
		return
	}
	b.Type = &ServiceType{Group: "org.ga4gh", Artifact: "wes", Version: "1.1.0"}
	h.base = b
}

// NewHandler creates WES handlers.
func NewHandler(ctrl *controller.RunController) *Handler {
	return &Handler{ctrl: ctrl}
}

// Routes registers WES routes on the given chi router.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/ga4gh/wes/v1/service-info", h.GetServiceInfo)
	r.Post("/ga4gh/wes/v1/runs", h.SubmitRun)
	r.Get("/ga4gh/wes/v1/runs", h.ListRuns)
	r.Get("/ga4gh/wes/v1/runs/{run_id}", h.GetRun)
	r.Get("/ga4gh/wes/v1/runs/{run_id}/status", h.GetRunStatus)
	r.Post("/ga4gh/wes/v1/runs/{run_id}/cancel", h.CancelRun)
	r.Get("/ga4gh/wes/v1/runs/{run_id}/tasks", h.ListRunTasks)
	r.Get("/ga4gh/wes/v1/runs/{run_id}/tasks/{task_id}", h.GetRunTask)
}

func (h *Handler) GetServiceInfo(w http.ResponseWriter, r *http.Request) {
	counts, err := h.ctrl.ServiceStateCounts(r.Context())
	if err != nil {
		counts = make(controller.StateCounts)
	}

	stateCounts := make(map[string]int, len(counts))
	for s, n := range counts {
		stateCounts[string(s)] = n
	}

	workflowTypes := map[string]TypeVersions{}
	engineVersions := map[string]string{}
	tags := map[string]string{}
	for _, engine := range h.ctrl.SupportedEngines() {
		switch engine {
		case "nextflow":
			workflowTypes["NFL"] = TypeVersions{WorkflowTypeVersion: []string{"DSL2"}}
			engineVersions[engine] = nfengine.DefaultVersion()
			tags["nextflow_supported_versions"] = strings.Join(nfengine.SupportedVersions(), ",")
		case "snakemake":
			workflowTypes["SMK"] = TypeVersions{WorkflowTypeVersion: smkengine.SupportedVersions()}
			engineVersions[engine] = smkengine.DefaultVersion()
			tags["snakemake_supported_versions"] = strings.Join(smkengine.SupportedVersions(), ",")
		case "wdl":
			workflowTypes["WDL"] = TypeVersions{WorkflowTypeVersion: wdlengine.SupportedWDLVersions()}
			engineVersions[engine] = wdlengine.DefaultVersion()
			tags["wdl_supported_versions"] = strings.Join(wdlengine.SupportedVersions(), ",")
		}
	}

	info := ServiceInfo{ServiceInfoBase: h.base}
	info.WorkflowTypeVersions = workflowTypes
	info.SupportedWESVersions = []string{"1.1.0"}
	info.SupportedFilesystemProtocols = []string{"s3", "https"}
	info.WorkflowEngineVersions = engineVersions
	info.SystemStateCounts = stateCounts
	info.Tags = tags
	writeJSON(w, http.StatusOK, info)
}

func (h *Handler) SubmitRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.submitTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.submitTimeout)
		defer cancel()
	}
	req, err := parseRunRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Map WES request to controller request.
	ctrlReq := &controller.RunRequest{
		WorkflowType:             req.WorkflowType,
		WorkflowTypeVersion:      req.WorkflowTypeVersion,
		WorkflowURL:              req.WorkflowURL,
		WorkflowParams:           req.WorkflowParams,
		WorkflowEngine:           req.WorkflowEngine,
		WorkflowEngineVersion:    req.WorkflowEngineVersion,
		WorkflowEngineParameters: req.WorkflowEngineParameters,
		Tags:                     req.Tags,
	}

	run, err := h.ctrl.SubmitRun(ctx, ctrlReq)
	if err != nil {
		var ve *adapter.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, RunID{RunID: run.RunID})
}

func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	result, err := h.ctrl.GetRun(r.Context(), runID)
	if err != nil {
		handleGetError(w, err)
		return
	}

	run := result.Run
	var req RunRequest
	_ = json.Unmarshal(run.RequestPayload, &req)

	layout := h.ctrl.LayoutOf(run)
	resp := RunLog{
		RunID:       run.RunID,
		Request:     &req,
		State:       run.State,
		TaskLogsURL: result.TaskLogsURL,
		Outputs:     result.Outputs,
		RunLog: &Log{
			StartTime: controller.TimeStr(run.StartedAt),
			EndTime:   controller.TimeStr(run.FinishedAt),
			Stdout:    layout.RunLogURI(run.RunID),
		},
	}
	if run.DataDeletedAt != nil {
		resp.RunLog.Stdout = ""
		resp.RunLog.SystemLogs = dataDeletedLogs(*run.DataDeletedAt)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) GetRunStatus(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	state, err := h.ctrl.GetRunStatus(r.Context(), runID)
	if err != nil {
		handleGetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, RunStatus{RunID: runID, State: state})
}

func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	pageSize := queryInt(r, "page_size", 50)
	pageToken := r.URL.Query().Get("page_token")

	result, err := h.ctrl.ListRuns(r.Context(), pageSize, pageToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	summaries := make([]RunSummary, 0, len(result.Runs))
	for _, run := range result.Runs {
		var tags map[string]string
		var req RunRequest
		if json.Unmarshal(run.RequestPayload, &req) == nil {
			tags = req.Tags
		}
		summaries = append(summaries, RunSummary{
			RunID:     run.RunID,
			State:     run.State,
			StartTime: run.StartedAt,
			EndTime:   run.FinishedAt,
			Tags:      tags,
		})
	}

	writeJSON(w, http.StatusOK, RunListResponse{
		Runs:          summaries,
		NextPageToken: result.NextToken,
	})
}

func (h *Handler) CancelRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	err := h.ctrl.CancelRun(r.Context(), runID)
	if err != nil {
		var te *domain.TransitionError
		if errors.As(err, &te) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		handleGetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, RunID{RunID: runID})
}

func (h *Handler) ListRunTasks(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	pageSize := queryInt(r, "page_size", 50)
	pageToken := r.URL.Query().Get("page_token")

	result, err := h.ctrl.ListRunTasks(r.Context(), runID, pageSize, pageToken)
	if err != nil {
		handleGetError(w, err)
		return
	}

	layout, deleted, err := h.runLogs(r.Context(), runID)
	if err != nil {
		handleGetError(w, err)
		return
	}
	logs := make([]TaskLog, 0, len(result.Tasks))
	for _, t := range result.Tasks {
		logs = append(logs, taskLog(t, layout, deleted))
	}

	writeJSON(w, http.StatusOK, TaskListResponse{
		TaskLogs:      logs,
		NextPageToken: result.NextToken,
	})
}

func (h *Handler) GetRunTask(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	taskID := chi.URLParam(r, "task_id")

	task, err := h.ctrl.GetRunTask(r.Context(), runID, taskID)
	if err != nil {
		handleGetError(w, err)
		return
	}

	layout, deleted, err := h.runLogs(r.Context(), runID)
	if err != nil {
		handleGetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskLog(task, layout, deleted))
}

// taskLog returns the WES log of a task of a run with the given layout,
// whose data was deleted at deleted (nil: it exists).
func taskLog(t *domain.Task, layout storage.Layout, deleted *time.Time) TaskLog {
	l := TaskLog{
		ID:        t.TaskID,
		Name:      t.EngineTaskName,
		StartTime: controller.TimeStr(t.StartedAt),
		EndTime:   controller.TimeStr(t.FinishedAt),
		ExitCode:  t.ExitCode,
		Stdout:    layout.TaskLogURI(t.RunID, t.TaskID, int(t.Attempt), 0),
		TesURI:    fmt.Sprintf("/ga4gh/tes/v1/tasks/%s", t.TESTaskID),
	}
	if deleted != nil {
		l.Stdout = ""
		l.SystemLogs = dataDeletedLogs(*deleted)
	}
	return l
}

// dataDeletedLogs are the system logs of a run, or a task of a run, whose
// data was deleted at deleted after the retention period: its log URIs
// would name deleted objects.
func dataDeletedLogs(deleted time.Time) []string {
	return []string{fmt.Sprintf("run data deleted at %s: the retention period of finished runs passed; logs and outputs are gone",
		deleted.UTC().Format(time.RFC3339))}
}

// ---------------------------------------------------------------------------
// Multipart parsing
// ---------------------------------------------------------------------------

func parseRunRequest(r *http.Request) (*RunRequest, error) {
	contentType := r.Header.Get("Content-Type")

	if isJSON(contentType) {
		var req RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		return &req, nil
	}

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, err
	}

	req := &RunRequest{
		WorkflowType:          r.FormValue("workflow_type"),
		WorkflowTypeVersion:   r.FormValue("workflow_type_version"),
		WorkflowURL:           r.FormValue("workflow_url"),
		WorkflowEngine:        r.FormValue("workflow_engine"),
		WorkflowEngineVersion: r.FormValue("workflow_engine_version"),
	}

	if v := r.FormValue("workflow_params"); v != "" {
		req.WorkflowParams = json.RawMessage(v)
	}
	if v := r.FormValue("workflow_engine_parameters"); v != "" {
		req.WorkflowEngineParameters = json.RawMessage(v)
	}
	if v := r.FormValue("tags"); v != "" {
		var tags map[string]string
		if err := json.Unmarshal([]byte(v), &tags); err == nil {
			req.Tags = tags
		}
	}

	return req, nil
}

func isJSON(contentType string) bool {
	return len(contentType) >= 16 && contentType[:16] == "application/json"
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// runLogs returns the layout of the run's objects, in the run's bucket,
// and when the run's data was deleted (nil: it exists); the server's layout
// and nil if there is no such run. Any other error is returned: the run's
// data may be gone, so its log URIs can't be given.
func (h *Handler) runLogs(ctx context.Context, runID string) (storage.Layout, *time.Time, error) {
	run, err := h.ctrl.Run(ctx, runID)
	if errors.Is(err, pg.ErrNotFound) || (err == nil && run == nil) {
		return h.ctrl.Layout(), nil, nil
	}
	if err != nil {
		return storage.Layout{}, nil, err
	}
	return h.ctrl.LayoutOf(run), run.DataDeletedAt, nil
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Msg: msg, StatusCode: status})
}

func handleGetError(w http.ResponseWriter, err error) {
	if errors.Is(err, pg.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func queryInt(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
