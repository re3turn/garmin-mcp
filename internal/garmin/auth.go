package garmin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	DefaultBaseURL = "https://personal.connectapi.garmin.com"
	KeyEnvVar      = "GARMIN_PERSONAL_API_KEY"
	BaseURLEnvVar  = "GARMIN_PERSONAL_API_BASE_URL"
	KeyPage        = "https://connect.garmin.com/app/settings/personalApiToken"

	tokenPath = "/personalapitoken-service/personal-api/personal-api-key/token"
	// Garmin's own client refreshes a cached token this long before it expires.
	refreshWindow = time.Hour
)

var (
	ReconnectHint = fmt.Sprintf(
		"Create a key at %s and run `garmin-personal-mcp auth` (or set %s).", KeyPage, KeyEnvVar)
	// Running `auth` can't fix a key from the environment, because the
	// environment variable overrides the key file it writes.
	envKeyHint = fmt.Sprintf(
		"Create a key at %s and update %s in your MCP client's configuration, or remove it to use the key saved by `garmin-personal-mcp auth`.",
		KeyPage, KeyEnvVar)
)

// ErrAPIKey reports a missing or rejected Personal API key. Its message is
// safe to show a model: it never contains the key or a token.
var ErrAPIKey = errors.New("garmin personal API key")

type apiKeyError struct{ msg string }

func (e *apiKeyError) Error() string { return e.msg }
func (e *apiKeyError) Unwrap() error { return ErrAPIKey }

// tokenError marks a failure to get a bearer token for a reason other than the
// key itself, such as a Garmin outage, so callers can tell it apart from a data
// request that failed.
type tokenError struct{ err error }

func (e *tokenError) Error() string { return e.err.Error() }
func (e *tokenError) Unwrap() error { return e.err }

func isTokenError(err error) bool {
	var target *tokenError
	return errors.As(err, &target)
}

// configDir is shared with Garmin's gc-personal CLI, so a key saved by either
// works for both.
func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "garmin"), nil
}

func keyFile() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "connect-personal-api-key"), nil
}

func tokenCacheFile() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "connect-personal-api-bearer-token.json"), nil
}

// ResolveAPIKey prefers the environment variable over the key file, as
// Garmin's client does.
func ResolveAPIKey() (string, error) {
	key, _, err := resolveAPIKey()
	return key, err
}

// resolveAPIKey also reports whether the key came from the environment, which
// decides how to tell the user to replace it.
func resolveAPIKey() (key string, fromEnv bool, err error) {
	if value, ok := os.LookupEnv(KeyEnvVar); ok {
		key := strings.TrimSpace(value)
		if key == "" {
			return "", true, &apiKeyError{fmt.Sprintf("%s is set but empty. %s", KeyEnvVar, envKeyHint)}
		}
		return key, true, nil
	}
	path, err := keyFile()
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, &apiKeyError{"No usable Garmin Personal API key was found. " + ReconnectHint}
	}
	if err != nil {
		return "", false, err
	}
	key = strings.TrimSpace(string(data))
	if key == "" {
		return "", false, &apiKeyError{fmt.Sprintf("The key file %s is empty. %s", path, ReconnectHint)}
	}
	return key, false, nil
}

func SaveAPIKey(key string) (string, error) {
	path, err := keyFile()
	if err != nil {
		return "", err
	}
	if err := writePrivateFile(path, []byte(key+"\n")); err != nil {
		return "", err
	}
	return path, nil
}

// writePrivateFile goes through a temp file, which os.CreateTemp creates with
// mode 0600, so the secret is never readable by others and a failed write
// never truncates a working key.
func writePrivateFile(path string, data []byte) error {
	return replaceFile(path, data, 0o700)
}

type cachedToken struct {
	Fingerprint string  `json:"fingerprint"`
	AccessToken string  `json:"access_token"`
	ExpiresAt   float64 `json:"expires_at"`
}

// Authenticator exchanges a Personal API key for a bearer token and caches it
// in memory and in the same file and format as Garmin's Python client.
type Authenticator struct {
	baseURL    string
	httpClient *http.Client
	now        func() time.Time
	// apiKey overrides key resolution; set when checking a key before saving it.
	apiKey string

	exchanges singleflight.Group
	mu        sync.Mutex
	token     cachedToken
}

func (a *Authenticator) key() (key string, fromEnv bool, err error) {
	if a.apiKey != "" {
		return a.apiKey, false, nil
	}
	return resolveAPIKey()
}

// Token returns a bearer token, exchanging the key only when no usable token is
// cached. stale is a token Garmin just rejected with 401, or "" for none; it is
// never returned again. The key is resolved on every call so a key replaced
// with `auth` takes effect without restarting the server.
func (a *Authenticator) Token(ctx context.Context, stale string) (string, error) {
	key, fromEnv, err := a.key()
	if err != nil {
		if errors.Is(err, ErrAPIKey) {
			return "", err
		}
		// Such as a key file that can't be read: no request can be made.
		return "", &tokenError{err}
	}
	sum := sha256.Sum256([]byte(a.baseURL + "\x00" + key))
	fingerprint := hex.EncodeToString(sum[:])
	if token, ok := a.cached(fingerprint, stale); ok {
		return token, nil
	}
	// Concurrent callers (garmin_get_day's sections) share one exchange, so a
	// failing exchange fails them all at once instead of each retrying it in
	// turn, and a 401 on every section still costs one exchange.
	result := a.exchanges.DoChan(fingerprint, func() (any, error) {
		if token, ok := a.cached(fingerprint, stale); ok {
			return token, nil
		}
		// Detached from the first caller's ctx so its cancellation doesn't fail
		// the others waiting on the same exchange; the HTTP client's timeout
		// still bounds it.
		token, expiresAt, err := a.exchange(context.WithoutCancel(ctx), key, fromEnv)
		if err != nil {
			return nil, err
		}
		a.store(cachedToken{Fingerprint: fingerprint, AccessToken: token, ExpiresAt: expiresAt})
		return token, nil
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-result:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	}
}

// cached checks memory first and then the file, which another process such as
// Garmin's Python client may have refreshed.
func (a *Authenticator) cached(fingerprint, stale string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usable(a.token, fingerprint, stale) {
		return a.token.AccessToken, true
	}
	token, ok := readCache()
	if !ok || !a.usable(token, fingerprint, stale) {
		return "", false
	}
	a.token = token
	return token.AccessToken, true
}

func (a *Authenticator) usable(token cachedToken, fingerprint, stale string) bool {
	return token.Fingerprint == fingerprint &&
		token.AccessToken != "" &&
		token.AccessToken != stale &&
		time.Unix(int64(token.ExpiresAt), 0).Sub(a.now()) > refreshWindow
}

// store keeps the token in memory, so a cache file that can't be written (a
// read-only home directory) doesn't cost an exchange on every request; the
// file only shares the token with other processes.
func (a *Authenticator) store(token cachedToken) {
	a.mu.Lock()
	a.token = token
	a.mu.Unlock()
	path, err := tokenCacheFile()
	if err != nil {
		return
	}
	data, err := json.Marshal(token)
	if err != nil {
		return
	}
	_ = writePrivateFile(path, data)
}

func readCache() (cachedToken, bool) {
	path, err := tokenCacheFile()
	if err != nil {
		return cachedToken{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cachedToken{}, false
	}
	var token cachedToken
	if err := json.Unmarshal(data, &token); err != nil {
		return cachedToken{}, false
	}
	return token, true
}

func (a *Authenticator) exchange(ctx context.Context, key string, fromEnv bool) (string, float64, error) {
	endpoint := a.baseURL + tokenPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		// Such as an invalid GARMIN_PERSONAL_API_BASE_URL.
		return "", 0, &tokenError{err}
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", "ApiKey "+key)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("couldn't reach Garmin to exchange the Personal API key: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("couldn't reach Garmin to exchange the Personal API key: %w",
			&url.Error{Op: http.MethodPost, URL: tokenPath, Err: err})
	}
	switch {
	case resp.StatusCode == http.StatusBadRequest,
		resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		hint := ReconnectHint
		if fromEnv {
			hint = envKeyHint
		}
		return "", 0, &apiKeyError{fmt.Sprintf(
			"Garmin rejected the Personal API key (HTTP %d); it may have expired or been deleted. %s",
			resp.StatusCode, hint)}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return "", 0, &tokenError{&HTTPError{StatusCode: resp.StatusCode, Method: http.MethodPost, Path: tokenPath, Body: redact(body, key)}}
	}
	token := strings.TrimSpace(string(body))
	expiresAt, err := jwtExpiration(token)
	if err != nil {
		return "", 0, &tokenError{err}
	}
	return token, expiresAt, nil
}

func jwtExpiration(token string) (float64, error) {
	invalid := errors.New("the Garmin token endpoint did not return a JWT with an exp claim")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, invalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0, invalid
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == nil {
		return 0, invalid
	}
	return *claims.Exp, nil
}
