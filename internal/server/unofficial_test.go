package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/re3turn/garmin-mcp/internal/garmin"
)

const unofficialToolCount = 23

// connectUnofficial is connect with the unofficial tools added, against a fake
// connectapi that a saved sign-in reaches.
func connectUnofficial(t *testing.T, remote bool, handle func(w http.ResponseWriter, r *http.Request)) (*mcp.ClientSession, *[]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(garmin.KeyEnvVar, "test-key")

	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(24*time.Hour).Unix()))
	path, err := garmin.ConnectTokenFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	tokens := fmt.Sprintf(`{"di_token":"h.%s.s","di_refresh_token":"r","di_client_id":"c"}`, payload)
	if err := os.WriteFile(path, []byte(tokens), 0o600); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		requests []string
	)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(fake.Close)
	hosts := garmin.ConnectHosts{SSO: fake.URL, API: fake.URL, DIAuth: fake.URL, Connect: fake.URL, MobileIntegration: fake.URL}
	connect, err := garmin.NewConnectClient(garmin.WithConnectHosts(hosts))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{client: garmin.NewClient(garmin.WithBaseURL(fake.URL)), today: func() time.Time { return testToday },
		remote: remote, connect: connect}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.mcpServer("test").Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, &requests
}

func TestUnofficialToolsAreAddedOnlyWhenAsked(t *testing.T) {
	session, _ := connectUnofficial(t, false, func(http.ResponseWriter, *http.Request) {})
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := 26 + unofficialToolCount; len(list.Tools) != want {
		t.Errorf("tools = %d, want %d", len(list.Tools), want)
	}
	required := map[string]string{"garmin_get_training_plan": "plan_id", "garmin_get_workout": "workout_id"}
	unofficial := 0
	for _, tool := range list.Tools {
		if !strings.Contains(tool.Description, "Unofficial Garmin Connect API") {
			continue
		}
		unofficial++
		if !strings.HasSuffix(tool.Title, "(unofficial)") || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s: title %q, annotations %+v", tool.Name, tool.Title, tool.Annotations)
		}
		got, _ := tool.InputSchema.(map[string]any)["required"].([]any)
		var want []any
		if name, ok := required[tool.Name]; ok {
			want = []any{name}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s requires %v, want %v", tool.Name, got, want)
		}
	}
	if unofficial != unofficialToolCount {
		t.Errorf("unofficial tools = %d, want %d", unofficial, unofficialToolCount)
	}
}

// Every unofficial tool only reads, so HTTP mode serves all of them when
// they are enabled.
func TestRemoteServesTheUnofficialTools(t *testing.T) {
	session, _ := connectUnofficial(t, true, func(http.ResponseWriter, *http.Request) {})
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := 25 + unofficialToolCount; len(list.Tools) != want {
		t.Errorf("tools = %d, want %d", len(list.Tools), want)
	}
}

func TestUnofficialArgumentsReachGarmin(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"garmin_get_training_readiness", nil, "GET /metrics-service/metrics/trainingreadiness/2026-10-31"},
		{"garmin_get_vo2max", map[string]any{"end": "2026-10-10"}, "GET /metrics-service/metrics/maxmet/daily/2026-10-04/2026-10-10"},
		{"garmin_get_endurance_score", nil, "aggregation=weekly"},
		{"garmin_get_running_tolerance", map[string]any{"aggregation": "Daily"}, "aggregation=daily"},
		{"garmin_get_race_predictions", nil, "GET /metrics-service/metrics/racepredictions/latest/runner"},
		{"garmin_get_race_predictions", map[string]any{"start": "2026-10-01"}, "/racepredictions/daily/runner?fromCalendarDate=2026-10-01&toCalendarDate=2026-10-31"},
		{"garmin_get_calendar_month", nil, "GET /calendar-service/year/2026/month/9"},
		{"garmin_get_calendar_month", map[string]any{"year": 2027, "month": 1}, "GET /calendar-service/year/2027/month/0"},
		{"garmin_list_workouts", nil, "GET /workout-service/workouts?limit=20&start=0"},
		{"garmin_get_training_plan", map[string]any{"plan_id": 5, "adaptive": true}, "GET /trainingplan-service/trainingplan/fbt-adaptive/5"},
		{"garmin_get_scheduled_workouts", nil, "POST /graphql-gateway/graphql"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			session, requests := connectUnofficial(t, false, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/userprofile-service/socialProfile" {
					fmt.Fprint(w, `{"displayName":"runner","profileId":1}`)
					return
				}
				fmt.Fprint(w, `{"data":{}}`)
			})
			result, _ := call(t, session, tc.tool, tc.args)
			if result.IsError {
				t.Fatalf("error: %v", result.Content[0].(*mcp.TextContent).Text)
			}
			last := requestAt(t, *requests, len(*requests)-1)
			if !strings.Contains(last, tc.want) {
				t.Errorf("request = %q, want it to contain %q", last, tc.want)
			}
		})
	}
}

func TestUnofficialBadInputsAreToolErrors(t *testing.T) {
	session, requests := connectUnofficial(t, false, func(http.ResponseWriter, *http.Request) {})
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"garmin_get_endurance_score", map[string]any{"aggregation": "monthly"}, "daily, weekly"},
		{"garmin_get_race_predictions", map[string]any{"end": "2026-10-01"}, "needs a start"},
		{"garmin_get_race_predictions", map[string]any{"start": "2026-10-01", "aggregation": "weekly"}, "daily, monthly"},
		{"garmin_list_workouts", map[string]any{"limit": 500}, "limit must be"},
		{"garmin_get_calendar_month", map[string]any{"month": 13}, "month 1-12"},
	}
	for _, tc := range cases {
		result, _ := call(t, session, tc.tool, tc.args)
		text := result.Content[0].(*mcp.TextContent).Text
		if !result.IsError || !strings.Contains(text, tc.want) {
			t.Errorf("%s %v: %q", tc.tool, tc.args, text)
		}
	}
	if len(*requests) != 0 {
		t.Errorf("Garmin received %v", *requests)
	}
}
