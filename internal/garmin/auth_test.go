package garmin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const parallelCalls = 6

// getInParallel runs one Get per goroutine, as garmin_get_day does, and
// returns their errors.
func getInParallel(t *testing.T, c *Client) []error {
	t.Helper()
	errs := make([]error, parallelCalls)
	var wg sync.WaitGroup
	for i := range parallelCalls {
		wg.Go(func() {
			_, errs[i] = c.Get(context.Background(), "/x", nil)
		})
	}
	wg.Wait()
	return errs
}

func TestParallelGetsShareOneExchange(t *testing.T) {
	isolate(t)
	f := newFakeGarmin(t, func(w http.ResponseWriter, _ *http.Request, _ string) { fmt.Fprint(w, `{}`) })
	for _, err := range getInParallel(t, f.client()) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.exchanges.Load(); n != 1 {
		t.Errorf("exchanges = %d, want 1", n)
	}
}

// Every call gets 401 with the first token; one refresh must serve them all.
func TestParallelUnauthorizedRefreshesOnce(t *testing.T) {
	isolate(t)
	f := newFakeGarmin(t, func(w http.ResponseWriter, _ *http.Request, token string) {
		if token == "1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	for _, err := range getInParallel(t, f.client()) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.exchanges.Load(); n != 2 {
		t.Errorf("exchanges = %d, want 2", n)
	}
}

func TestFailingExchangeIsSharedByConcurrentCallers(t *testing.T) {
	isolate(t)
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchanges.Add(1)
		// Slow enough that every caller arrives while the exchange is in flight,
		// with room for a loaded CI runner.
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	for _, err := range getInParallel(t, NewClient(WithBaseURL(server.URL))) {
		if err == nil || !isTokenError(err) {
			t.Errorf("err = %v, want the shared token error", err)
		}
	}
	if n := exchanges.Load(); n != 1 {
		t.Errorf("exchanges = %d, want 1", n)
	}
}

func TestCancelledCallerStopsWaitingForExchange(t *testing.T) {
	isolate(t)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := withinSeconds(t, 5, func() error {
		_, err := NewClient(WithBaseURL(server.URL)).Get(ctx, "/x", nil)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the caller's deadline", err)
	}
}

// withinSeconds fails the test instead of hanging when fn doesn't return,
// which is how a broken ctx check in Token would show up.
func withinSeconds(t *testing.T, seconds int, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Duration(seconds) * time.Second):
		t.Fatalf("call didn't return within %ds", seconds)
		return nil
	}
}

// A caller whose ctx ends while the exchange it started is in flight must not
// fail another caller that joined the same exchange.
func TestCancelledFirstCallerDoesNotFailOthersSharingTheExchange(t *testing.T) {
	isolate(t)
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			arrived <- struct{}{}
			<-release
			fmt.Fprint(w, fakeJWT(t, time.Now().Add(24*time.Hour)))
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	c := NewClient(WithBaseURL(server.URL))

	first, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		_, err := c.Get(first, "/x", nil)
		firstErr <- err
	}()
	<-arrived
	secondErr := make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), "/x", nil)
		secondErr <- err
	}()
	cancelFirst()
	if err := withinSeconds(t, 5, func() error { return <-firstErr }); !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: err = %v, want context.Canceled", err)
	}
	close(release)
	if err := withinSeconds(t, 5, func() error { return <-secondErr }); err != nil {
		t.Errorf("second caller: err = %v, want the shared exchange to succeed", err)
	}
}

func TestUnwritableCacheStillReusesToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory modes don't apply on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root can write to a 0500 directory, so the cache file would still work")
	}
	home := isolate(t)
	dir := filepath.Join(home, ".config", "garmin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	f := newFakeGarmin(t, func(w http.ResponseWriter, _ *http.Request, _ string) { fmt.Fprint(w, `{}`) })
	c := f.client()
	for range 3 {
		if _, err := c.Get(context.Background(), "/x", nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.exchanges.Load(); n != 1 {
		t.Errorf("exchanges = %d, want 1", n)
	}
}

// echoAuthorization stands in for a proxy whose error page echoes the request
// headers.
func echoAuthorization(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprintf(w, "upstream failed for Authorization: %s", r.Header.Get("Authorization"))
	}
}

func TestErrorsDoNotLeakTheKey(t *testing.T) {
	isolate(t)
	server := httptest.NewServer(echoAuthorization(http.StatusBadGateway))
	defer server.Close()
	_, err := NewClient(WithBaseURL(server.URL)).Get(context.Background(), "/x", nil)
	if err == nil || strings.Contains(err.Error(), "test-key") {
		t.Errorf("err = %v, want an error without the key", err)
	}
}

func TestErrorsDoNotLeakTheBearerToken(t *testing.T) {
	isolate(t)
	var sent atomic.Value
	f := newFakeGarmin(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		sent.Store(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		echoAuthorization(http.StatusInternalServerError)(w, r)
	})
	_, err := f.client().Get(context.Background(), "/x", nil)
	token, _ := sent.Load().(string)
	if err == nil || token == "" || strings.Contains(err.Error(), token) {
		t.Errorf("err = %v, want an error without the bearer token", err)
	}
}

func TestRejectedEnvKeyPointsAtClientConfiguration(t *testing.T) {
	isolate(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	_, err := NewClient(WithBaseURL(server.URL)).Get(context.Background(), "/x", nil)
	if !errors.Is(err, ErrAPIKey) || !strings.Contains(err.Error(), "MCP client's configuration") {
		t.Errorf("err = %v, want a hint to fix %s in the client configuration", err, KeyEnvVar)
	}
}

func TestRejectedFileKeyPointsAtAuth(t *testing.T) {
	setHome(t)
	t.Setenv(KeyEnvVar, "")
	os.Unsetenv(KeyEnvVar)
	if _, err := SaveAPIKey("file-key"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	_, err := NewClient(WithBaseURL(server.URL)).Get(context.Background(), "/x", nil)
	if !errors.Is(err, ErrAPIKey) || !strings.Contains(err.Error(), "run `garmin-personal-mcp auth`") ||
		strings.Contains(err.Error(), "MCP client's configuration") {
		t.Errorf("err = %v, want the hint to run auth", err)
	}
}

func TestSavingThroughASymlinkUpdatesItsTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs extra privileges on Windows")
	}
	home := setHome(t)
	target := filepath.Join(t.TempDir(), "dotfiles-key")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".config", "garmin", "connect-personal-api-key")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveAPIKey("new"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the key path is no longer a symlink: %v, %v", info, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "new\n" {
		t.Errorf("target = %q, %v; want the new key", data, err)
	}
}
