// Package storagegw is Conquest's storage gateway: an HTTP front for the
// object store that remote TES endpoints use instead of S3 credentials.
//
// Every task forwarded to a remote endpoint gets capability URLs of the form
//
//	<public URL>/storage/v1/t/<task ID>/<signature>/<bucket>/<key>
//
// where the signature is an HMAC of the task ID. A request to such a URL is
// answered with a 307 redirect to an S3 URL presigned at that moment for that
// one object and method, so nothing long-lived leaves Conquest and an URL
// can't expire while a task waits in a remote queue. What a task may read or
// write is decided from its stored TES request, not from the URL:
//
//   - GET and HEAD: the task's inputs (exact keys, and keys below DIRECTORY
//     inputs);
//   - PUT: the task's outputs (exact keys, keys below wildcard and DIRECTORY
//     outputs, and keys in the directory of a FILE output, which is where
//     remote servers upload wildcard renderings of FILE outputs).
//
// URLs work while the task is active and for a grace period after it ends.
//
// Two optional extensions serve Conquest's own task pods when they get no S3
// credentials (internal/isolation): SetBound confines every task to a
// prefix, such as its run's root, on top of its request; SetLister lets a
// GET of a prefix URL (key ending in "/") below a DIRECTORY input list the
// keys there. Without them, the gateway behaves as described above.
package storagegw

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/anuragxxd/conquest/internal/domain"
)

// RoutePrefix is the path below which the gateway serves.
const RoutePrefix = "/storage/v1/t"

// TaskLookup reads tasks; repository.TaskStore satisfies it.
type TaskLookup interface {
	Get(ctx context.Context, taskID string) (*domain.Task, error)
}

// Presigner presigns object requests; objstore.S3Client satisfies it.
type Presigner interface {
	Presign(ctx context.Context, method, bucket, key string, expires time.Duration) (*url.URL, error)
}

// MaxListKeys bounds a listing's answer; longer listings get 413.
const MaxListKeys = 100000

// Lister lists the keys below a prefix; objstore.S3Client satisfies it.
type Lister interface {
	ListPrefix(ctx context.Context, bucket, prefix string) ([]string, error)
}

// Bound returns the "<bucket>/<key prefix>" that every object a task reaches
// must lie under, whatever its request names; "" allows nothing.
type Bound func(ctx context.Context, task *domain.Task) (string, error)

// Config configures the gateway.
type Config struct {
	// PublicURL is the gateway's base URL as remote endpoints reach it,
	// e.g. https://conquest.example.org.
	PublicURL string
	// Secret signs capability URLs; at least 32 bytes.
	Secret []byte
	// Buckets the gateway serves; requests for others are refused.
	Buckets []string
	// PresignExpiry bounds each redirect's presigned URL (default 15m).
	PresignExpiry time.Duration
	// Grace is how long after a task ends its URLs keep working
	// (default 10m), for uploads racing the end-of-task report.
	Grace time.Duration
}

// Gateway signs capability URLs and serves them.
type Gateway struct {
	cfg     Config
	tasks   TaskLookup
	presign Presigner
	lister  Lister
	bound   Bound
	log     *slog.Logger
	now     func() time.Time
}

// New creates a gateway.
func New(cfg Config, tasks TaskLookup, presign Presigner, log *slog.Logger) (*Gateway, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("storage gateway public URL %q must be an absolute http(s) URL", cfg.PublicURL)
	}
	if len(cfg.Secret) < 32 {
		return nil, errors.New("storage gateway secret must be at least 32 bytes")
	}
	if len(cfg.Buckets) == 0 {
		return nil, errors.New("storage gateway serves no bucket")
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if cfg.PresignExpiry <= 0 {
		cfg.PresignExpiry = 15 * time.Minute
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 10 * time.Minute
	}
	if u.Scheme == "http" {
		log.Warn("storage gateway URLs use plain HTTP: capability and presigned URLs can be read on the network; use only on trusted networks")
	}
	return &Gateway{cfg: cfg, tasks: tasks, presign: presign, log: log, now: time.Now}, nil
}

// SetBound confines every task to the prefix bound returns.
func (g *Gateway) SetBound(bound Bound) { g.bound = bound }

// SetLister enables listings: a GET of a prefix URL below one of the task's
// DIRECTORY inputs answers {"keys": [...]} instead of a redirect.
func (g *Gateway) SetLister(l Lister) { g.lister = l }

// URL returns the capability URL through which a task reaches an object
// or, with a key ending in "/", a prefix.
func (g *Gateway) URL(taskID, bucket, key string) string {
	segments := strings.Split(key, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/%s/%s", g.TaskRoot(g.cfg.PublicURL, taskID),
		url.PathEscape(bucket), strings.Join(segments, "/"))
}

// TaskRoot returns the capability URL of a task without bucket and key, on
// the given base URL (e.g. Conquest's in-cluster address); object URLs are
// <root>/<bucket>/<key>.
func (g *Gateway) TaskRoot(baseURL, taskID string) string {
	return fmt.Sprintf("%s%s/%s/%s", strings.TrimRight(baseURL, "/"), RoutePrefix,
		url.PathEscape(taskID), g.sign(taskID))
}

func (g *Gateway) sign(taskID string) string {
	mac := hmac.New(sha256.New, g.cfg.Secret)
	mac.Write([]byte("conquest-storage-gateway/v1\x00" + taskID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Routes mounts the gateway.
func (g *Gateway) Routes(r chi.Router) {
	r.MethodFunc(http.MethodGet, RoutePrefix+"/*", g.serve)
	r.MethodFunc(http.MethodHead, RoutePrefix+"/*", g.serve)
	r.MethodFunc(http.MethodPut, RoutePrefix+"/*", g.serve)
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	taskID, sig, bucket, key, ok := parsePath(r.URL.EscapedPath())
	if !ok {
		http.Error(w, "malformed storage URL", http.StatusBadRequest)
		return
	}
	if !hmac.Equal([]byte(sig), []byte(g.sign(taskID))) {
		// same answer as for a missing task: don't confirm task IDs
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	task, err := g.tasks.Get(r.Context(), taskID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !g.active(task) {
		http.Error(w, "task has ended", http.StatusGone)
		return
	}
	if !contains(g.cfg.Buckets, bucket) {
		http.Error(w, "bucket not served", http.StatusForbidden)
		return
	}
	if !g.withinBound(r.Context(), task, bucket, key) {
		g.log.Warn("storage gateway: out of bound", "task_id", taskID, "method", r.Method, "bucket", bucket, "key", key)
		http.Error(w, "object outside the task's bound", http.StatusForbidden)
		return
	}
	scope, err := scopeOf(task)
	if err != nil {
		g.log.Error("storage gateway: unreadable task request", "task_id", taskID, "err", err)
		http.Error(w, "task request unreadable", http.StatusInternalServerError)
		return
	}
	if g.lister != nil && r.Method == http.MethodGet && strings.HasSuffix(key, "/") {
		g.list(w, r, taskID, scope, bucket, key)
		return
	}
	write := r.Method == http.MethodPut
	if !scope.allows(bucket, key, write) {
		g.log.Warn("storage gateway: out of scope", "task_id", taskID, "method", r.Method, "bucket", bucket, "key", key)
		http.Error(w, "object not in the task's inputs or outputs", http.StatusForbidden)
		return
	}
	target, err := g.presign.Presign(r.Context(), r.Method, bucket, key, g.cfg.PresignExpiry)
	if err != nil {
		g.log.Error("storage gateway: presign failed", "task_id", taskID, "err", err)
		http.Error(w, "presign failed", http.StatusBadGateway)
		return
	}
	g.log.Info("storage gateway: redirect", "task_id", taskID, "method", r.Method, "bucket", bucket, "key", key)
	// 307 keeps the method and body (RFC 9110 §15.4.8).
	w.Header().Set("Location", target.String())
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusTemporaryRedirect)
}

// withinBound reports whether bucket/key lies under the task's bound; with
// no bound set, everything does.
func (g *Gateway) withinBound(ctx context.Context, task *domain.Task, bucket, key string) bool {
	if g.bound == nil {
		return true
	}
	prefix, err := g.bound(ctx, task)
	if err != nil {
		g.log.Error("storage gateway: bound failed", "task_id", task.TaskID, "err", err)
		return false
	}
	return prefix != "" && strings.HasPrefix(bucket+"/"+key, prefix)
}

// list answers the keys below prefix, which must lie below a DIRECTORY input.
func (g *Gateway) list(w http.ResponseWriter, r *http.Request, taskID string, s *scope, bucket, prefix string) {
	if !s.allowsList(bucket, prefix) {
		g.log.Warn("storage gateway: listing out of scope", "task_id", taskID, "bucket", bucket, "prefix", prefix)
		http.Error(w, "prefix not below the task's directory inputs", http.StatusForbidden)
		return
	}
	keys, err := g.lister.ListPrefix(r.Context(), bucket, prefix)
	if err != nil {
		g.log.Error("storage gateway: listing failed", "task_id", taskID, "err", err)
		http.Error(w, "listing failed", http.StatusBadGateway)
		return
	}
	if len(keys) > MaxListKeys {
		g.log.Warn("storage gateway: listing too long", "task_id", taskID, "bucket", bucket, "prefix", prefix, "keys", len(keys))
		http.Error(w, fmt.Sprintf("more than %d keys", MaxListKeys), http.StatusRequestEntityTooLarge)
		return
	}
	if keys == nil {
		keys = []string{}
	}
	g.log.Info("storage gateway: list", "task_id", taskID, "bucket", bucket, "prefix", prefix, "keys", len(keys))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Keys []string `json:"keys"`
	}{keys})
}

func (g *Gateway) active(task *domain.Task) bool {
	if !task.State.IsTerminal() {
		return true
	}
	end := task.UpdatedAt
	if task.FinishedAt != nil {
		end = *task.FinishedAt
	}
	return g.now().Sub(end) <= g.cfg.Grace
}

// parsePath splits /storage/v1/t/<task>/<sig>/<bucket>/<key...>.
func parsePath(escaped string) (taskID, sig, bucket, key string, ok bool) {
	rest, found := strings.CutPrefix(escaped, RoutePrefix+"/")
	if !found {
		return "", "", "", "", false
	}
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) != 4 || parts[3] == "" {
		return "", "", "", "", false
	}
	decoded := make([]string, 0, 4)
	for _, p := range parts[:3] {
		v, err := url.PathUnescape(p)
		if err != nil || v == "" {
			return "", "", "", "", false
		}
		decoded = append(decoded, v)
	}
	var keyParts []string
	for _, seg := range strings.Split(parts[3], "/") {
		v, err := url.PathUnescape(seg)
		if err != nil || hasDotSegment(v) {
			return "", "", "", "", false
		}
		keyParts = append(keyParts, v)
	}
	return decoded[0], decoded[1], decoded[2], strings.Join(keyParts, "/"), true
}

// hasDotSegment reports whether a decoded path segment is, or contains as
// an encoded slash (%2F) would, a "." or ".." segment.
func hasDotSegment(decoded string) bool {
	for _, part := range strings.Split(decoded, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}

// scope is what a task may read and write.
type scope struct {
	readExact, readPrefix   map[string]bool // "bucket/key"
	writeExact, writePrefix map[string]bool
}

func scopeOf(task *domain.Task) (*scope, error) {
	var req struct {
		Inputs []struct {
			URL  string `json:"url"`
			Type string `json:"type"`
		} `json:"inputs"`
		Outputs []struct {
			URL        string `json:"url"`
			Path       string `json:"path"`
			PathPrefix string `json:"path_prefix"`
			Type       string `json:"type"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(task.TaskRequestPayload, &req); err != nil {
		return nil, err
	}
	s := &scope{map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}}
	for _, in := range req.Inputs {
		obj, ok := s3Object(in.URL)
		if !ok {
			continue
		}
		if in.Type == "DIRECTORY" {
			s.readPrefix[dirPrefix(obj)] = true
		} else {
			s.readExact[obj] = true
		}
	}
	for _, out := range req.Outputs {
		obj, ok := s3Object(out.URL)
		if !ok {
			continue
		}
		wildcard := out.PathPrefix != "" || strings.ContainsAny(out.Path, "*?[")
		if out.Type == "DIRECTORY" || wildcard {
			s.writePrefix[dirPrefix(obj)] = true
			continue
		}
		s.writeExact[obj] = true
		if dir := path.Dir(obj); dir != "." {
			s.writePrefix[dir+"/"] = true
		}
	}
	return s, nil
}

func (s *scope) allows(bucket, key string, write bool) bool {
	obj := bucket + "/" + key
	exact, prefixes := s.readExact, s.readPrefix
	if write {
		exact, prefixes = s.writeExact, s.writePrefix
	}
	if exact[obj] {
		return true
	}
	for p := range prefixes {
		if strings.HasPrefix(obj, p) {
			return true
		}
	}
	return false
}

// allowsList reports whether a prefix (ending in "/") lies below one of the
// task's DIRECTORY inputs.
func (s *scope) allowsList(bucket, prefix string) bool {
	obj := bucket + "/" + prefix
	for p := range s.readPrefix {
		if strings.HasPrefix(obj, p) {
			return true
		}
	}
	return false
}

func s3Object(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return "", false
	}
	key := strings.TrimPrefix(u.Path, "/")
	if key == "" {
		return u.Host + "/", true
	}
	return u.Host + "/" + key, true
}

func dirPrefix(obj string) string {
	return strings.TrimRight(obj, "/") + "/"
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
