package runbucket

import (
	"context"
	"testing"
)

func TestShared_EnsureReturnsTheSharedBucketForEveryRun(t *testing.T) {
	p, err := NewShared("conquest-workflows", "storage-secrets")
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}
	want := Bucket{Name: "conquest-workflows", SecretName: "storage-secrets"}
	for _, runID := range []string{"run-a", "run-b"} {
		got, err := p.Ensure(context.Background(), runID)
		if err != nil {
			t.Fatalf("Ensure(%s): %v", runID, err)
		}
		if got != want {
			t.Fatalf("Ensure(%s) = %+v, want %+v", runID, got, want)
		}
	}
}

func TestShared_EnsureRefusesAnEmptyRunID(t *testing.T) {
	p, err := NewShared("conquest-workflows", "storage-secrets")
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}
	if _, err := p.Ensure(context.Background(), ""); err == nil {
		t.Fatal("Ensure with an empty run ID must fail")
	}
}

func TestNewShared_RequiresBucketAndSecret(t *testing.T) {
	if _, err := NewShared("", "storage-secrets"); err == nil {
		t.Fatal("NewShared without a bucket must fail")
	}
	if _, err := NewShared("conquest-workflows", ""); err == nil {
		t.Fatal("NewShared without a Secret must fail")
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeShared, false},
		{"shared", ModeShared, false},
		{" shared ", ModeShared, false},
		{"Shared", "", true},
		{"obc", ModeOBC, false},
		{" obc ", ModeOBC, false},
		{"OBC", "", true},
		{"per-run", "", true},
	} {
		got, err := ParseMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("ParseMode(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Fatalf("ParseMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
