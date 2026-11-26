package adapter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anuragxxd/conquest/internal/storage"
)

// The runner works in the bucket of the layout it is given, the run's own
// (internal/runbucket), for every engine.
func TestPrepareRun_RunnerUsesTheLayoutsBucket(t *testing.T) {
	layout := storage.Layout{Bucket: "conquest-run-123", Version: "v1"}
	cases := []struct {
		adapter RunAdapter
		req     SubmissionRequest
	}{
		{NewNextflowRunAdapter(NextflowRunAdapterConfig{
			DefaultRunnerImage: "example.com/conquest-nf-24.10.4-runner:latest",
			S3Endpoint:         "http://s3:9000",
			S3Region:           "us-east-1",
		}), SubmissionRequest{
			WorkflowType:             "NFL",
			WorkflowTypeVersion:      "DSL2",
			WorkflowURL:              "https://github.com/nf-core/rnaseq.git",
			WorkflowEngineParameters: json.RawMessage(`{"revision": "13328f73915ceda9bb794caee2f662230c5a33b6"}`),
		}},
		{NewWDLRunAdapter(WDLRunAdapterConfig{
			DefaultRunnerImage: "example.com/conquest-wdl-runner:latest",
			S3Endpoint:         "http://s3:9000",
			S3Region:           "us-east-1",
		}), SubmissionRequest{
			WorkflowType:             "WDL",
			WorkflowTypeVersion:      "1.1",
			WorkflowURL:              "https://github.com/acme-org/my-wdl-pipelines.git",
			WorkflowEngineParameters: json.RawMessage(`{"revision": "13328f73915ceda9bb794caee2f662230c5a33b6", "main_workflow_path": "main.wdl"}`),
		}},
		{NewSnakemakeRunAdapter(SnakemakeRunAdapterConfig{
			DefaultRunnerImage: "example.com/conquest-smk-9.17.3-runner:latest",
			DefaultTaskImage:   "example.com/conquest-snakemake-task:latest",
			DefaultJobs:        1,
			DefaultDiskMB:      1024,
			S3Endpoint:         "http://s3:9000",
			S3Region:           "us-east-1",
		}), SubmissionRequest{
			WorkflowType:             "SMK",
			WorkflowTypeVersion:      "9.17.3",
			WorkflowURL:              "https://github.com/snakemake-workflows/dna-seq-gatk-variant-calling",
			WorkflowEngineParameters: json.RawMessage(`{"revision": "13328f73915ceda9bb794caee2f662230c5a33b6"}`),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.adapter.Name(), func(t *testing.T) {
			prepared, err := tc.adapter.PrepareRun("run-123", tc.req, layout)
			if err != nil {
				t.Fatalf("PrepareRun: %v", err)
			}
			args := prepared.RunnerSpec.Args
			bucket := ""
			for i, arg := range args {
				if arg == "--s3-bucket" && i+1 < len(args) {
					bucket = args[i+1]
				}
				if strings.HasPrefix(arg, "s3://") && !strings.HasPrefix(arg, "s3://"+layout.Bucket+"/") {
					t.Errorf("runner arg %q is outside the run's bucket", arg)
				}
			}
			if bucket != layout.Bucket {
				t.Fatalf("--s3-bucket = %q, want %q", bucket, layout.Bucket)
			}
		})
	}
}
