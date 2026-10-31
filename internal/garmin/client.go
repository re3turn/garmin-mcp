package garmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultTimeout = 30 * time.Second
	// Error bodies reach the model; an HTML error page would flood it.
	maxErrorBody = 512
)

type HTTPError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("%s %s returned HTTP %d", e.Method, e.Path, e.StatusCode)
	if body := strings.TrimSpace(e.Body); body != "" {
		if len(body) > maxErrorBody {
			body = strings.ToValidUTF8(body[:maxErrorBody], "") + "..."
		}
		msg += ": " + body
	}
	return msg
}

func IsUnauthorized(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusUnauthorized
}

type Client struct {
	baseURL    string
	httpClient *http.Client
	auth       *Authenticator
}

type Option func(*Client)

func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(baseURL, "/") }
}

func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) { c.httpClient = httpClient }
}

// WithAPIKey uses this key instead of the environment variable or key file.
func WithAPIKey(key string) Option {
	return func(c *Client) { c.auth.apiKey = key }
}

func WithClock(now func() time.Time) Option {
	return func(c *Client) { c.auth.now = now }
}

func NewClient(opts ...Option) *Client {
	baseURL := DefaultBaseURL
	if value := os.Getenv(BaseURLEnvVar); value != "" {
		baseURL = strings.TrimRight(value, "/")
	}
	c := &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: defaultTimeout},
		auth:       &Authenticator{now: time.Now},
	}
	for _, opt := range opts {
		opt(c)
	}
	c.auth.baseURL = c.baseURL
	c.auth.httpClient = c.httpClient
	return c
}

// Get returns the decoded JSON body, or nil when Garmin answers with no
// content (its way of saying nothing was recorded).
func (c *Client) Get(ctx context.Context, path string, query url.Values) (any, error) {
	body, err := c.do(ctx, path, query, "application/json")
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	// Keeps large ids such as activityId exact.
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("GET %s returned invalid JSON: %w", path, err)
	}
	return value, nil
}

func (c *Client) Download(ctx context.Context, path string) ([]byte, error) {
	body, err := c.do(ctx, path, nil, "application/zip, application/x-zip-compressed, application/octet-stream")
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("GET %s returned an empty file", path)
	}
	return body, nil
}

// do retries once with another token on 401, because Garmin can revoke a
// bearer token before its exp claim.
func (c *Client) do(ctx context.Context, path string, query url.Values, accept string) ([]byte, error) {
	token, err := c.auth.Token(ctx, "")
	if err != nil {
		return nil, err
	}
	body, err := c.get(ctx, path, query, accept, token)
	if !IsUnauthorized(err) {
		return body, err
	}
	token, err = c.auth.Token(ctx, token)
	if err != nil {
		return nil, err
	}
	return c.get(ctx, path, query, accept, token)
}

func (c *Client) get(ctx context.Context, path string, query url.Values, accept, token string) ([]byte, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach Garmin: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Method: http.MethodGet, Path: path, Body: redact(body, token)}
	}
	return body, nil
}

// redact hides the credential a request sent, in case a proxy or gateway
// echoes request headers into its error page: error bodies reach the model.
func redact(body []byte, secret string) string {
	return strings.ReplaceAll(string(body), secret, "[redacted]")
}
