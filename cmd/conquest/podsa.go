package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anuragxxd/conquest/internal/runbucket"
)

// serviceAccountFromToken returns the namespace and name of the
// ServiceAccount a Kubernetes token was issued to (its "sub" claim,
// system:serviceaccount:<namespace>:<name>). The token is not verified: it is
// the server's own, read from its mounted file, and only names the account.
func serviceAccountFromToken(token []byte) (namespace, name string, err error) {
	parts := bytes.Split(bytes.TrimSpace(token), []byte("."))
	if len(parts) != 3 {
		return "", "", errors.New("ServiceAccount token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil {
		return "", "", fmt.Errorf("ServiceAccount token payload: %w", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", fmt.Errorf("ServiceAccount token claims: %w", err)
	}
	rest, ok := strings.CutPrefix(claims.Sub, "system:serviceaccount:")
	if !ok {
		return "", "", fmt.Errorf("ServiceAccount token subject %q names no ServiceAccount", claims.Sub)
	}
	namespace, name, ok = strings.Cut(rest, ":")
	if !ok || namespace == "" || name == "" {
		return "", "", fmt.Errorf("ServiceAccount token subject %q names no ServiceAccount", claims.Sub)
	}
	return namespace, name, nil
}

// checkPodServiceAccount refuses runner and task pods that would run as the
// server's own ServiceAccount: a token injected for it (by a mutating webhook
// keyed on the account, or a later change to the pod spec) would hand
// workflow code the server's rights, Secrets of every run included. podSA ""
// is the namespace's default account.
func checkPodServiceAccount(podNS, podSA, serverNS, serverSA string) error {
	podSA = strings.TrimSpace(podSA)
	if podSA == "" {
		podSA = "default"
	}
	if podNS == serverNS && podSA == serverSA {
		return fmt.Errorf("CONQUEST_SERVICE_ACCOUNT: runner and task pods would run as the server's own ServiceAccount %s/%s; give them one without rights (the chart creates one)", serverNS, serverSA)
	}
	return nil
}

// podServiceAccountProblem checks the pods' ServiceAccount in podNS against
// the server's, named by its token in tokenFile. An error also when the
// server's account can't be told.
func podServiceAccountProblem(tokenFile, podNS, podSA string) error {
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("can't tell the server's ServiceAccount: %w", err)
	}
	serverNS, serverSA, err := serviceAccountFromToken(token)
	if err != nil {
		return fmt.Errorf("can't tell the server's ServiceAccount: %w", err)
	}
	return checkPodServiceAccount(podNS, podSA, serverNS, serverSA)
}

// podServiceAccountFatal reports whether a podServiceAccountProblem stops the
// server: with per-run buckets, the server's account may read every run's
// Secret. With shared buckets, every pod already shares one Secret; the
// server warns, as it ran so before.
func podServiceAccountFatal(mode runbucket.Mode) bool {
	return mode.PerRun()
}
