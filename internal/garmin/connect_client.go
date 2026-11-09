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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ConnectClient reads the unofficial Garmin Connect API with the tokens that
// `auth --unofficial` saved. It only issues GETs, plus POSTs to the GraphQL
// gateway for queries.
type ConnectClient struct {
	hosts      ConnectHosts
	httpClient *http.Client
	auth       *connectAuth
	// sleep and jitter pace the sign-in; tests replace them so they don't wait.
	sleep  func(context.Context, time.Duration) error
	jitter func(low, high time.Duration) time.Duration

	profileMu sync.Mutex
	profile   *connectProfile
}

type ConnectOption func(*ConnectClient)

// WithConnectHosts points the client at other hosts. It exists for tests:
// production code takes the region from GARMIN_CONNECT_DOMAIN, so the
// password and tokens only ever go to Garmin.
func WithConnectHosts(hosts ConnectHosts) ConnectOption {
	return func(c *ConnectClient) { c.hosts = hosts }
}

func WithConnectHTTPClient(httpClient *http.Client) ConnectOption {
	return func(c *ConnectClient) { c.httpClient = httpClient }
}

func WithConnectClock(now func() time.Time) ConnectOption {
	return func(c *ConnectClient) { c.auth.now = now }
}

func NewConnectClient(opts ...ConnectOption) (*ConnectClient, error) {
	hosts, err := connectHostsFromEnv()
	if err != nil {
		return nil, err
	}
	c := &ConnectClient{
		hosts:      hosts,
		httpClient: &http.Client{Timeout: defaultTimeout},
		auth:       &connectAuth{now: time.Now},
		sleep:      sleepContext,
		jitter:     randomBetween,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.auth.hosts = c.hosts
	c.auth.httpClient = c.httpClient
	return c, nil
}

func (c *ConnectClient) Get(ctx context.Context, path string, query url.Values) (any, error) {
	return c.request(ctx, http.MethodGet, path, query, nil)
}

func (c *ConnectClient) request(ctx context.Context, method, path string, query url.Values, payload []byte) (any, error) {
	token, err := c.auth.Token(ctx, "")
	if err != nil {
		return nil, err
	}
	body, err := c.send(ctx, method, path, query, payload, token)
	// Only 401 means the token was refused; a 403 can also be Garmin blocking
	// the client, which a new token wouldn't change.
	if IsUnauthorized(err) {
		if token, err = c.auth.Token(ctx, token); err != nil {
			return nil, err
		}
		body, err = c.send(ctx, method, path, query, payload, token)
	}
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return nil, err
	}
	return decodeJSON(body, method, path)
}

func (c *ConnectClient) send(ctx context.Context, method, path string, query url.Values, payload []byte, token string) ([]byte, error) {
	target := c.hosts.API + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	setNativeHeaders(req.Header)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach Garmin: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("couldn't reach Garmin: %w", &url.Error{Op: method, URL: path, Err: err})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: c.redact(body, token)}
	}
	return body, nil
}

func (c *ConnectClient) redact(body []byte, token string) string {
	text := redact(body, token)
	c.auth.mu.Lock()
	refresh := c.auth.tokens.RefreshToken
	c.auth.mu.Unlock()
	if refresh != "" {
		text = strings.ReplaceAll(text, refresh, "[redacted]")
	}
	return text
}

func decodeJSON(body []byte, method, path string) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	// Keeps large ids such as activityId exact.
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%s %s returned invalid JSON: %w", method, path, err)
	}
	return value, nil
}

const graphQLPath = "/graphql-gateway/graphql"

// graphQLValue is all a query argument may contain. Arguments are written into
// the query text, so nothing that could close the string literal may get
// through.
var graphQLValue = regexp.MustCompile(`^[A-Za-z0-9-]{1,32}$`)

type graphQLArg struct{ name, value string }

// graphQL runs one anonymous query naming one root field and returns that
// field. Garmin can answer 200 with an errors array, which is a failure.
func (c *ConnectClient) graphQL(ctx context.Context, field string, args ...graphQLArg) (any, error) {
	parts := make([]string, len(args))
	for i, arg := range args {
		if !graphQLValue.MatchString(arg.value) {
			return nil, fmt.Errorf("invalid GraphQL argument %s", arg.name)
		}
		parts[i] = arg.name + `:"` + arg.value + `"`
	}
	payload, err := json.Marshal(map[string]string{"query": "query{" + field + "(" + strings.Join(parts, ",") + ")}"})
	if err != nil {
		return nil, err
	}
	value, err := c.request(ctx, http.MethodPost, graphQLPath, nil, payload)
	if err != nil {
		return nil, err
	}
	response, _ := value.(map[string]any)
	if messages := graphQLErrors(response["errors"]); len(messages) > 0 {
		return nil, fmt.Errorf("garmin's GraphQL gateway refused %s: %s", field, strings.Join(messages, "; "))
	}
	data, _ := response["data"].(map[string]any)
	return data[field], nil
}

func graphQLErrors(value any) []string {
	list, _ := value.([]any)
	messages := make([]string, 0, len(list))
	for _, item := range list {
		entry, _ := item.(map[string]any)
		message, _ := entry["message"].(string)
		if message == "" {
			message = "unknown error"
		}
		messages = append(messages, message)
	}
	return messages
}

// connectProfile holds the ids some endpoints put in their path.
type connectProfile struct {
	DisplayName string
	ProfileID   string
}

const socialProfilePath = "/userprofile-service/socialProfile"

// Profile is the account's social profile: display name, full name, and ids.
func (c *ConnectClient) Profile(ctx context.Context) (any, error) {
	return c.Get(ctx, socialProfilePath, nil)
}

func (c *ConnectClient) ids(ctx context.Context) (connectProfile, error) {
	c.profileMu.Lock()
	defer c.profileMu.Unlock()
	if c.profile != nil {
		return *c.profile, nil
	}
	value, err := c.Profile(ctx)
	if err != nil {
		return connectProfile{}, err
	}
	record, _ := value.(map[string]any)
	name, _ := record["displayName"].(string)
	if strings.TrimSpace(name) == "" {
		return connectProfile{}, errors.New("the Garmin Connect profile has no display name, which this request needs")
	}
	profile := connectProfile{DisplayName: name}
	switch id := record["profileId"].(type) {
	case json.Number:
		profile.ProfileID = id.String()
	case string:
		profile.ProfileID = id
	}
	c.profile = &profile
	return profile, nil
}

// pathSegment escapes a value Garmin returned before it goes into a path, so
// a display name can't add path segments or a query.
func pathSegment(value string) string { return url.PathEscape(value) }

func positiveID(name string, id int64) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("%s must be a positive number, got %d", name, id)
	}
	return strconv.FormatInt(id, 10), nil
}
