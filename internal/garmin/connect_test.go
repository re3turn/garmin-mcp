package garmin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func jwtWith(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func testHosts(base string) ConnectHosts {
	return ConnectHosts{SSO: base, API: base, DIAuth: base, Connect: base, MobileIntegration: base}
}

// newConnect serves every Garmin host from one fake; paths tell them apart.
func newConnect(t *testing.T, handle http.HandlerFunc) *ConnectClient {
	t.Helper()
	setHome(t)
	fake := httptest.NewServer(handle)
	t.Cleanup(fake.Close)
	c, err := NewConnectClient(WithConnectHosts(testHosts(fake.URL)), WithConnectClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func writeTokens(t *testing.T, tokens connectTokens) {
	t.Helper()
	if _, err := saveConnectTokens(tokens); err != nil {
		t.Fatal(err)
	}
}

func savedTokens(t *testing.T) connectTokens {
	t.Helper()
	path, err := ConnectTokenFile()
	if err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, path)
	data, err := readSealed(path, sealedConnectSignIn)
	if err != nil {
		t.Fatal(err)
	}
	var tokens connectTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		t.Fatal(err)
	}
	return tokens
}

func TestConnectWithoutSignInPointsAtAuth(t *testing.T) {
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected %s", r.URL) })
	_, err := c.Devices(context.Background())
	if !errors.Is(err, ErrConnectLogin) || !strings.Contains(err.Error(), "auth --unofficial") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckConnectSignInReadsOnlyTheFile(t *testing.T) {
	setHome(t)
	if err := CheckConnectSignIn(); !errors.Is(err, ErrConnectLogin) || !strings.Contains(err.Error(), "auth --unofficial") {
		t.Errorf("no file: err = %v", err)
	}
	path, _ := ConnectTokenFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeSealed(path, sealedConnectSignIn, []byte(`{"di_token":""}`)); err != nil {
		t.Fatal(err)
	}
	if err := CheckConnectSignIn(); !errors.Is(err, ErrConnectLogin) {
		t.Errorf("empty token: err = %v", err)
	}
	// An expired token still counts: it can be renewed on the first call.
	writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(-time.Hour)), RefreshToken: "r", ClientID: "c"})
	if err := CheckConnectSignIn(); err != nil {
		t.Errorf("saved sign-in: err = %v", err)
	}
}

// Signing in while the server runs takes effect without a restart, which is
// why startup only warns.
func TestSignInWhileServingTakesEffect(t *testing.T) {
	c := newConnect(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{}`) })
	if _, err := c.Devices(context.Background()); !errors.Is(err, ErrConnectLogin) {
		t.Fatalf("before sign-in: err = %v", err)
	}
	writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(time.Hour)), RefreshToken: "r", ClientID: "c"})
	if _, err := c.Devices(context.Background()); err != nil {
		t.Errorf("after sign-in: err = %v", err)
	}
}

func TestConnectSendsTokenAndAppHeaders(t *testing.T) {
	var got http.Header
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, `[{"deviceId":12345678901234567}]`)
	})
	access := fakeJWT(t, testNow.Add(time.Hour))
	writeTokens(t, connectTokens{AccessToken: access, RefreshToken: "refresh", ClientID: "client"})
	value, err := c.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer "+access || got.Get("User-Agent") != "GCM-Android-5.23" || got.Get("X-Garmin-Client-Platform") != "Android" {
		t.Errorf("headers = %v", got)
	}
	if id := value.([]any)[0].(map[string]any)["deviceId"]; id != json.Number("12345678901234567") {
		t.Errorf("deviceId = %v", id)
	}
}

// diauthHandler answers a refresh with access-<n> tokens and records the forms.
type diauth struct {
	mu    sync.Mutex
	forms []url.Values
	auth  []string
	code  int
}

func (d *diauth) serve(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/di-oauth2-service/oauth/token" {
		return false
	}
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.forms = append(d.forms, r.PostForm)
	d.auth = append(d.auth, r.Header.Get("Authorization"))
	n := len(d.forms)
	d.mu.Unlock()
	if d.code != 0 {
		w.WriteHeader(d.code)
		fmt.Fprintf(w, `{"error":"invalid_grant","echo":%q}`, r.PostForm.Get("refresh_token"))
		return true
	}
	access := jwtWith(t, map[string]any{"exp": testNow.Add(24 * time.Hour).Unix(), "client_id": "NEWER_CLIENT", "n": n})
	json.NewEncoder(w).Encode(map[string]string{"access_token": access, "refresh_token": fmt.Sprintf("refresh-%d", n)})
	return true
}

func TestExpiringTokenIsRefreshedAndSaved(t *testing.T) {
	d := &diauth{}
	var bearer string
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) {
		if d.serve(t, w, r) {
			return
		}
		bearer = r.Header.Get("Authorization")
		fmt.Fprint(w, `{}`)
	})
	writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(10*time.Minute)), RefreshToken: "refresh-0", ClientID: "OLD_CLIENT"})
	if _, err := c.TrainingStatus(context.Background(), day(t, "2026-10-30")); err != nil {
		t.Fatal(err)
	}
	if len(d.forms) != 1 {
		t.Fatalf("refreshes = %d, want 1", len(d.forms))
	}
	form := d.forms[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "refresh-0" || form.Get("client_id") != "OLD_CLIENT" {
		t.Errorf("form = %v", form)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("OLD_CLIENT:")); d.auth[0] != want {
		t.Errorf("Authorization = %q, want %q", d.auth[0], want)
	}
	saved := savedTokens(t)
	if saved.RefreshToken != "refresh-1" || saved.ClientID != "NEWER_CLIENT" || bearer != "Bearer "+saved.AccessToken {
		t.Errorf("saved = %+v, bearer = %q", saved, bearer)
	}
}

func TestUnauthorizedRefreshesOnceAndRetries(t *testing.T) {
	d := &diauth{}
	var gets []string
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) {
		if d.serve(t, w, r) {
			return
		}
		gets = append(gets, r.Header.Get("Authorization"))
		if len(gets) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(time.Hour)), RefreshToken: "refresh-0", ClientID: "client"})
	if _, err := c.Devices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.forms) != 1 || len(gets) != 2 || gets[0] == gets[1] {
		t.Errorf("refreshes = %d, gets = %v", len(d.forms), gets)
	}
}

// A 403 can be Garmin blocking the client; a new token wouldn't help.
func TestForbiddenIsNotRetried(t *testing.T) {
	d := &diauth{}
	gets := 0
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) {
		if d.serve(t, w, r) {
			return
		}
		gets++
		w.WriteHeader(http.StatusForbidden)
	})
	writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(time.Hour)), RefreshToken: "r", ClientID: "c"})
	var httpErr *HTTPError
	if _, err := c.Devices(context.Background()); !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusForbidden {
		t.Errorf("err = %v", err)
	}
	if gets != 1 || len(d.forms) != 0 {
		t.Errorf("gets = %d, refreshes = %d", gets, len(d.forms))
	}
}

func TestRefusedRefreshPointsAtAuthAndHidesTheBody(t *testing.T) {
	d := &diauth{code: http.StatusBadRequest}
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) {
		if !d.serve(t, w, r) {
			t.Errorf("unexpected %s", r.URL)
		}
	})
	writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(-time.Hour)), RefreshToken: "secret-refresh", ClientID: "c"})
	_, err := c.Devices(context.Background())
	if !errors.Is(err, ErrConnectLogin) || !strings.Contains(err.Error(), "auth --unofficial") || strings.Contains(err.Error(), "secret-refresh") {
		t.Errorf("err = %v", err)
	}
}

// Another process running this server may have refreshed already, spending
// the refresh token this one holds.
func TestTokenFileIsRereadBeforeRefreshing(t *testing.T) {
	d := &diauth{}
	var bearer string
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) {
		if d.serve(t, w, r) {
			return
		}
		bearer = r.Header.Get("Authorization")
		fmt.Fprint(w, `{}`)
	})
	writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(time.Hour)), RefreshToken: "r0", ClientID: "c"})
	if _, err := c.Devices(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.auth.now = func() time.Time { return testNow.Add(55 * time.Minute) }
	fresh := jwtWith(t, map[string]any{"exp": testNow.Add(5 * time.Hour).Unix(), "other": "process"})
	writeTokens(t, connectTokens{AccessToken: fresh, RefreshToken: "r1", ClientID: "c"})
	if _, err := c.Devices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.forms) != 0 || bearer != "Bearer "+fresh {
		t.Errorf("refreshes = %d, bearer = %q", len(d.forms), bearer)
	}
}

// The other process can also refresh between this one's reread and its own
// refresh, so Garmin refuses the spent refresh token; the tokens it saved
// still work.
func TestRefusedRefreshUsesTokensAnotherProcessSaved(t *testing.T) {
	fresh := jwtWith(t, map[string]any{"exp": testNow.Add(5 * time.Hour).Unix(), "other": "process"})
	refreshes := 0
	var bearer string
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/di-oauth2-service/oauth/token" {
			refreshes++
			if _, err := saveConnectTokens(connectTokens{AccessToken: fresh, RefreshToken: "r1", ClientID: "c"}); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bearer = r.Header.Get("Authorization")
		fmt.Fprint(w, `{}`)
	})
	writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(-time.Hour)), RefreshToken: "r0", ClientID: "c"})
	if _, err := c.Devices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 || bearer != "Bearer "+fresh {
		t.Errorf("refreshes = %d, bearer = %q", refreshes, bearer)
	}
}

// A token file saved before it was sealed, or sealed with a key that is gone,
// asks for a new sign-in rather than failing every request with a crypto error.
func TestUnsealableTokenFileAsksForSignIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{"plaintext", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"di_token":"h.p.s"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"another key", func(t *testing.T, path string) {
			writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(time.Hour))})
			replaceEncryptionKey(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHome(t)
			path, _ := ConnectTokenFile()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			tc.write(t, path)
			if err := CheckConnectSignIn(); !errors.Is(err, ErrConnectLogin) ||
				!strings.Contains(err.Error(), "can't be decrypted") || !strings.Contains(err.Error(), "auth --unofficial") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestErrorBodiesHideTokens(t *testing.T) {
	access := fakeJWT(t, testNow.Add(time.Hour))
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "echo %s and refresh-secret", r.Header.Get("Authorization"))
	})
	writeTokens(t, connectTokens{AccessToken: access, RefreshToken: "refresh-secret", ClientID: "c"})
	_, err := c.Devices(context.Background())
	if err == nil || strings.Contains(err.Error(), access) || strings.Contains(err.Error(), "refresh-secret") {
		t.Errorf("err = %v", err)
	}
}

func TestConnectDomainIsAllowlisted(t *testing.T) {
	t.Setenv(ConnectDomainEnvVar, "garmin.cn")
	c, err := NewConnectClient()
	if err != nil || c.hosts.SSO != "https://sso.garmin.cn" || c.hosts.API != "https://connectapi.garmin.cn" {
		t.Errorf("hosts = %+v, err = %v", c, err)
	}
	for _, bad := range []string{"evil.example", "sso.garmin.com", "GARMIN.COM"} {
		t.Setenv(ConnectDomainEnvVar, bad)
		if _, err := NewConnectClient(); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// signedIn serves the API from routes keyed by path prefix, with a social
// profile, and records each request URI.
func signedIn(t *testing.T, routes map[string]string) (*ConnectClient, *[]string) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []string
	)
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		if r.URL.Path == socialProfilePath {
			fmt.Fprint(w, `{"displayName":"runner/../x?y","profileId":98765}`)
			return
		}
		for prefix, text := range routes {
			if strings.HasPrefix(r.URL.Path, prefix) {
				fmt.Fprint(w, text)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(time.Hour)), RefreshToken: "r", ClientID: "c"})
	return c, &requests
}

func TestConnectEndpoints(t *testing.T) {
	from, to := day(t, "2026-10-01"), day(t, "2026-10-07")
	cases := []struct {
		name string
		call func(context.Context, *ConnectClient) (any, error)
		want []string
	}{
		{"readiness", func(ctx context.Context, c *ConnectClient) (any, error) { return c.TrainingReadiness(ctx, to) },
			[]string{"GET /metrics-service/metrics/trainingreadiness/2026-10-07"}},
		{"status", func(ctx context.Context, c *ConnectClient) (any, error) { return c.TrainingStatus(ctx, to) },
			[]string{"GET /metrics-service/metrics/trainingstatus/aggregated/2026-10-07"}},
		{"vo2max", func(ctx context.Context, c *ConnectClient) (any, error) { return c.MaxMetrics(ctx, from, to) },
			[]string{"GET /metrics-service/metrics/maxmet/daily/2026-10-01/2026-10-07"}},
		{"latest race predictions", func(ctx context.Context, c *ConnectClient) (any, error) { return c.RacePredictions(ctx, nil, nil, "") },
			[]string{"GET /userprofile-service/socialProfile", "GET /metrics-service/metrics/racepredictions/latest/runner%2F..%2Fx%3Fy"}},
		{"race predictions range", func(ctx context.Context, c *ConnectClient) (any, error) {
			return c.RacePredictions(ctx, &from, &to, "monthly")
		}, []string{"GET /userprofile-service/socialProfile",
			"GET /metrics-service/metrics/racepredictions/monthly/runner%2F..%2Fx%3Fy?fromCalendarDate=2026-10-01&toCalendarDate=2026-10-07"}},
		{"endurance", func(ctx context.Context, c *ConnectClient) (any, error) {
			return c.EnduranceScore(ctx, from, to, "weekly")
		},
			[]string{"GET /metrics-service/metrics/endurancescore/stats?aggregation=weekly&endDate=2026-10-07&startDate=2026-10-01"}},
		{"hill", func(ctx context.Context, c *ConnectClient) (any, error) { return c.HillScore(ctx, from, to) },
			[]string{"GET /metrics-service/metrics/hillscore/stats?aggregation=daily&endDate=2026-10-07&startDate=2026-10-01"}},
		{"tolerance", func(ctx context.Context, c *ConnectClient) (any, error) {
			return c.RunningTolerance(ctx, from, to, "daily")
		},
			[]string{"GET /metrics-service/metrics/runningtolerance/stats?aggregation=daily&endDate=2026-10-07&startDate=2026-10-01"}},
		{"lactate", func(ctx context.Context, c *ConnectClient) (any, error) { return c.LactateThreshold(ctx) },
			[]string{"GET /biometric-service/biometric/latestLactateThreshold"}},
		{"ftp", func(ctx context.Context, c *ConnectClient) (any, error) { return c.CyclingFTP(ctx) },
			[]string{"GET /biometric-service/biometric/latestFunctionalThresholdPower/CYCLING"}},
		{"records", func(ctx context.Context, c *ConnectClient) (any, error) { return c.PersonalRecords(ctx) },
			[]string{"GET /userprofile-service/socialProfile", "GET /personalrecord-service/personalrecord/prs/runner%2F..%2Fx%3Fy"}},
		{"respiration", func(ctx context.Context, c *ConnectClient) (any, error) { return c.Respiration(ctx, to) },
			[]string{"GET /wellness-service/wellness/daily/respiration/2026-10-07"}},
		{"stress", func(ctx context.Context, c *ConnectClient) (any, error) { return c.StressTimeline(ctx, to) },
			[]string{"GET /wellness-service/wellness/dailyStress/2026-10-07"}},
		{"floors", func(ctx context.Context, c *ConnectClient) (any, error) { return c.Floors(ctx, to) },
			[]string{"GET /wellness-service/wellness/floorsChartData/daily/2026-10-07"}},
		{"blood pressure", func(ctx context.Context, c *ConnectClient) (any, error) { return c.BloodPressure(ctx, from, to) },
			[]string{"GET /bloodpressure-service/bloodpressure/range/2026-10-01/2026-10-07?includeAll=true"}},
		{"active plans", func(ctx context.Context, c *ConnectClient) (any, error) { return c.ActiveTrainingPlans(ctx, to) },
			[]string{"GET /trainingplan-service/trainingplan/active-plans?startDate=2026-10-07"}},
		{"phased plan", func(ctx context.Context, c *ConnectClient) (any, error) { return c.TrainingPlan(ctx, 42, false) },
			[]string{"GET /trainingplan-service/trainingplan/phased/42"}},
		{"adaptive plan", func(ctx context.Context, c *ConnectClient) (any, error) { return c.TrainingPlan(ctx, 42, true) },
			[]string{"GET /trainingplan-service/trainingplan/fbt-adaptive/42"}},
		{"calendar", func(ctx context.Context, c *ConnectClient) (any, error) {
			return c.CalendarMonth(ctx, 2026, time.January)
		},
			[]string{"GET /calendar-service/year/2026/month/0"}},
		{"workouts", func(ctx context.Context, c *ConnectClient) (any, error) { return c.Workouts(ctx, 20, 10) },
			[]string{"GET /workout-service/workouts?limit=10&start=20"}},
		{"workout", func(ctx context.Context, c *ConnectClient) (any, error) { return c.Workout(ctx, 7) },
			[]string{"GET /workout-service/workout/7"}},
		{"devices", func(ctx context.Context, c *ConnectClient) (any, error) { return c.Devices(ctx) },
			[]string{"GET /device-service/deviceregistration/devices"}},
		{"gear", func(ctx context.Context, c *ConnectClient) (any, error) { return c.Gear(ctx) },
			[]string{"GET /userprofile-service/socialProfile", "GET /gear-service/gear/filterGear?userProfilePk=98765"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, requests := signedIn(t, nil)
			if _, err := tc.call(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*requests, tc.want) {
				t.Errorf("requests = %v, want %v", *requests, tc.want)
			}
		})
	}
}

func TestProfileIsFetchedOnce(t *testing.T) {
	c, requests := signedIn(t, nil)
	ctx := context.Background()
	if _, err := c.PersonalRecords(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Gear(ctx); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(*requests, "\n"), socialProfilePath); n != 1 {
		t.Errorf("social profile fetched %d times: %v", n, *requests)
	}
}

func TestInvalidArgumentsMakeNoRequest(t *testing.T) {
	c, requests := signedIn(t, nil)
	ctx := context.Background()
	from, to := day(t, "2025-01-01"), day(t, "2026-10-07")
	calls := []func() (any, error){
		func() (any, error) { return c.TrainingPlan(ctx, 0, false) },
		func() (any, error) { return c.Workout(ctx, -1) },
		func() (any, error) { return c.CalendarMonth(ctx, 2026, 13) },
		func() (any, error) { return c.RacePredictions(ctx, &from, &to, "daily") },
	}
	for i, call := range calls {
		if _, err := call(); err == nil {
			t.Errorf("call %d succeeded", i)
		}
	}
	// The race predictions check runs after the profile lookup.
	for _, request := range *requests {
		if !strings.Contains(request, socialProfilePath) {
			t.Errorf("unexpected request %s", request)
		}
	}
}

func TestGraphQLQueries(t *testing.T) {
	var queries []string
	c := newConnect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != graphQLPath || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s %s", r.Method, r.URL, r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		var request struct{ Query string }
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		queries = append(queries, request.Query)
		if strings.Contains(request.Query, "trainingPlanScalar") {
			fmt.Fprint(w, `{"data":{"trainingPlanScalar":{"trainingPlanWorkoutScheduleDTOS":[]}}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"workoutScheduleSummariesScalar":null},"errors":[{"message":"boom"}]}`)
	})
	writeTokens(t, connectTokens{AccessToken: fakeJWT(t, testNow.Add(time.Hour)), RefreshToken: "r", ClientID: "c"})
	ctx := context.Background()
	got, err := c.CoachWorkouts(ctx, day(t, "2026-10-07"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(map[string]any)["trainingPlanWorkoutScheduleDTOS"]; !ok {
		t.Errorf("got %v", got)
	}
	if _, err := c.ScheduledWorkouts(ctx, day(t, "2026-10-01"), day(t, "2026-10-07")); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("errors array: err = %v", err)
	}
	want := []string{
		`query{trainingPlanScalar(calendarDate:"2026-10-07",lang:"en-US",firstDayOfWeek:"monday")}`,
		`query{workoutScheduleSummariesScalar(startDate:"2026-10-01",endDate:"2026-10-07")}`,
	}
	if !reflect.DeepEqual(queries, want) {
		t.Errorf("queries = %q", queries)
	}
	if _, err := c.graphQL(ctx, "x", graphQLArg{"a", `2026") { evil }`}); err == nil {
		t.Error("an argument that closes the literal was accepted")
	}
}
