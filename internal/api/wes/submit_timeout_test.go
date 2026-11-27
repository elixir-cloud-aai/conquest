package wes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anuragxxd/conquest/internal/adapter"
	"github.com/anuragxxd/conquest/internal/controller"
	"github.com/anuragxxd/conquest/internal/runbucket"
	"github.com/anuragxxd/conquest/internal/storage"
)

// preparingAdapter prepares every run.
type preparingAdapter struct{}

func (preparingAdapter) Name() string { return "nextflow" }

func (preparingAdapter) PrepareRun(string, adapter.SubmissionRequest, storage.Layout) (*adapter.PreparedRun, error) {
	return &adapter.PreparedRun{WorkflowEngine: "nextflow", RequestPayload: json.RawMessage(`{}`)}, nil
}

// deadlineProvisioner records the deadline Ensure gets and fails.
type deadlineProvisioner struct {
	deadline time.Time
	has      bool
}

func (p *deadlineProvisioner) Ensure(ctx context.Context, _ string) (runbucket.Bucket, error) {
	p.deadline, p.has = ctx.Deadline()
	return runbucket.Bucket{}, errors.New("stop here")
}

func (p *deadlineProvisioner) Credentials(context.Context, string) (runbucket.Credentials, error) {
	return runbucket.Credentials{}, errors.New("not used")
}

func (p *deadlineProvisioner) Delete(context.Context, string) error { return nil }

func submitWithProvisioner(t *testing.T, timeout time.Duration) *deadlineProvisioner {
	t.Helper()
	ctrl := controller.NewRunController(&wesRunStoreStub{}, &wesTaskStoreStub{}, &wesEventStoreStub{},
		storage.Layout{Bucket: "conquest", Version: "v1"}, adapter.NewRegistry(preparingAdapter{}),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	prov := &deadlineProvisioner{}
	ctrl.SetRunBuckets(prov)
	h := NewHandler(ctrl)
	h.SetSubmitTimeout(timeout)
	req := httptest.NewRequest(http.MethodPost, "/ga4gh/wes/v1/runs", strings.NewReader(
		`{"workflow_type":"NFL","workflow_type_version":"DSL2","workflow_url":"https://example.org/wf"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.SubmitRun(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d %s, want 500 from the stub provisioner", rec.Code, rec.Body)
	}
	return prov
}

// A submission must finish before the server's write deadline, so the
// client learns its run ID: the handler bounds it, and the provisioner sees
// that bound.
func TestSubmitRun_IsBoundedBySubmitTimeout(t *testing.T) {
	start := time.Now()
	prov := submitWithProvisioner(t, 10*time.Second)
	if !prov.has {
		t.Fatal("submission has no deadline")
	}
	if d := prov.deadline.Sub(start); d < 9*time.Second || d > 11*time.Second {
		t.Fatalf("deadline %v after the request, want about 10s", d)
	}

	if prov := submitWithProvisioner(t, 0); prov.has {
		t.Fatal("submission got a deadline without a submit timeout")
	}
}
