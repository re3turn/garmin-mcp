package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/re3turn/garmin-personal-mcp/internal/garmin"
)

var testToday = time.Date(2026, 10, 31, 9, 0, 0, 0, time.Local)

// connect serves the tools against a fake Garmin whose GETs are answered by
// handle, and records each request URI.
func connect(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*mcp.ClientSession, *[]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(garmin.KeyEnvVar, "test-key")

	var (
		mu       sync.Mutex
		requests []string
	)
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(24*time.Hour).Unix()))
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			fmt.Fprint(w, "h."+payload+".s")
			return
		}
		mu.Lock()
		requests = append(requests, r.URL.RequestURI())
		mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(fake.Close)

	s := &Server{client: garmin.NewClient(garmin.WithBaseURL(fake.URL)), today: func() time.Time { return testToday }}
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

func call(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var decoded map[string]any
	if !result.IsError {
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			t.Fatalf("%s returned non-object %s", name, text)
		}
	}
	return result, decoded
}

func TestToolsAreListedWithOptionalArguments(t *testing.T) {
	session, _ := connect(t, func(http.ResponseWriter, *http.Request) {})
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 26 {
		t.Errorf("tools = %d, want 26", len(list.Tools))
	}
	for _, tool := range list.Tools {
		schema := tool.InputSchema.(map[string]any)
		required, _ := schema["required"].([]any)
		if strings.Contains(tool.Name, "activity") && slices.Contains(required, any("activity_id")) {
			continue
		}
		if len(required) > 0 {
			t.Errorf("%s requires %v", tool.Name, required)
		}
		if tool.Annotations == nil || tool.Description == "" {
			t.Errorf("%s lacks annotations or a description", tool.Name)
		}
	}
}

func TestEmbeddedInputsAreFlattened(t *testing.T) {
	session, _ := connect(t, func(http.ResponseWriter, *http.Request) {})
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if tool.Name != "garmin_get_body_battery" {
			continue
		}
		properties := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
		for _, name := range []string{"start", "end", "chunks"} {
			property, ok := properties[name].(map[string]any)
			if !ok {
				t.Errorf("body battery schema lacks %s: %v", name, properties)
				continue
			}
			if description, _ := property["description"].(string); description == "" {
				t.Errorf("%s has no description: %v", name, property)
			}
		}
	}
}

func TestRangeDefaultsToSevenDaysEndingToday(t *testing.T) {
	session, requests := connect(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `[]`) })
	result, got := call(t, session, "garmin_get_steps", nil)
	if result.IsError {
		t.Fatal(result.Content[0].(*mcp.TextContent).Text)
	}
	if want := "/usersummary-service/stats/steps/daily/2026-10-25/2026-10-31"; (*requests)[0] != want {
		t.Errorf("request = %s, want %s", (*requests)[0], want)
	}
	if results, ok := got["results"].([]any); !ok || len(results) != 0 {
		t.Errorf("got %v, want an empty results list", got)
	}
}

func TestNoContentExplainsItself(t *testing.T) {
	session, _ := connect(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	_, got := call(t, session, "garmin_get_hrv", map[string]any{"date": "2026-10-30"})
	if _, ok := got["no_data"]; !ok {
		t.Errorf("got %v, want no_data", got)
	}
}

func TestBadInputsAreToolErrors(t *testing.T) {
	session, requests := connect(t, func(http.ResponseWriter, *http.Request) {})
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"garmin_get_sleep", map[string]any{"date": "10/30/2026"}, "YYYY-MM-DD"},
		{"garmin_get_sleep", map[string]any{"date": "2026-02-30"}, "not a valid date"},
		{"garmin_get_steps", map[string]any{"start": "2026-10-31", "end": "2026-10-01"}, "swap them"},
		{"garmin_get_steps", map[string]any{"start": "2026-11-05"}, "after today"},
		{"garmin_search_activities", map[string]any{"north": 35.7}, "all four edges"},
		{"garmin_download_activity", map[string]any{"activity_id": 1, "directory": "../x"}, "subfolder"},
	}
	for _, tc := range cases {
		result, _ := call(t, session, tc.tool, tc.args)
		text := result.Content[0].(*mcp.TextContent).Text
		if !result.IsError || !strings.Contains(text, tc.want) {
			t.Errorf("%s: IsError=%v text=%q, want %q", tc.tool, result.IsError, text, tc.want)
		}
	}
	if len(*requests) != 0 {
		t.Errorf("bad inputs reached Garmin: %v", *requests)
	}
}

func TestSearchActivitiesDefaults(t *testing.T) {
	session, requests := connect(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `[]`) })
	call(t, session, "garmin_search_activities", map[string]any{"start": "2026-10-01"})
	got := (*requests)[0]
	for _, want := range []string{"limit=10", "start=0", "excludeChildren=true", "startDate=2026-10-01", "endDate=2026-10-31"} {
		if !strings.Contains(got, want) {
			t.Errorf("query %s lacks %s", got, want)
		}
	}
}

func TestSearchActivitiesEndAloneHasNoStart(t *testing.T) {
	session, requests := connect(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `[]`) })
	call(t, session, "garmin_search_activities", map[string]any{"end": "2024-01-01"})
	got := (*requests)[0]
	if !strings.Contains(got, "endDate=2024-01-01") || strings.Contains(got, "startDate") {
		t.Errorf("query = %s, want endDate only", got)
	}
}

func TestDefaultTrueBooleans(t *testing.T) {
	session, requests := connect(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{}`) })
	call(t, session, "garmin_get_food_log", map[string]any{"date": "2026-10-30"})
	call(t, session, "garmin_get_heart_rate_zones", map[string]any{"generate_defaults": false})
	if got := (*requests)[0]; !strings.Contains(got, "includeGoals=true") || !strings.Contains(got, "includeNutrition=true") {
		t.Errorf("food log query = %s", got)
	}
	if got := (*requests)[1]; !strings.Contains(got, "generateDefaults=false") {
		t.Errorf("zones query = %s", got)
	}
}
