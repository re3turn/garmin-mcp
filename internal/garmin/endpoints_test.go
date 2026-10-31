package garmin

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
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

func day(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(DateLayout, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// recorder answers GETs by a route whose key is a prefix of the path, and 204
// for anything unrouted. Routes is a map, so keep its prefixes from
// overlapping: which of two matching routes answers is not defined.
type recorder struct {
	mu       sync.Mutex
	requests []string
	routes   map[string]func(w http.ResponseWriter)
}

func newRecorder(t *testing.T, routes map[string]func(w http.ResponseWriter)) (*recorder, *Client) {
	t.Helper()
	isolate(t)
	r := &recorder{routes: routes}
	f := newFakeGarmin(t, func(w http.ResponseWriter, req *http.Request, _ string) {
		r.mu.Lock()
		r.requests = append(r.requests, req.URL.RequestURI())
		r.mu.Unlock()
		for prefix, respond := range r.routes {
			if strings.HasPrefix(req.URL.Path, prefix) {
				respond(w)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return r, f.client()
}

func body(text string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.Write([]byte(text)) }
}

func status(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

func TestStatsSplitsLongRangesIntoWindows(t *testing.T) {
	r, c := newRecorder(t, map[string]func(http.ResponseWriter){
		"/usersummary-service/stats/steps/": body(`[{"calendarDate":"x"}]`),
	})
	got, err := c.Stats(context.Background(), StatSteps, day(t, "2026-01-01"), day(t, "2026-03-01"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usersummary-service/stats/steps/daily/2026-01-01/2026-01-28",
		"/usersummary-service/stats/steps/daily/2026-01-29/2026-02-25",
		"/usersummary-service/stats/steps/daily/2026-02-26/2026-03-01",
	}
	if !reflect.DeepEqual(r.requests, want) {
		t.Errorf("requests = %v", r.requests)
	}
	if rows := got.([]any); len(rows) != 3 {
		t.Errorf("rows = %v", rows)
	}
}

func TestDailySummariesConcatenatesData(t *testing.T) {
	r, c := newRecorder(t, map[string]func(http.ResponseWriter){
		"/usersummary-service/v2/dailysummaries": body(`{"data":[{"d":1}]}`),
	})
	got, err := c.DailySummaries(context.Background(), day(t, "2025-01-01"), day(t, "2026-01-05"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usersummary-service/v2/dailysummaries?endDate=2025-12-31&startDate=2025-01-01",
		"/usersummary-service/v2/dailysummaries?endDate=2026-01-05&startDate=2026-01-01",
	}
	if !reflect.DeepEqual(r.requests, want) {
		t.Errorf("requests = %v", r.requests)
	}
	if rows := got.(map[string]any)["data"].([]any); len(rows) != 2 {
		t.Errorf("data = %v", rows)
	}
}

// Each endpoint's documented maximum must fit in one request, and one more day
// must not.
func TestRangeLimitsPerEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		maxDays int
		fetch   func(c *Client, start, end time.Time) (any, error)
	}{
		{"steps", 28, func(c *Client, s, e time.Time) (any, error) { return c.Stats(context.Background(), StatSteps, s, e) }},
		{"stress", 28, func(c *Client, s, e time.Time) (any, error) { return c.Stats(context.Background(), StatStress, s, e) }},
		{"hydration", 31, func(c *Client, s, e time.Time) (any, error) {
			return c.Stats(context.Background(), StatHydration, s, e)
		}},
		{"sleep summaries", 31, func(c *Client, s, e time.Time) (any, error) { return c.SleepSummaries(context.Background(), s, e) }},
		{"body battery", 31, func(c *Client, s, e time.Time) (any, error) { return c.BodyBattery(context.Background(), s, e, nil) }},
		{"daily summaries", 365, func(c *Client, s, e time.Time) (any, error) { return c.DailySummaries(context.Background(), s, e) }},
	}
	start := day(t, "2025-01-01")
	for _, tc := range cases {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s +%d", tc.name, extra), func(t *testing.T) {
				r, c := newRecorder(t, nil)
				if _, err := tc.fetch(c, start, start.AddDate(0, 0, tc.maxDays-1+extra)); err != nil {
					t.Fatal(err)
				}
				if want := 1 + extra; len(r.requests) != want {
					t.Errorf("requests = %d, want %d: %v", len(r.requests), want, r.requests)
				}
			})
		}
	}
}

func TestEndpointPathsAndQueries(t *testing.T) {
	d := day(t, "2026-10-30")
	cases := []struct {
		name string
		call func(c *Client) (any, error)
		want string
	}{
		{"daily summary", func(c *Client) (any, error) { return c.DailySummary(context.Background(), d) },
			"/usersummary-service/usersummary/daily?calendarDate=2026-10-30"},
		{"sleep", func(c *Client) (any, error) { return c.Sleep(context.Background(), d) },
			"/sleep-service/sleep/dailySleep?date=2026-10-30"},
		{"hrv", func(c *Client) (any, error) { return c.HRV(context.Background(), d) },
			"/hrv-service/hrv/2026-10-30"},
		{"hrv summaries", func(c *Client) (any, error) { return c.HRVSummaries(context.Background(), d, d) },
			"/hrv-service/hrv/daily/2026-10-30/2026-10-30"},
		{"heart rate", func(c *Client) (any, error) { return c.HeartRate(context.Background(), d, false) },
			"/wellness-service/wellness/dailyHeartRate?date=2026-10-30"},
		{"spo2", func(c *Client) (any, error) { return c.SpO2(context.Background(), d, false) },
			"/wellness-service/wellness/daily/spo2acclimation/2026-10-30"},
		{"wellness activities", func(c *Client) (any, error) { return c.WellnessActivities(context.Background(), d, d) },
			"/wellnessactivity-service/activity/summary/range/2026-10-30/2026-10-30"},
		{"weight", func(c *Client) (any, error) { return c.Weight(context.Background(), d, d, true) },
			"/weight-service/weight/range/2026-10-30/2026-10-30?includeAll=true"},
		{"weight goals", func(c *Client) (any, error) { return c.WeightGoals(context.Background(), d, d) },
			"/goal-service/goal/user/effective/weightgoal/2026-10-30/2026-10-30"},
		{"food log", func(c *Client) (any, error) {
			return c.FoodLog(context.Background(), d, FoodLogOptions{MealID: new(3), IncludeGoals: true, IncludeNutrition: false, IncludeMedia: true})
		}, "/nutrition-service/food/logs/2026-10-30?includeGoals=true&includeMedia=true&includeNutrition=false&mealId=3"},
		{"menstrual cycle", func(c *Client) (any, error) { return c.MenstrualCycle(context.Background(), d) },
			"/periodichealth-service/menstrualcycle/summary/2026-10-30"},
		{"menstrual day", func(c *Client) (any, error) {
			return c.MenstrualDay(context.Background(), d, MenstrualDayOptions{Today: day(t, "2026-10-31"), Reduced: true, SkipBabyMovement: true})
		}, "/periodichealth-service/menstrualcycle/dayview/2026-10-30?includeAnalysis=false&reducedResponse=true&skipbm=true&todayDate=2026-10-31"},
		{"activity", func(c *Client) (any, error) { return c.Activity(context.Background(), 21234567890) },
			"/activity-service/activity/21234567890"},
		{"activity workouts", func(c *Client) (any, error) { return c.ActivityWorkouts(context.Background(), 7) },
			"/activity-service/activity/7/workouts"},
		{"profile", func(c *Client) (any, error) { return c.ProfileSettings(context.Background()) },
			"/userprofile-service/userprofile/settings"},
		{"heart rate zones", func(c *Client) (any, error) { return c.HeartRateZones(context.Background(), false) },
			"/biometric-service/heartRateZones/v2?generateDefaults=false"},
		{"fitness age", func(c *Client) (any, error) { return c.FitnessAge(context.Background(), d, true) },
			"/fitnessage-service/fitnessage/v2/2026-10-30?fullHistoryScan=true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := newRecorder(t, nil)
			if _, err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			if len(r.requests) != 1 || r.requests[0] != tc.want {
				t.Errorf("requests = %v, want [%s]", r.requests, tc.want)
			}
		})
	}
}

func TestSearchActivitiesFavoriteFalseAndDecoDive(t *testing.T) {
	r, c := newRecorder(t, nil)
	_, err := c.SearchActivities(context.Background(), ActivitySearch{Favorite: new(false), DecoDive: new(true), Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"favorite=0", "decoDive=true", "limit=5"} {
		if !strings.Contains(r.requests[0], want) {
			t.Errorf("query %s lacks %s", r.requests[0], want)
		}
	}
}

func TestNoContentWindowsYieldEmptyList(t *testing.T) {
	_, c := newRecorder(t, nil)
	got, err := c.SleepSummaries(context.Background(), day(t, "2026-10-01"), day(t, "2026-10-07"))
	if err != nil {
		t.Fatal(err)
	}
	if rows := got.([]any); rows == nil || len(rows) != 0 {
		t.Errorf("got %#v, want an empty non-nil list", got)
	}
}

func TestSamplesAreDroppedUnlessRequested(t *testing.T) {
	_, c := newRecorder(t, map[string]func(http.ResponseWriter){
		"/wellness-service/wellness/dailyHeartRate":    body(`{"restingHeartRate":50,"heartRateValues":[[1,2]],"heartRateValueDescriptors":[]}`),
		"/wellness-service/wellness/bodyBattery/":      body(`[{"charged":40,"bodyBatteryValuesArray":[[1,2]],"bodyBatteryValueDescriptorDTOList":[]}]`),
		"/wellness-service/wellness/daily/spo2acclima": body(`{"averageSpO2":97,"spO2SingleValues":[1],"monitoringEnvironmentValues":[[1,2]]}`),
	})
	ctx := context.Background()
	today := day(t, "2026-10-31")

	hr, _ := c.HeartRate(ctx, today, false)
	if _, ok := hr.(map[string]any)["heartRateValues"]; ok {
		t.Error("heart rate samples kept")
	}
	hr, _ = c.HeartRate(ctx, today, true)
	if _, ok := hr.(map[string]any)["heartRateValues"]; !ok {
		t.Error("heart rate samples dropped despite include_samples")
	}
	spo2, _ := c.SpO2(ctx, today, false)
	if _, ok := spo2.(map[string]any)["monitoringEnvironmentValues"]; ok {
		t.Error("SpO2 monitoring environment samples kept")
	}
	if _, ok := spo2.(map[string]any)["spO2SingleValues"]; ok {
		t.Error("SpO2 samples kept")
	}
	bb, _ := c.BodyBattery(ctx, today, today, nil)
	if _, ok := bb.([]any)[0].(map[string]any)["bodyBatteryValuesArray"]; ok {
		t.Error("body battery samples kept without chunks")
	}
	chunks := 24
	bb, _ = c.BodyBattery(ctx, today, today, &chunks)
	if _, ok := bb.([]any)[0].(map[string]any)["bodyBatteryValuesArray"]; !ok {
		t.Error("body battery chunks dropped")
	}
}

func TestSearchActivitiesQuery(t *testing.T) {
	r, c := newRecorder(t, map[string]func(http.ResponseWriter){"/activitylist-service/": body(`[]`)})
	start := day(t, "2026-10-01")
	favorite := true
	minDistance := 5000
	box, err := NewBoundingBox(new(35.7), new(35.6), new(139.8), new(139.7))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SearchActivities(context.Background(), ActivitySearch{
		Start:            &start,
		ActivityType:     "running",
		ExcludedSubtypes: []string{"treadmill_running", "indoor_running"},
		Favorite:         &favorite,
		ExcludeChildren:  true,
		MinDistance:      &minDistance,
		StartArea:        box,
		RequiredFields:   []string{"avgPower"},
		Limit:            10,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := r.requests[0]
	for _, want := range []string{
		"activityType=running", "startDate=2026-10-01", "favorite=1", "excludeChildren=true",
		"excludedActivitySubType=treadmill_running&excludedActivitySubType=indoor_running",
		"minDistance=5000", "upperLatitude=35.7", "leftLongitude=139.7", "requiredField=avgPower",
		"limit=10", "start=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("query %s lacks %s", got, want)
		}
	}
	if strings.Contains(got, "endDate") {
		t.Errorf("query %s has an endDate nobody set", got)
	}
}

func TestBoundingBoxNeedsAllEdges(t *testing.T) {
	if box, err := NewBoundingBox(nil, nil, nil, nil); box != nil || err != nil {
		t.Errorf("no edges: %v, %v", box, err)
	}
	if _, err := NewBoundingBox(new(1.0), nil, nil, nil); err == nil {
		t.Error("one edge accepted")
	}
}

func TestDayToleratesMissingSections(t *testing.T) {
	_, c := newRecorder(t, map[string]func(http.ResponseWriter){
		"/usersummary-service/usersummary/daily": body(`{"totalSteps":1000}`),
		"/hrv-service/":                          status(http.StatusNotFound),
		"/activitylist-service/":                 body(`[]`),
	})
	got, err := c.Day(context.Background(), day(t, "2026-10-31"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["hrv"].(map[string]any)["error"]; !ok {
		t.Errorf("hrv = %v, want an error section", got["hrv"])
	}
	if got["summary"].(map[string]any)["totalSteps"] == nil {
		t.Errorf("summary = %v", got["summary"])
	}
	if _, ok := got["sleep"].(map[string]any)["no_data"]; !ok {
		t.Errorf("sleep = %v, want no_data for Garmin's 204", got["sleep"])
	}
}

// hangUp drops the connection without answering, as an unreachable or
// failing network would.
func hangUp(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func TestDayFailsWhenGarminDropsTheConnection(t *testing.T) {
	_, c := newRecorder(t, map[string]func(http.ResponseWriter){"/": hangUp})
	_, err := c.Day(context.Background(), day(t, "2026-10-31"))
	var transportErr *url.Error
	if !errors.As(err, &transportErr) {
		t.Fatalf("err = %v, want a transport error", err)
	}
}

func TestDayFailsWhenTheKeyIsRejected(t *testing.T) {
	_, c := newRecorder(t, nil)
	t.Setenv(KeyEnvVar, "rejected-key")
	if _, err := c.Day(context.Background(), day(t, "2026-10-31")); !errors.Is(err, ErrAPIKey) {
		t.Fatalf("err = %v, want a key error", err)
	}
}

func TestDayFailsWhenNoTokenCanBeObtained(t *testing.T) {
	isolate(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if _, err := NewClient(WithBaseURL(server.URL)).Day(context.Background(), day(t, "2026-10-31")); !isTokenError(err) {
		t.Fatalf("err = %v, want the token error instead of six error sections", err)
	}
}

func TestDayPropagatesUnauthorized(t *testing.T) {
	_, c := newRecorder(t, map[string]func(http.ResponseWriter){"/": status(http.StatusUnauthorized)})
	if _, err := c.Day(context.Background(), day(t, "2026-10-31")); !IsUnauthorized(err) {
		t.Fatalf("err = %v, want 401", err)
	}
}

func TestDayPropagatesCancellation(t *testing.T) {
	_, c := newRecorder(t, nil)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Day(cancelled, day(t, "2026-10-31")); err == nil {
		t.Fatal("Day with a cancelled context returned no error")
	}
}

func TestStatsRejectsUnknownStat(t *testing.T) {
	_, c := newRecorder(t, nil)
	if _, err := c.Stats(context.Background(), Stat("unknown"), day(t, "2026-10-01"), day(t, "2026-10-31")); err == nil {
		t.Fatal("unknown stat accepted")
	}
}

func TestDownloadActivitySavesZip(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	if _, err := writer.Create("123_ACTIVITY.fit"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	_, c := newRecorder(t, map[string]func(http.ResponseWriter){"/download-service/": body(archive.String())})
	dir := t.TempDir()
	got, err := c.DownloadActivity(context.Background(), 123, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != filepath.Join(dir, "activity-123.zip") || !reflect.DeepEqual(got.Files, []string{"123_ACTIVITY.fit"}) {
		t.Errorf("got %+v", got)
	}
	saved, err := os.ReadFile(got.Path)
	if err != nil || !bytes.Equal(saved, archive.Bytes()) {
		t.Errorf("saved file differs: %v", err)
	}
}

func TestDownloadDirExpandsHome(t *testing.T) {
	home := setHome(t)
	t.Setenv(DownloadDirEnvVar, "~/garmin-files")
	if got, err := DownloadDir(); err != nil || got != filepath.Join(home, "garmin-files") {
		t.Errorf("~/garmin-files: %q, %v", got, err)
	}
	t.Setenv(DownloadDirEnvVar, "")
	if got, err := DownloadDir(); err != nil || got != filepath.Join(home, "Downloads", "garmin") {
		t.Errorf("default: %q, %v", got, err)
	}
	t.Setenv(DownloadDirEnvVar, "downloads")
	if got, err := DownloadDir(); err == nil {
		t.Errorf("relative path accepted as %q", got)
	}
}

func TestDownloadTargetStaysInsideDownloadDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv(DownloadDirEnvVar, base)
	if got, err := DownloadTarget("races"); err != nil || got != filepath.Join(base, "races") {
		t.Errorf("races: %q, %v", got, err)
	}
	for _, bad := range []string{"..", "../outside", "a/../../b", filepath.Join(base, "abs")} {
		if _, err := DownloadTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
