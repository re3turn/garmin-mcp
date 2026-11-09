package garmin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

var testNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func fakeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func isolate(t *testing.T) string {
	t.Helper()
	home := setHome(t)
	t.Setenv(KeyEnvVar, "test-key")
	return home
}

// setHome points os.UserHomeDir at a temp dir on every OS (Windows reads
// USERPROFILE).
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func assertPrivate(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("%s mode = %o, want 600", path, mode)
	}
}

type fakeGarmin struct {
	t         *testing.T
	exchanges atomic.Int32
	gets      atomic.Int32
	server    *httptest.Server
}

// newFakeGarmin issues tokens named token-1, token-2, ... and passes GETs to
// handle with the bearer token they carried.
func newFakeGarmin(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, token string)) *fakeGarmin {
	t.Helper()
	f := &fakeGarmin{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "ApiKey test-key" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			n := f.exchanges.Add(1)
			token := fakeJWT(t, testNow.Add(24*time.Hour))
			fmt.Fprintf(w, "%s.%d\n", strings.TrimSuffix(token, ".signature"), n)
			return
		}
		f.gets.Add(1)
		auth := r.Header.Get("Authorization")
		handle(w, r, auth[strings.LastIndex(auth, ".")+1:])
	}))
	t.Cleanup(f.server.Close)
	return f
}

// client waits, before the test's temp home is removed, for any token exchange
// a cancelled caller left running, so its cache write can't race the removal.
// Cleanups run last-registered first, so this runs before the server closes
// and before the temp dir from isolate is removed.
func (f *fakeGarmin) client() *Client {
	c := NewClient(WithBaseURL(f.server.URL), WithClock(func() time.Time { return testNow }))
	f.t.Cleanup(c.auth.waitForExchanges)
	return c
}

func TestGetDecodesJSONAndKeepsLargeIDs(t *testing.T) {
	isolate(t)
	f := newFakeGarmin(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Query().Get("date") != "2026-10-30" {
			t.Errorf("query = %v", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"activityId": 21234567890123456}`)
	})
	got, err := f.client().Get(context.Background(), "/x", map[string][]string{"date": {"2026-10-30"}})
	if err != nil {
		t.Fatal(err)
	}
	if id := got.(map[string]any)["activityId"].(json.Number).String(); id != "21234567890123456" {
		t.Errorf("activityId = %s", id)
	}
}

func TestGetReturnsNilForNoContent(t *testing.T) {
	isolate(t)
	f := newFakeGarmin(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.WriteHeader(http.StatusNoContent)
	})
	got, err := f.client().Get(context.Background(), "/hrv-service/hrv/2026-10-31", nil)
	if err != nil || got != nil {
		t.Fatalf("got %v, %v; want nil, nil", got, err)
	}
}

func TestTokenIsCachedAcrossClients(t *testing.T) {
	home := isolate(t)
	f := newFakeGarmin(t, func(w http.ResponseWriter, _ *http.Request, _ string) { fmt.Fprint(w, `{}`) })
	for range 2 {
		if _, err := f.client().Get(context.Background(), "/x", nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.exchanges.Load(); n != 1 {
		t.Errorf("exchanges = %d, want 1", n)
	}
	assertPrivate(t, filepath.Join(home, ".config", "garmin", "connect-personal-api-bearer-token.json"))
}

// Tokens last 24 hours from testNow and are replaced within an hour of expiry.
func TestCachedTokenRefreshWindow(t *testing.T) {
	cases := []struct {
		name          string
		elapsed       time.Duration
		wantExchanges int32
	}{
		{"outside the window is reused", 22*time.Hour + 59*time.Minute, 1},
		{"inside the window is replaced", 23*time.Hour + time.Minute, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			f := newFakeGarmin(t, func(w http.ResponseWriter, _ *http.Request, _ string) { fmt.Fprint(w, `{}`) })
			if _, err := f.client().Get(context.Background(), "/x", nil); err != nil {
				t.Fatal(err)
			}
			later := NewClient(WithBaseURL(f.server.URL), WithClock(func() time.Time { return testNow.Add(tc.elapsed) }))
			if _, err := later.Get(context.Background(), "/x", nil); err != nil {
				t.Fatal(err)
			}
			if n := f.exchanges.Load(); n != tc.wantExchanges {
				t.Errorf("exchanges = %d, want %d", n, tc.wantExchanges)
			}
		})
	}
}

// The cache file is shared with other processes running this server, and
// expires_at may carry a fraction.
func TestCacheWrittenByAnotherProcessIsReused(t *testing.T) {
	home := isolate(t)
	f := newFakeGarmin(t, func(w http.ResponseWriter, _ *http.Request, token string) {
		if token != "other" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	sum := sha256.Sum256([]byte(f.server.URL + "\x00test-key"))
	cache := fmt.Sprintf(`{"fingerprint": %q, "access_token": "h.p.other", "expires_at": %d.25}`,
		hex.EncodeToString(sum[:]), testNow.Add(10*time.Hour).Unix())
	path := filepath.Join(home, ".config", "garmin", "connect-personal-api-bearer-token.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client().Get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if n := f.exchanges.Load(); n != 0 {
		t.Errorf("exchanges = %d, want the other process's token reused", n)
	}
}

func TestCacheForAnotherKeyIsIgnored(t *testing.T) {
	isolate(t)
	f := newFakeGarmin(t, func(w http.ResponseWriter, _ *http.Request, _ string) { fmt.Fprint(w, `{}`) })
	if _, err := f.client().Get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	other := NewClient(WithBaseURL(f.server.URL), WithClock(func() time.Time { return testNow }), WithAPIKey("other-key"))
	if _, err := other.Get(context.Background(), "/x", nil); !errors.Is(err, ErrAPIKey) {
		t.Fatalf("err = %v, want the other key exchanged and rejected", err)
	}
}

func TestUnauthorizedRetriesOnceWithFreshToken(t *testing.T) {
	isolate(t)
	f := newFakeGarmin(t, func(w http.ResponseWriter, _ *http.Request, token string) {
		if token == "1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `[1]`)
	})
	if _, err := f.client().Get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if e, g := f.exchanges.Load(), f.gets.Load(); e != 2 || g != 2 {
		t.Errorf("exchanges, gets = %d, %d; want 2, 2", e, g)
	}
}

func TestUnauthorizedTwiceSurfaces(t *testing.T) {
	isolate(t)
	f := newFakeGarmin(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := f.client().Get(context.Background(), "/x", nil)
	if !IsUnauthorized(err) {
		t.Fatalf("err = %v, want HTTP 401", err)
	}
	if e, g := f.exchanges.Load(), f.gets.Load(); e != 2 || g != 2 {
		t.Errorf("exchanges, gets = %d, %d; want 2, 2", e, g)
	}
}

func TestDownloadReturnsBytes(t *testing.T) {
	isolate(t)
	f := newFakeGarmin(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/zip") {
			t.Errorf("Accept = %q", accept)
		}
		fmt.Fprint(w, "PK\x03\x04")
	})
	got, err := f.client().Download(context.Background(), "/download-service/files/activity/1")
	if err != nil || string(got) != "PK\x03\x04" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestErrorBodyIsTruncatedOnRuneBoundary(t *testing.T) {
	body := strings.Repeat("a", maxErrorBody-1) + "é" + strings.Repeat("b", 100)
	msg := (&HTTPError{StatusCode: 500, Method: "GET", Path: "/x", Body: body}).Error()
	if !utf8.ValidString(msg) {
		t.Errorf("message is not valid UTF-8: %q", msg[len(msg)-10:])
	}
	if !strings.HasSuffix(msg, "...") || strings.Contains(msg, "b") {
		t.Errorf("body was not truncated: ...%q", msg[len(msg)-10:])
	}
}

func TestTokenExchangeErrors(t *testing.T) {
	cases := []struct {
		status    int
		keyErr    bool
		wantInMsg string
	}{
		{http.StatusBadRequest, true, "rejected"},
		{http.StatusUnauthorized, true, "rejected"},
		{http.StatusForbidden, true, "rejected"},
		{http.StatusServiceUnavailable, false, "HTTP 503"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			isolate(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			_, err := NewClient(WithBaseURL(server.URL)).Get(context.Background(), "/x", nil)
			if errors.Is(err, ErrAPIKey) != tc.keyErr || !strings.Contains(fmt.Sprint(err), tc.wantInMsg) {
				t.Errorf("err = %v", err)
			}
			if strings.Contains(fmt.Sprint(err), "test-key") {
				t.Errorf("error leaks the key: %v", err)
			}
		})
	}
}

func TestNonJWTTokenIsRejected(t *testing.T) {
	isolate(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "not-a-jwt")
	}))
	defer server.Close()
	_, err := NewClient(WithBaseURL(server.URL)).Get(context.Background(), "/x", nil)
	if err == nil || !strings.Contains(err.Error(), "exp claim") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveAPIKey(t *testing.T) {
	setHome(t)
	// t.Setenv restores the original value; Unsetenv then removes it for the test.
	t.Setenv(KeyEnvVar, "")
	os.Unsetenv(KeyEnvVar)
	if _, err := ResolveAPIKey(); !errors.Is(err, ErrAPIKey) {
		t.Fatalf("missing key: err = %v", err)
	}
	path, err := SaveAPIKey("from-file")
	if err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, path)
	if key, err := ResolveAPIKey(); err != nil || key != "from-file" {
		t.Errorf("file key = %q, %v", key, err)
	}
	t.Setenv(KeyEnvVar, " from-env ")
	if key, err := ResolveAPIKey(); err != nil || key != "from-env" {
		t.Errorf("env key = %q, %v", key, err)
	}
}

func TestEmptyKeyFileIsAKeyError(t *testing.T) {
	setHome(t)
	t.Setenv(KeyEnvVar, "")
	os.Unsetenv(KeyEnvVar)
	if _, err := SaveAPIKey(" "); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveAPIKey(); !errors.Is(err, ErrAPIKey) || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want an empty key file error", err)
	}
}

func TestSavingOverAReadableKeyFileTightensIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes don't apply on Windows")
	}
	home := setHome(t)
	path := filepath.Join(home, ".config", "garmin", "connect-personal-api-key")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveAPIKey("new"); err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, path)
}
