package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anuragxxd/conquest/internal/runbucket"
)

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
