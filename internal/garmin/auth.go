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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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

var ReconnectHint = fmt.Sprintf(
	"Create a key at %s and run `garmin-personal-mcp auth` (or set %s).", KeyPage, KeyEnvVar)

// ErrAPIKey reports a missing or rejected Personal API key. Its message is
// safe to show a model: it never contains the key or a token.
var ErrAPIKey = errors.New("garmin personal API key")

type apiKeyError struct{ msg string }

func (e *apiKeyError) Error() string { return e.msg }
func (e *apiKeyError) Unwrap() error { return ErrAPIKey }

// ConfigDir is shared with Garmin's gc-personal CLI, so a key saved by either
// works for both.
func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "garmin"), nil
}

func KeyFile() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "connect-personal-api-key"), nil
}

func tokenCacheFile() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "connect-personal-api-bearer-token.json"), nil
}

// ResolveAPIKey prefers the environment variable over the key file, as
// Garmin's client does.
func ResolveAPIKey() (string, error) {
	if value, ok := os.LookupEnv(KeyEnvVar); ok {
		key := strings.TrimSpace(value)
		if key == "" {
			return "", &apiKeyError{fmt.Sprintf("%s is set but empty. %s", KeyEnvVar, ReconnectHint)}
		}
		return key, nil
	}
	path, err := KeyFile()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", &apiKeyError{"No usable Garmin Personal API key was found. " + ReconnectHint}
	}
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", &apiKeyError{fmt.Sprintf("The key file %s is empty. %s", path, ReconnectHint)}
	}
	return key, nil
}

func SaveAPIKey(key string) (string, error) {
	path, err := KeyFile()
	if err != nil {
		return "", err
	}
	if err := writePrivateFile(path, []byte(key+"\n")); err != nil {
		return "", err
	}
	return path, nil
}

func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// WriteFile keeps the mode of an existing file, so tighten it before the
	// secret goes in rather than after.
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

type cachedToken struct {
	Fingerprint string  `json:"fingerprint"`
	AccessToken string  `json:"access_token"`
	ExpiresAt   float64 `json:"expires_at"`
}

// Authenticator exchanges a Personal API key for a bearer token and caches it
// in the same file and format as Garmin's Python client.
type Authenticator struct {
	baseURL    string
	httpClient *http.Client
	now        func() time.Time
	// apiKey overrides key resolution; set when checking a key before saving it.
	apiKey string

	mu sync.Mutex
}

func (a *Authenticator) key() (string, error) {
	if a.apiKey != "" {
		return a.apiKey, nil
	}
	return ResolveAPIKey()
}

// Token resolves the key on every call so a key replaced with `auth` takes
// effect without restarting the server.
func (a *Authenticator) Token(ctx context.Context, forceRefresh bool) (string, error) {
	key, err := a.key()
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	sum := sha256.Sum256([]byte(a.baseURL + "\x00" + key))
	fingerprint := hex.EncodeToString(sum[:])
	if !forceRefresh {
		if token, ok := a.readCache(fingerprint); ok {
			return token, nil
		}
	}
	token, expiresAt, err := a.exchange(ctx, key)
	if err != nil {
		return "", err
	}
	a.writeCache(cachedToken{Fingerprint: fingerprint, AccessToken: token, ExpiresAt: expiresAt})
	return token, nil
}

func (a *Authenticator) readCache(fingerprint string) (string, bool) {
	path, err := tokenCacheFile()
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var cache cachedToken
	if err := json.Unmarshal(data, &cache); err != nil || cache.Fingerprint != fingerprint || cache.AccessToken == "" {
		return "", false
	}
	expiresAt := time.Unix(int64(cache.ExpiresAt), 0)
	if expiresAt.Sub(a.now()) <= refreshWindow {
		return "", false
	}
	return cache.AccessToken, true
}

// A cache that can't be written only costs an extra exchange next time.
func (a *Authenticator) writeCache(cache cachedToken) {
	path, err := tokenCacheFile()
	if err != nil {
		return
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	_ = writePrivateFile(path, data)
}

func (a *Authenticator) exchange(ctx context.Context, key string) (string, float64, error) {
	url := a.baseURL + tokenPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", 0, err
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
		return "", 0, err
	}
	switch {
	case resp.StatusCode == http.StatusBadRequest,
		resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		return "", 0, &apiKeyError{fmt.Sprintf(
			"Garmin rejected the Personal API key (HTTP %d); it may have expired or been deleted. %s",
			resp.StatusCode, ReconnectHint)}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return "", 0, &HTTPError{StatusCode: resp.StatusCode, Method: http.MethodPost, Path: tokenPath, Body: string(body)}
	}
	token := strings.TrimSpace(string(body))
	expiresAt, err := jwtExpiration(token)
	if err != nil {
		return "", 0, err
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
