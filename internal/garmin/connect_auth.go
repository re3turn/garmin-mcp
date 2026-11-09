package garmin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// connectRefreshWindow is how long before expiry a DI token is renewed, so
	// a request never goes out with a token about to expire.
	connectRefreshWindow = 15 * time.Minute
	// connectLoginCommand is the only way to recover from a refused refresh.
	connectLoginCommand = "garmin-mcp auth --unofficial"
)

// ErrConnectLogin reports that there is no usable Garmin Connect sign-in. Its
// message is safe to show a model: it never contains a token or password.
var ErrConnectLogin = errors.New("garmin connect sign-in")

type connectLoginError struct{ msg string }

func (e *connectLoginError) Error() string { return e.msg }
func (e *connectLoginError) Unwrap() error { return ErrConnectLogin }

// connectTokens has the field names of python-garminconnect's
// garmin_tokens.json, so a token file saved by it can be copied in as is.
type connectTokens struct {
	AccessToken  string `json:"di_token"`
	RefreshToken string `json:"di_refresh_token"`
	ClientID     string `json:"di_client_id"`
}

// ConnectTokenFile is where `auth --unofficial` saves the sign-in. It is a
// separate file from python-garminconnect's ~/.garminconnect because every
// refresh replaces the refresh token: two tools sharing one file would each
// keep using a refresh token the other already spent.
func ConnectTokenFile() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "connect-unofficial-tokens.json"), nil
}

func readConnectTokens() (connectTokens, error) {
	path, err := ConnectTokenFile()
	if err != nil {
		return connectTokens{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return connectTokens{}, &connectLoginError{fmt.Sprintf(
			"Not signed in to Garmin Connect for the unofficial API. Run `%s` in a terminal.", connectLoginCommand)}
	}
	if err != nil {
		return connectTokens{}, err
	}
	var tokens connectTokens
	if err := json.Unmarshal(data, &tokens); err != nil || tokens.AccessToken == "" {
		return connectTokens{}, &connectLoginError{fmt.Sprintf(
			"The Garmin Connect token file %s is not usable. Run `%s` in a terminal.", path, connectLoginCommand)}
	}
	return tokens, nil
}

// CheckConnectSignIn reports why the unofficial API's tools would fail for
// want of a sign-in, from the token file alone: it makes no request, so a
// sign-in Garmin has since revoked still passes.
func CheckConnectSignIn() error {
	_, err := readConnectTokens()
	return err
}

func saveConnectTokens(tokens connectTokens) (string, error) {
	path, err := ConnectTokenFile()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(tokens)
	if err != nil {
		return "", err
	}
	return path, writePrivateFile(path, data)
}

// connectAuth keeps a DI access token fresh. One mutex serializes every refresh
// in the process; the token file is reread first because another process (a
// second MCP client running this server) may already have spent the refresh
// token.
type connectAuth struct {
	hosts      ConnectHosts
	httpClient *http.Client
	now        func() time.Time

	mu     sync.Mutex
	tokens connectTokens
}

// Token returns an access token. stale is one Garmin just answered 401 for, or
// "" for none; it is never returned again.
func (a *connectAuth) Token(ctx context.Context, stale string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tokens.AccessToken == "" || a.tokens.AccessToken == stale || a.expiring(a.tokens.AccessToken) {
		tokens, err := readConnectTokens()
		if err != nil {
			return "", err
		}
		a.tokens = tokens
	}
	if a.tokens.AccessToken != stale && !a.expiring(a.tokens.AccessToken) {
		return a.tokens.AccessToken, nil
	}
	// Detached from ctx: once Garmin has rotated the refresh token, dropping the
	// response would lose the only one that still works.
	tokens, err := refreshConnectTokens(context.WithoutCancel(ctx), a.httpClient, a.hosts, a.tokens)
	if err != nil {
		// Another process may have spent the refresh token between the reread
		// above and this request; if it saved newer tokens, use those instead of
		// reporting a sign-in that is in fact fine.
		if _, ok := errors.AsType[*connectLoginError](err); ok {
			if latest, readErr := readConnectTokens(); readErr == nil &&
				latest.AccessToken != a.tokens.AccessToken && latest.AccessToken != stale && !a.expiring(latest.AccessToken) {
				a.tokens = latest
				return latest.AccessToken, nil
			}
		}
		return "", err
	}
	a.tokens = tokens
	if _, err := saveConnectTokens(tokens); err != nil {
		// The new tokens still work for this process; the next one will need
		// to sign in again.
		slog.Warn("couldn't save the refreshed Garmin Connect tokens", "error", err)
	}
	return tokens.AccessToken, nil
}

// expiring treats a token without a readable exp claim as usable and leaves it
// to a 401 to say otherwise.
func (a *connectAuth) expiring(token string) bool {
	exp, err := jwtExpiration(token)
	if err != nil {
		return false
	}
	return time.Unix(int64(exp), 0).Sub(a.now()) < connectRefreshWindow
}

func refreshConnectTokens(ctx context.Context, httpClient *http.Client, hosts ConnectHosts, tokens connectTokens) (connectTokens, error) {
	if tokens.RefreshToken == "" || tokens.ClientID == "" {
		return connectTokens{}, &connectLoginError{fmt.Sprintf(
			"The Garmin Connect sign-in has expired and can't be renewed. Run `%s` in a terminal.", connectLoginCommand)}
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {tokens.ClientID},
		"refresh_token": {tokens.RefreshToken},
	}
	resp, err := postDIToken(ctx, httpClient, hosts, tokens.ClientID, "application/json", form)
	if err != nil {
		return connectTokens{}, err
	}
	switch {
	case resp.status == http.StatusBadRequest || resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden:
		return connectTokens{}, &connectLoginError{fmt.Sprintf(
			"Garmin refused to renew the Garmin Connect sign-in (HTTP %d). Run `%s` in a terminal.", resp.status, connectLoginCommand)}
	case resp.status < 200 || resp.status >= 300:
		// The body can echo the refresh token, so only the status is reported.
		return connectTokens{}, fmt.Errorf("renewing the Garmin Connect sign-in failed with HTTP %d", resp.status)
	}
	refreshed, err := parseDIToken(resp.body, tokens.ClientID)
	if err != nil {
		return connectTokens{}, err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = tokens.RefreshToken
	}
	return refreshed, nil
}

type diResponse struct {
	status int
	body   []byte
}

func postDIToken(ctx context.Context, httpClient *http.Client, hosts ConnectHosts, clientID, accept string, form url.Values) (diResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hosts.diTokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return diResponse{}, err
	}
	setNativeHeaders(req.Header)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(clientID+":")))
	req.Header.Set("Accept", accept)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := httpClient.Do(req)
	if err != nil {
		return diResponse{}, fmt.Errorf("couldn't reach Garmin's token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return diResponse{}, fmt.Errorf("couldn't reach Garmin's token endpoint: %w", err)
	}
	return diResponse{status: resp.StatusCode, body: body}, nil
}

// parseDIToken takes the client id from the token itself when it names one,
// as Garmin may issue the token for a newer client than the one asked.
func parseDIToken(body []byte, clientID string) (connectTokens, error) {
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.AccessToken == "" {
		return connectTokens{}, errors.New("garmin's token endpoint returned no access token")
	}
	if id := jwtStringClaim(parsed.AccessToken, "client_id"); id != "" {
		clientID = id
	}
	return connectTokens{AccessToken: parsed.AccessToken, RefreshToken: parsed.RefreshToken, ClientID: clientID}, nil
}

// jwtStringClaim reads a claim without verifying the signature, which this
// client can't do. It is only used to label the token, never to trust it.
func jwtStringClaim(token, name string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	value, _ := claims[name].(string)
	return value
}

// setNativeHeaders identifies the client as the Garmin Connect Android app;
// connectapi refuses a valid token without them.
func setNativeHeaders(h http.Header) {
	h.Set("User-Agent", "GCM-Android-5.23")
	h.Set("X-Garmin-User-Agent", "com.garmin.android.apps.connectmobile/5.23; ; Google/sdk_gphone64_arm64/google; Android/33; Dalvik/2.1.0")
	h.Set("X-Garmin-Paired-App-Version", "10861")
	h.Set("X-Garmin-Client-Platform", "Android")
	h.Set("X-App-Ver", "10861")
	h.Set("X-Lang", "en")
	h.Set("X-GCExperience", "GC5")
	h.Set("Accept-Language", "en-US,en;q=0.9")
}
