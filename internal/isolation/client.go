package isolation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/anuragxxd/conquest/internal/objstore"
)

// Client is the filer's object store in a task pod without S3 credentials:
// it asks the storage gateway for a presigned URL per object, then reads or
// writes there. Delete is not offered.
type Client struct {
	root string
	http *http.Client
}

var _ objstore.Client = (*Client)(nil)

// NewClient returns a client for a task's capability URL (see
// storagegw.Gateway.TaskRoot). A nil hc uses a default client.
func NewClient(root string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	noRedirect := *hc
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{root: strings.TrimRight(root, "/"), http: &noRedirect}
}

// ClientFromEnv returns a client for the capability URL in
// EnvTaskStorageURL, or false if it is not set.
func ClientFromEnv() (*Client, bool) {
	root := strings.TrimSpace(os.Getenv(EnvTaskStorageURL))
	if root == "" {
		return nil, false
	}
	return NewClient(root, nil), true
}

// Get downloads an object.
func (c *Client) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	resp, err := c.presigned(ctx, http.MethodGet, bucket, key, nil, 0)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, statusError("get", bucket, key, resp)
	}
	return resp.Body, nil
}

// Put uploads an object of known size. Presigned uploads are single PUTs,
// so S3's limit for one PUT (5 GiB on AWS) applies.
func (c *Client) Put(ctx context.Context, bucket, key string, r io.Reader, size int64) error {
	if size < 0 {
		return fmt.Errorf("put s3://%s/%s: the size must be known", bucket, key)
	}
	resp, err := c.presigned(ctx, http.MethodPut, bucket, key, r, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError("put", bucket, key, resp)
	}
	return nil
}

// Exists reports whether an object exists.
func (c *Client) Exists(ctx context.Context, bucket, key string) (bool, error) {
	resp, err := c.presigned(ctx, http.MethodHead, bucket, key, nil, 0)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, statusError("head", bucket, key, resp)
	}
}

// Delete is not offered through the gateway.
func (c *Client) Delete(_ context.Context, bucket, key string) error {
	return fmt.Errorf("delete s3://%s/%s: %w", bucket, key, errors.ErrUnsupported)
}

// ListPrefix lists the keys below prefix, which the gateway allows only
// below the task's DIRECTORY inputs. The prefix is taken as a directory:
// "a/b" lists "a/b/...", not "a/bc".
func (c *Client) ListPrefix(ctx context.Context, bucket, prefix string) ([]string, error) {
	dir := strings.TrimRight(prefix, "/") + "/"
	resp, err := c.do(ctx, http.MethodGet, c.objectURL(bucket, dir), nil, 0)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError("list", bucket, dir, resp)
	}
	var body struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("list s3://%s/%s: %w", bucket, dir, err)
	}
	return body.Keys, nil
}

// presigned asks the gateway where to send method for the object, then sends
// it there. The gateway answers 307 without reading a body, so the body goes
// only to the presigned URL.
func (c *Client) presigned(ctx context.Context, method, bucket, key string, body io.Reader, size int64) (*http.Response, error) {
	resp, err := c.do(ctx, method, c.objectURL(bucket, key), nil, 0)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusTemporaryRedirect {
		defer resp.Body.Close()
		return nil, statusError(strings.ToLower(method), bucket, key, resp)
	}
	resp.Body.Close()
	target, err := resp.Location()
	if err != nil {
		return nil, fmt.Errorf("%s s3://%s/%s: gateway redirect: %w", strings.ToLower(method), bucket, key, err)
	}
	return c.do(ctx, method, target.String(), body, size)
}

func (c *Client) do(ctx context.Context, method, rawURL string, body io.Reader, size int64) (*http.Response, error) {
	var rc io.ReadCloser = http.NoBody
	if body != nil && size > 0 {
		rc = io.NopCloser(body) // the caller closes its reader
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rc)
	if err != nil {
		return nil, err
	}
	if method == http.MethodPut {
		req.ContentLength = size
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// never log the URL: it is a capability
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return nil, fmt.Errorf("%s %s: %w", strings.ToLower(method), redact(rawURL), uerr.Err)
		}
		return nil, err
	}
	return resp, nil
}

func (c *Client) objectURL(bucket, key string) string {
	segments := strings.Split(key, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return c.root + "/" + url.PathEscape(bucket) + "/" + strings.Join(segments, "/")
}

// redact drops the path and query of a URL, which carry capabilities.
func redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<url>"
	}
	return u.Scheme + "://" + u.Host + "/..."
}

func statusError(op, bucket, key string, resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%s s3://%s/%s: %s: %s", op, bucket, key, resp.Status, strings.TrimSpace(string(msg)))
}
