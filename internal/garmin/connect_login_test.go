package garmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testEmail    = "runner@example.com"
	testPassword = "correct horse"
)

// fakeSSO answers each path with the handler registered for it, serves the
// ticket exchange and the social profile, and records every request.
type fakeSSO struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string
	routes   map[string]http.HandlerFunc
	// exchanges records each ticket exchange form.
	exchanges []map[string]string
	// profileStatus is what the social profile check answers (200 if zero).
	profileStatus int
}

func (f *fakeSSO) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	switch r.URL.Path {
	case "/di-oauth2-service/oauth/token":
		if err := r.ParseForm(); err != nil {
			f.t.Fatal(err)
		}
		form := map[string]string{}
		for key := range r.PostForm {
			form[key] = r.PostForm.Get(key)
		}
		f.mu.Lock()
		f.exchanges = append(f.exchanges, form)
		f.mu.Unlock()
		access := jwtWith(f.t, map[string]any{"exp": testNow.Add(24 * time.Hour).Unix()})
		json.NewEncoder(w).Encode(map[string]string{"access_token": access, "refresh_token": "refresh"})
		return
	case socialProfilePath:
		if f.profileStatus != 0 {
			w.WriteHeader(f.profileStatus)
			return
		}
		fmt.Fprint(w, `{"displayName":"runner"}`)
		return
	}
	if route, ok := f.routes[r.Method+" "+r.URL.Path]; ok {
		route(w, r)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (f *fakeSSO) requested(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if strings.HasSuffix(request, " "+path) {
			return true
		}
	}
	return false
}

func newSSO(t *testing.T, routes map[string]http.HandlerFunc) (*fakeSSO, *ConnectClient) {
	t.Helper()
	f := &fakeSSO{t: t, routes: routes}
	return f, newConnect(t, f.handle)
}

func jsonAnswer(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, text) }
}

func statusAnswer(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// mobileSignInPage sets the cookie the credential POST must carry.
func mobileSignInPage(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "SESSION", Value: "s1", Path: "/"})
	fmt.Fprint(w, "<html></html>")
}

func noMFA(context.Context, string, bool) (string, error) {
	return "", errors.New("MFA was not expected")
}

func TestMobileSignInSavesTokens(t *testing.T) {
	var credentials map[string]any
	f, c := newSSO(t, map[string]http.HandlerFunc{
		"GET /mobile/sso/en_US/sign-in": mobileSignInPage,
		"POST /mobile/api/login": func(w http.ResponseWriter, r *http.Request) {
			if cookie, err := r.Cookie("SESSION"); err != nil || cookie.Value != "s1" {
				t.Errorf("sign-in page cookie missing: %v", err)
			}
			if r.URL.Query().Get("clientId") != iosSSOClientID || r.Header.Get("Referer") == "" {
				t.Errorf("query = %v, Referer = %q", r.URL.Query(), r.Header.Get("Referer"))
			}
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &credentials)
			fmt.Fprint(w, `{"responseStatus":{"type":"SUCCESSFUL"},"serviceTicketId":"ST-123"}`)
		},
	})
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	path, err := c.Login(context.Background(), testEmail, testPassword, LoginPrompts{MFACode: noMFA})
	if err != nil {
		t.Fatal(err)
	}
	if credentials["username"] != testEmail || credentials["password"] != testPassword {
		t.Errorf("credentials = %v", credentials)
	}
	if len(waits) != 1 || waits[0] < mobileDelay[0] || waits[0] > mobileDelay[1] {
		t.Errorf("waits = %v", waits)
	}
	if len(f.exchanges) != 1 || f.exchanges[0]["service_ticket"] != "ST-123" ||
		f.exchanges[0]["service_url"] != c.hosts.iosServiceURL() || f.exchanges[0]["grant_type"] != diGrantType ||
		f.exchanges[0]["client_id"] != diClientIDs[0] {
		t.Errorf("exchanges = %v", f.exchanges)
	}
	saved := savedTokens(t)
	if saved.RefreshToken != "refresh" || saved.ClientID != diClientIDs[0] {
		t.Errorf("saved = %+v", saved)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), testPassword) || strings.Contains(string(data), testEmail) {
		t.Error("the token file holds the credentials")
	}
	// The client signed in is ready to use without rereading the file.
	if _, err := c.Profile(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestWrongPasswordStopsTheChain(t *testing.T) {
	f, c := newSSO(t, map[string]http.HandlerFunc{
		"GET /mobile/sso/en_US/sign-in": mobileSignInPage,
		"POST /mobile/api/login":        jsonAnswer(`{"responseStatus":{"type":"INVALID_USERNAME_PASSWORD"}}`),
	})
	_, err := c.Login(context.Background(), testEmail, "wrong", LoginPrompts{MFACode: noMFA})
	if !errors.Is(err, ErrConnectLogin) || !strings.Contains(err.Error(), "email or password") {
		t.Errorf("err = %v", err)
	}
	if f.requested("/sso/embed") || f.requested("/portal/api/login") {
		t.Errorf("later strategies ran: %v", f.requests)
	}
}

func TestMobileMFAAcceptsASecondTry(t *testing.T) {
	var codes []string
	_, c := newSSO(t, map[string]http.HandlerFunc{
		"GET /mobile/sso/en_US/sign-in": mobileSignInPage,
		"POST /mobile/api/login": jsonAnswer(`{"responseStatus":{"type":"MFA_REQUIRED"},` +
			`"customerMfaInfo":{"mfaLastMethodUsed":"sms"}}`),
		"POST /mobile/api/mfa/verifyCode": func(w http.ResponseWriter, r *http.Request) {
			if cookie, err := r.Cookie("SESSION"); err != nil || cookie.Value != "s1" {
				t.Errorf("MFA check lost the sign-in cookie: %v", err)
			}
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &body)
			if body["mfaMethod"] != "sms" {
				t.Errorf("mfaMethod = %v", body["mfaMethod"])
			}
			if body["mfaVerificationCode"] == "123456" {
				fmt.Fprint(w, `{"responseStatus":{"type":"SUCCESSFUL"},"serviceTicketId":"ST-mfa"}`)
				return
			}
			fmt.Fprint(w, `{"responseStatus":{"type":"INVALID_MFA_CODE"}}`)
		},
		"POST /portal/api/mfa/verifyCode": jsonAnswer(`{"responseStatus":{"type":"INVALID_MFA_CODE"}}`),
	})
	_, err := c.Login(context.Background(), testEmail, testPassword, LoginPrompts{
		MFACode: func(_ context.Context, method string, retry bool) (string, error) {
			codes = append(codes, fmt.Sprintf("%s/%v", method, retry))
			if retry {
				return "123456", nil
			}
			return "000000", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(codes, ",") != "sms/false,sms/true" {
		t.Errorf("prompts = %v", codes)
	}
}

func TestBlockedStrategiesFallThroughToTheWidget(t *testing.T) {
	f, c := newSSO(t, map[string]http.HandlerFunc{
		"GET /mobile/sso/en_US/sign-in": statusAnswer(http.StatusTooManyRequests),
		"GET /sso/embed":                jsonAnswer("<html>embed</html>"),
		"GET /sso/signin":               jsonAnswer(`<input name="_csrf" value="csrf-1">`),
		"POST /sso/signin": func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseForm(); err != nil || r.PostForm.Get("_csrf") != "csrf-1" || r.PostForm.Get("password") != testPassword {
				t.Errorf("form = %v (%v)", r.PostForm, err)
			}
			fmt.Fprint(w, `<title>Success</title><a href="https://sso.garmin.com/sso/embed?ticket=ST-widget">`)
		},
	})
	if _, err := c.Login(context.Background(), testEmail, testPassword, LoginPrompts{MFACode: noMFA}); err != nil {
		t.Fatal(err)
	}
	if len(f.exchanges) != 1 || f.exchanges[0]["service_ticket"] != "ST-widget" || f.exchanges[0]["service_url"] != c.hosts.widgetEmbedURL() {
		t.Errorf("exchanges = %v", f.exchanges)
	}
	if f.requested("/portal/api/login") {
		t.Error("the portal ran after the widget succeeded")
	}
}

func TestWidgetMFA(t *testing.T) {
	var requestedCode map[string]any
	_, c := newSSO(t, map[string]http.HandlerFunc{
		"GET /mobile/sso/en_US/sign-in": statusAnswer(http.StatusForbidden),
		"GET /sso/embed":                jsonAnswer("<html>embed</html>"),
		"GET /sso/signin":               jsonAnswer(`<input name="_csrf" value="csrf-1">`),
		"POST /sso/signin": jsonAnswer(`<title>GARMIN Authentication Application</title>` +
			`<script>var customerGuid = "guid"; var mfaMethod = "email"; var locale = "en"; var clientId = "GarminConnect";</script>` +
			`<input name="_csrf" value="csrf-2">`),
		"POST /sso/verifyMFA/mfaCode": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &requestedCode)
		},
		"POST /sso/verifyMFA/loginEnterMfaCode": func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseForm(); err != nil || r.PostForm.Get("_csrf") != "csrf-2" || r.PostForm.Get("mfa-code") != "654321" {
				t.Errorf("form = %v (%v)", r.PostForm, err)
			}
			fmt.Fprint(w, `<title>Success</title>?ticket=ST-widget-mfa"`)
		},
	})
	_, err := c.Login(context.Background(), testEmail, testPassword, LoginPrompts{
		MFACode: func(context.Context, string, bool) (string, error) { return "654321", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestedCode["customerGuid"] != "guid" || requestedCode["mfaMethod"] != "email" {
		t.Errorf("code request = %v", requestedCode)
	}
}

func TestEveryStrategyRateLimited(t *testing.T) {
	_, c := newSSO(t, map[string]http.HandlerFunc{
		"GET /mobile/sso/en_US/sign-in": statusAnswer(http.StatusTooManyRequests),
		"GET /sso/embed":                statusAnswer(427),
		"GET /portal/sso/en-US/sign-in": statusAnswer(http.StatusTooManyRequests),
	})
	_, err := c.Login(context.Background(), testEmail, testPassword, LoginPrompts{MFACode: noMFA})
	if !errors.Is(err, ErrConnectLogin) || !strings.Contains(err.Error(), "rate-limited") || !strings.Contains(err.Error(), "garmin_tokens.json") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Error("the error holds the password")
	}
}

// A token connectapi refuses is account or region dependent; the next
// strategy may get one it accepts.
func TestRefusedTokenTriesTheNextStrategy(t *testing.T) {
	f, c := newSSO(t, map[string]http.HandlerFunc{
		"GET /mobile/sso/en_US/sign-in": mobileSignInPage,
		"POST /mobile/api/login":        jsonAnswer(`{"responseStatus":{"type":"SUCCESSFUL"},"serviceTicketId":"ST-1"}`),
		"GET /sso/embed":                statusAnswer(http.StatusForbidden),
		"GET /portal/sso/en-US/sign-in": statusAnswer(http.StatusForbidden),
	})
	f.profileStatus = http.StatusUnauthorized
	_, err := c.Login(context.Background(), testEmail, testPassword, LoginPrompts{MFACode: noMFA})
	if err == nil || !strings.Contains(err.Error(), "refused the new token") {
		t.Errorf("err = %v", err)
	}
	if !f.requested("/sso/embed") {
		t.Error("the widget wasn't tried")
	}
	if path, _ := ConnectTokenFile(); fileExists(path) {
		t.Error("a refused token was saved")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestCancelledSignInStopsWaiting(t *testing.T) {
	_, c := newSSO(t, map[string]http.HandlerFunc{"GET /mobile/sso/en_US/sign-in": mobileSignInPage})
	c.sleep = sleepContext
	c.jitter = func(time.Duration, time.Duration) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := c.Login(ctx, testEmail, testPassword, LoginPrompts{MFACode: noMFA}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
