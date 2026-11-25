package filer

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anuragxxd/conquest/internal/api/tes"
	"github.com/anuragxxd/conquest/internal/isolation"
)

// With a task storage URL and no S3 settings, stage-out goes through the
// storage gateway.
func TestExecAndStageOut_UsesTaskStorageURL(t *testing.T) {
	var mu sync.Mutex
	stored := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if obj, ok := strings.CutPrefix(r.URL.Path, "/storage/v1/t/t1/sig/"); ok {
			w.Header().Set("Location", "/s3/"+obj)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		stored[strings.TrimPrefix(r.URL.Path, "/s3/")] = string(data)
		mu.Unlock()
	}))
	defer srv.Close()
	t.Setenv(isolation.EnvTaskStorageURL, srv.URL+"/storage/v1/t/t1/sig")
	for _, k := range []string{"CONQUEST_S3_ENDPOINT", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		t.Setenv(k, "")
	}

	workdir := t.TempDir()
	out := filepath.Join(workdir, "out.txt")
	manifest, _ := json.Marshal(StageManifest{
		Workdir: workdir,
		Outputs: []tes.Output{{URL: "s3://conquest/run/out.txt", Path: out}},
	})
	code := ExecAndStageOut(t.Context(), string(manifest), []string{"sh", "-c", "printf hi > " + out}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := stored["conquest/run/out.txt"]; got != "hi" {
		t.Errorf("uploaded = %v", stored)
	}
	_ = os.Remove(out)
}
