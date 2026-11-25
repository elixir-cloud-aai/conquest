package wes

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anuragxxd/conquest/internal/adapter"
	"github.com/anuragxxd/conquest/internal/controller"
	"github.com/anuragxxd/conquest/internal/storage"
)

func serviceInfo(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	resp := httptest.NewRecorder()
	h.GetServiceInfo(resp, httptest.NewRequest(http.MethodGet, "/ga4gh/wes/v1/service-info", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	var info map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	return info
}

func newServiceInfoHandler() *Handler {
	return NewHandler(controller.NewRunController(
		&wesRunStoreStub{}, &wesTaskStoreStub{}, &wesEventStoreStub{},
		storage.Layout{Bucket: "conquest", Version: "v1"},
		adapter.NewRegistry(&wesAdapterStub{name: "nextflow"}),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	))
}

func TestGetServiceInfo_BaseFields(t *testing.T) {
	h := newServiceInfoHandler()
	h.SetServiceInfo(ServiceInfoBase{
		ID:               "org.example.conquest",
		Name:             "Conquest",
		Description:      "Workflows",
		Organization:     &Organization{Name: "Example", URL: "https://example.org"},
		ContactURL:       "mailto:ops@example.org",
		DocumentationURL: "https://docs.example.org",
		Environment:      "prod",
		Version:          "1.2.3",
	})
	info := serviceInfo(t, h)
	want := map[string]any{
		"id": "org.example.conquest", "name": "Conquest", "description": "Workflows",
		"contactUrl": "mailto:ops@example.org", "documentationUrl": "https://docs.example.org",
		"environment": "prod", "version": "1.2.3",
	}
	for k, v := range want {
		if info[k] != v {
			t.Errorf("%s = %v, want %v", k, info[k], v)
		}
	}
	typ, _ := info["type"].(map[string]any)
	if typ["group"] != "org.ga4gh" || typ["artifact"] != "wes" || typ["version"] != "1.1.0" {
		t.Errorf("type = %v", info["type"])
	}
	org, _ := info["organization"].(map[string]any)
	if org["name"] != "Example" || org["url"] != "https://example.org" {
		t.Errorf("organization = %v", info["organization"])
	}
	// the WES fields stay
	if _, ok := info["workflow_type_versions"].(map[string]any)["NFL"]; !ok {
		t.Errorf("workflow_type_versions = %v", info["workflow_type_versions"])
	}
}

func TestGetServiceInfo_NoBaseFieldsByDefault(t *testing.T) {
	h := newServiceInfoHandler()
	h.SetServiceInfo(ServiceInfoBase{Name: "ignored without an ID"})
	info := serviceInfo(t, h)
	for _, k := range []string{"id", "name", "type", "organization", "version", "contactUrl", "documentationUrl", "environment", "description"} {
		if _, ok := info[k]; ok {
			t.Errorf("%s present: %v", k, info[k])
		}
	}
}
