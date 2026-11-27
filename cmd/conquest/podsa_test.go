package main

import (
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anuragxxd/conquest/internal/config"
	"github.com/anuragxxd/conquest/internal/runbucket"
)

func TestEnforcePodServiceAccount(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, testToken(`{"sub":"system:serviceaccount:conquest:conquest"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		mode    runbucket.Mode
		podSA   string
		wantErr bool
	}{
		{runbucket.ModeOBC, "conquest-pods", false},
		{runbucket.ModeOBC, "conquest", true},
		{runbucket.ModeShared, "conquest-pods", false},
		{runbucket.ModeShared, "conquest", false}, // warns only
	} {
		cfg := &config.Config{RunBuckets: tc.mode, K8sNamespace: "conquest", ServiceAccount: tc.podSA}
		if err := enforcePodServiceAccount(cfg, tokenFile, log); (err != nil) != tc.wantErr {
			t.Errorf("%s with pods as %s: err = %v, wantErr %v", tc.mode, tc.podSA, err, tc.wantErr)
		}
	}
}

// A run submission waits for its claim within the server's answer.
func TestSubmitTimeoutFitsTheWriteTimeout(t *testing.T) {
	if submitTimeout >= serverWriteTimeout {
		t.Fatalf("submitTimeout %s must stay below the server's write timeout %s", submitTimeout, serverWriteTimeout)
	}
	if submitTimeout-runbucket.DefaultOBCReserve < 10*time.Second {
		t.Fatalf("submitTimeout %s leaves an obc claim less than 10s to bind", submitTimeout)
	}
	if runbucket.MaxOBCBindTimeout > submitTimeout {
		t.Fatalf("MaxOBCBindTimeout %s above submitTimeout %s", runbucket.MaxOBCBindTimeout, submitTimeout)
	}
}

// testToken returns an unsigned JWT with the claims payload.
func testToken(payload string) []byte {
	enc := base64.RawURLEncoding.EncodeToString
	return []byte(enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(payload)) + ".c2ln")
}

func TestServiceAccountFromToken(t *testing.T) {
	ns, name, err := serviceAccountFromToken(testToken(`{"sub":"system:serviceaccount:workflows:conquest"}`))
	if err != nil || ns != "workflows" || name != "conquest" {
		t.Fatalf("serviceAccountFromToken = %q, %q, %v", ns, name, err)
	}
	for _, tok := range [][]byte{
		nil,
		[]byte("not-a-jwt"),
		[]byte("a.!!!.c"),
		testToken(`not json`),
		testToken(`{"sub":"user:alice"}`),
		testToken(`{"sub":"system:serviceaccount:only-namespace"}`),
		testToken(`{"sub":"system:serviceaccount::name"}`),
		testToken(`{"sub":"system:serviceaccount:namespace:"}`),
		testToken(`{}`),
	} {
		if ns, name, err := serviceAccountFromToken(tok); err == nil {
			t.Errorf("serviceAccountFromToken(%q) = %q, %q; want an error", tok, ns, name)
		}
	}
}

func TestCheckPodServiceAccount(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		podNS, podSA, srvNS, srvSA string
		wantErr                    bool
	}{
		{"own account", "conquest", "conquest-pods", "conquest", "conquest", false},
		{"server's account", "conquest", "conquest", "conquest", "conquest", true},
		{"server's account, padded", "conquest", " conquest ", "conquest", "conquest", true},
		{"namespace default is the server's", "conquest", "", "conquest", "default", true},
		{"same name elsewhere", "jobs", "conquest", "conquest", "conquest", false},
		{"default account elsewhere", "jobs", "", "conquest", "default", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPodServiceAccount(tc.podNS, tc.podSA, tc.srvNS, tc.srvSA)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkPodServiceAccount = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestPodServiceAccountProblem(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	write := func(tok []byte) {
		if err := os.WriteFile(tokenFile, tok, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(testToken(`{"sub":"system:serviceaccount:conquest:conquest"}`))
	if err := podServiceAccountProblem(tokenFile, "conquest", "conquest-pods"); err != nil {
		t.Fatalf("own pod account: %v", err)
	}
	if err := podServiceAccountProblem(tokenFile, "conquest", "conquest"); err == nil || !strings.Contains(err.Error(), "CONQUEST_SERVICE_ACCOUNT") {
		t.Fatalf("server's account: %v, want an error naming CONQUEST_SERVICE_ACCOUNT", err)
	}
	write([]byte("garbage"))
	if err := podServiceAccountProblem(tokenFile, "conquest", "conquest-pods"); err == nil {
		t.Fatal("unreadable token: want an error (the check can't be made)")
	}
	if err := podServiceAccountProblem(filepath.Join(dir, "missing"), "conquest", "conquest-pods"); err == nil {
		t.Fatal("missing token: want an error")
	}
}

func TestPodServiceAccountIsFatalOnlyForPerRunBuckets(t *testing.T) {
	if !podServiceAccountFatal(runbucket.ModeOBC) {
		t.Error("obc: a runner pod as the server's account could read every run's Secret")
	}
	if podServiceAccountFatal(runbucket.ModeShared) {
		t.Error("shared: keep starting (warn) as before")
	}
}
