// Package deploy holds render tests of the Helm chart. They run helm
// template and skip without helm, except in CI (CI=true), where a missing
// helm fails them.
package deploy

import (
	"bytes"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// object is one rendered manifest.
type object map[string]any

func (o object) kind() string { return str(o["kind"]) }

func (o object) name() string { return str(get(o, "metadata", "name")) }

func str(v any) string {
	s, _ := v.(string)
	return s
}

// get walks nested maps.
func get(v any, path ...string) any {
	if o, ok := v.(object); ok {
		v = map[string]any(o)
	}
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// render runs helm template on the chart with the --set values given.
func render(t *testing.T, sets ...string) ([]object, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("helm is required in CI")
		}
		t.Skip("helm not found")
	}
	args := []string{"template", "t", "charts/conquest", "--namespace", "conquest"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	cmd := exec.Command(helm, args...)
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+home, "HELM_CONFIG_HOME="+home, "HELM_DATA_HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, &renderError{stderr.String()}
	}
	var objs []object
	for _, doc := range strings.Split(stdout.String(), "\n---") {
		var o object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("parse rendered manifest: %v\n%s", err, doc)
		}
		if o != nil {
			objs = append(objs, o)
		}
	}
	return objs, nil
}

type renderError struct{ msg string }

func (e *renderError) Error() string { return e.msg }

func mustRender(t *testing.T, sets ...string) []object {
	t.Helper()
	objs, err := render(t, sets...)
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	return objs
}

func find(objs []object, kind, name string) object {
	for _, o := range objs {
		if o.kind() == kind && (name == "" || o.name() == name) {
			return o
		}
	}
	return nil
}

// serverEnv returns the env of the server container (value only).
func serverEnv(t *testing.T, objs []object) map[string]string {
	t.Helper()
	dep := find(objs, "Deployment", "")
	if dep == nil {
		t.Fatal("no Deployment")
	}
	containers, _ := get(dep, "spec", "template", "spec", "containers").([]any)
	env := map[string]string{}
	for _, c := range containers {
		if str(get(c, "name")) != "conquest" {
			continue
		}
		list, _ := get(c, "env").([]any)
		for _, e := range list {
			env[str(get(e, "name"))] = str(get(e, "value"))
		}
		return env
	}
	t.Fatal("no conquest container")
	return nil
}

// roleRules returns the chart Role's rules as "group/resource:verbs".
func roleRules(t *testing.T, objs []object) []string {
	t.Helper()
	role := find(objs, "Role", "")
	if role == nil {
		t.Fatal("no Role")
	}
	var out []string
	rules, _ := role["rules"].([]any)
	for _, r := range rules {
		var groups, resources, verbs []string
		for _, g := range get(r, "apiGroups").([]any) {
			groups = append(groups, str(g))
		}
		for _, g := range get(r, "resources").([]any) {
			resources = append(resources, str(g))
		}
		for _, g := range get(r, "verbs").([]any) {
			verbs = append(verbs, str(g))
		}
		for _, g := range groups {
			for _, res := range resources {
				out = append(out, g+"/"+res+":"+strings.Join(verbs, ","))
			}
		}
	}
	return out
}

func TestChart_PodsRunAsTheirOwnServiceAccount(t *testing.T) {
	objs := mustRender(t)
	dep := find(objs, "Deployment", "")
	serverSA := str(get(dep, "spec", "template", "spec", "serviceAccountName"))
	podSA := serverEnv(t, objs)["CONQUEST_SERVICE_ACCOUNT"]
	if podSA == "" || podSA == serverSA {
		t.Fatalf("CONQUEST_SERVICE_ACCOUNT = %q, server runs as %q: pods need their own account", podSA, serverSA)
	}
	sa := find(objs, "ServiceAccount", podSA)
	if sa == nil {
		t.Fatalf("ServiceAccount %s not created", podSA)
	}
	if v, ok := sa["automountServiceAccountToken"].(bool); !ok || v {
		t.Errorf("ServiceAccount %s: automountServiceAccountToken = %v, want false", podSA, sa["automountServiceAccountToken"])
	}
	for _, o := range objs {
		if o.kind() != "RoleBinding" && o.kind() != "ClusterRoleBinding" {
			continue
		}
		subjects, _ := o["subjects"].([]any)
		for _, s := range subjects {
			if str(get(s, "name")) == podSA {
				t.Errorf("%s %s binds the pods' ServiceAccount", o.kind(), o.name())
			}
		}
	}
}

func TestChart_PodServiceAccountSettings(t *testing.T) {
	objs := mustRender(t, "conquest.podServiceAccount.name=workflows")
	if got := serverEnv(t, objs)["CONQUEST_SERVICE_ACCOUNT"]; got != "workflows" {
		t.Errorf("CONQUEST_SERVICE_ACCOUNT = %q, want workflows", got)
	}
	if find(objs, "ServiceAccount", "workflows") == nil {
		t.Error("named pod ServiceAccount not created")
	}

	objs = mustRender(t, "conquest.podServiceAccount.create=false", "conquest.podServiceAccount.name=existing")
	if got := serverEnv(t, objs)["CONQUEST_SERVICE_ACCOUNT"]; got != "existing" {
		t.Errorf("CONQUEST_SERVICE_ACCOUNT = %q, want existing", got)
	}
	if find(objs, "ServiceAccount", "existing") != nil {
		t.Error("ServiceAccount created with create=false")
	}

	// an explicit env value wins, as before
	objs = mustRender(t, "conquest.env.CONQUEST_SERVICE_ACCOUNT=custom")
	env := serverEnv(t, objs)
	if env["CONQUEST_SERVICE_ACCOUNT"] != "custom" {
		t.Errorf("CONQUEST_SERVICE_ACCOUNT = %q, want custom", env["CONQUEST_SERVICE_ACCOUNT"])
	}

	// an upgrade with the old default kept in the values renders it; the
	// server then refuses obc and warns with shared (cmd/conquest)
	objs = mustRender(t, "conquest.env.CONQUEST_SERVICE_ACCOUNT=conquest")
	if got := serverEnv(t, objs)["CONQUEST_SERVICE_ACCOUNT"]; got != "conquest" {
		t.Errorf("CONQUEST_SERVICE_ACCOUNT = %q, want conquest", got)
	}

	// no account created and none named: the namespace's default
	objs = mustRender(t, "conquest.podServiceAccount.create=false")
	if got := serverEnv(t, objs)["CONQUEST_SERVICE_ACCOUNT"]; got != "default" {
		t.Errorf("CONQUEST_SERVICE_ACCOUNT = %q, want default", got)
	}

	// the pods' account can't be the server's
	if _, err := render(t, "conquest.podServiceAccount.name=conquest"); err == nil || !strings.Contains(err.Error(), "podServiceAccount") {
		t.Errorf("pod account named as the server's: render = %v, want an error", err)
	}
}

func TestChart_SharedRunBucketsNeedNoExtraRights(t *testing.T) {
	objs := mustRender(t)
	env := serverEnv(t, objs)
	if env["CONQUEST_RUN_BUCKETS"] != "shared" {
		t.Errorf("CONQUEST_RUN_BUCKETS = %q, want shared", env["CONQUEST_RUN_BUCKETS"])
	}
	for k := range env {
		if strings.HasPrefix(k, "CONQUEST_RUN_BUCKETS_OBC_") {
			t.Errorf("%s set with shared", k)
		}
	}
	for _, r := range roleRules(t, objs) {
		if strings.Contains(r, "objectbucketclaims") || strings.Contains(r, "/secrets") || strings.Contains(r, "/configmaps") {
			t.Errorf("Role rule %s with shared", r)
		}
	}
}

func TestChart_OBCRunBuckets(t *testing.T) {
	objs := mustRender(t,
		"storageIsolation.enabled=true",
		"storageIsolation.runBuckets=obc",
		"storageIsolation.obc.storageClassName=object-bucket",
		"storageIsolation.obc.maxSize=10Gi",
		"storageIsolation.obc.maxObjects=100000",
		"storageIsolation.obc.bindTimeout=15s",
	)
	env := serverEnv(t, objs)
	for k, want := range map[string]string{
		"CONQUEST_RUN_BUCKETS":                   "obc",
		"CONQUEST_RUN_BUCKETS_OBC_STORAGE_CLASS": "object-bucket",
		"CONQUEST_RUN_BUCKETS_OBC_MAX_SIZE":      "10Gi",
		"CONQUEST_RUN_BUCKETS_OBC_MAX_OBJECTS":   "100000",
		"CONQUEST_RUN_BUCKETS_OBC_BIND_TIMEOUT":  "15s",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	rules := roleRules(t, objs)
	for _, want := range []string{
		"objectbucket.io/objectbucketclaims:create,get,delete",
		"/secrets:get",
		"/configmaps:get",
	} {
		if !slices.Contains(rules, want) {
			t.Errorf("Role lacks %s; rules: %v", want, rules)
		}
	}

	// optional settings stay unset
	env = serverEnv(t, mustRender(t,
		"storageIsolation.enabled=true",
		"storageIsolation.runBuckets=obc",
		"storageIsolation.obc.storageClassName=object-bucket",
	))
	for _, k := range []string{"CONQUEST_RUN_BUCKETS_OBC_MAX_SIZE", "CONQUEST_RUN_BUCKETS_OBC_MAX_OBJECTS", "CONQUEST_RUN_BUCKETS_OBC_BIND_TIMEOUT"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s set without a value", k)
		}
	}
}

func TestChart_OBCRunBucketsRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		sets []string
		want string
	}{
		"no isolation":     {[]string{"storageIsolation.runBuckets=obc", "storageIsolation.obc.storageClassName=sc"}, "storageIsolation.enabled"},
		"no storage class": {[]string{"storageIsolation.enabled=true", "storageIsolation.runBuckets=obc"}, "storageClassName"},
		"objectfs": {[]string{"storageIsolation.enabled=true", "storageIsolation.runBuckets=obc",
			"storageIsolation.obc.storageClassName=sc", "conquest.workspace.mode=objectfs"}, "objectfs"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := render(t, tc.sets...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("render = %v, want an error naming %s", err, tc.want)
			}
		})
	}
}
