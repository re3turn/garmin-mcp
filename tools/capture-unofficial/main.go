// Command capture-unofficial records the shape of each unofficial Garmin
// Connect API response the --unofficial tools read.
//
// Garmin publishes no spec for this API, so the only record of what it returns
// is a real response. This command signs in with the tokens `garmin-mcp auth
// --unofficial` saved, calls each endpoint once, and writes one file per tool
// with every value replaced by its type (see shape), so the files hold field
// names and no account data. Every request is a read.
//
// Usage:
//
//	go run ./tools/capture-unofficial/ [-date YYYY-MM-DD] [-o api/unofficial] [-only TOOL]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/re3turn/garmin-mcp/internal/garmin"
)

// pause spaces the requests out, as a person browsing the app would.
const pause = time.Second

type capture struct {
	name string
	read func(context.Context) (any, error)
}

func main() {
	date := flag.String("date", "", "day to read, YYYY-MM-DD (default: yesterday, which has a full day of data)")
	out := flag.String("o", filepath.Join("api", "unofficial"), "output directory")
	only := flag.String("only", "", "record just this tool, such as garmin_get_scheduled_workouts")
	flag.Parse()

	day := time.Now().AddDate(0, 0, -1)
	if *date != "" {
		parsed, err := time.ParseInLocation(time.DateOnly, *date, time.Local)
		if err != nil {
			fmt.Fprintln(os.Stderr, "capture-unofficial: -date must be YYYY-MM-DD")
			os.Exit(2)
		}
		day = parsed
	}
	if err := run(day, *out, *only); err != nil {
		fmt.Fprintln(os.Stderr, "capture-unofficial:", err)
		os.Exit(1)
	}
}

func run(day time.Time, out, only string) error {
	if err := garmin.CheckConnectSignIn(); err != nil {
		return err
	}
	c, err := garmin.NewConnectClient()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	ctx := context.Background()

	var failed []string
	results := map[string]any{}
	requests := 0
	for _, item := range captureList(c, day) {
		if only != "" && item.name != only {
			continue
		}
		if requests > 0 {
			time.Sleep(pause)
		}
		requests++
		value, err := item.read(ctx)
		if !record(out, item.name, value, err) {
			failed = append(failed, item.name)
			continue
		}
		results[item.name] = value
	}

	if only != "" {
		if len(results) == 0 && len(failed) == 0 {
			return fmt.Errorf("-only %s: no such capture (the plan and workout details need the full run)", only)
		}
		if len(failed) > 0 {
			return fmt.Errorf("%s failed", only)
		}
		return nil
	}

	// The plan and workout tools need an id, taken from the lists above.
	if id, ok := smallestID(results["garmin_get_active_training_plans"], "trainingPlanId"); ok {
		time.Sleep(pause)
		value, err := c.TrainingPlan(ctx, id, false)
		// A Garmin Coach plan lives under the adaptive path instead.
		if err != nil || value == nil {
			time.Sleep(pause)
			value, err = c.TrainingPlan(ctx, id, true)
		}
		if !record(out, "garmin_get_training_plan", value, err) {
			failed = append(failed, "garmin_get_training_plan")
		}
	} else {
		fmt.Println("skip garmin_get_training_plan: no active training plan")
	}
	if id, ok := smallestID(results["garmin_list_workouts"], "workoutId"); ok {
		time.Sleep(pause)
		value, err := c.Workout(ctx, id)
		if !record(out, "garmin_get_workout", value, err) {
			failed = append(failed, "garmin_get_workout")
		}
	} else {
		fmt.Println("skip garmin_get_workout: no saved workout")
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d request(s) failed: %v", len(failed), failed)
	}
	return nil
}

// captureList is one request per unofficial tool, or per mode for a tool with
// several, named tool[.mode].
func captureList(c *garmin.ConnectClient, day time.Time) []capture {
	week := day.AddDate(0, 0, -6)
	month := day.AddDate(0, 0, -29)
	ahead := day.AddDate(0, 0, 30)
	return []capture{
		{"garmin_get_training_readiness", func(ctx context.Context) (any, error) { return c.TrainingReadiness(ctx, day) }},
		{"garmin_get_training_status", func(ctx context.Context) (any, error) { return c.TrainingStatus(ctx, day) }},
		{"garmin_get_vo2max", func(ctx context.Context) (any, error) { return c.MaxMetrics(ctx, week, day) }},
		{"garmin_get_race_predictions.latest", func(ctx context.Context) (any, error) { return c.RacePredictions(ctx, nil, nil, "") }},
		{"garmin_get_race_predictions.daily", func(ctx context.Context) (any, error) { return c.RacePredictions(ctx, &week, &day, "daily") }},
		{"garmin_get_endurance_score", func(ctx context.Context) (any, error) { return c.EnduranceScore(ctx, month, day, "weekly") }},
		{"garmin_get_hill_score", func(ctx context.Context) (any, error) { return c.HillScore(ctx, week, day) }},
		{"garmin_get_running_tolerance", func(ctx context.Context) (any, error) { return c.RunningTolerance(ctx, month, day, "weekly") }},
		{"garmin_get_lactate_threshold", c.LactateThreshold},
		{"garmin_get_cycling_ftp", c.CyclingFTP},
		{"garmin_get_personal_records", c.PersonalRecords},
		{"garmin_get_respiration", func(ctx context.Context) (any, error) { return c.Respiration(ctx, day) }},
		{"garmin_get_stress_timeline", func(ctx context.Context) (any, error) { return c.StressTimeline(ctx, day) }},
		{"garmin_get_floors", func(ctx context.Context) (any, error) { return c.Floors(ctx, day) }},
		{"garmin_get_blood_pressure", func(ctx context.Context) (any, error) { return c.BloodPressure(ctx, month, day) }},
		{"garmin_get_active_training_plans", func(ctx context.Context) (any, error) { return c.ActiveTrainingPlans(ctx, day) }},
		{"garmin_get_coach_workouts", func(ctx context.Context) (any, error) { return c.CoachWorkouts(ctx, day) }},
		// Scheduled workouts are mostly ahead of the day, so the window reaches forward.
		{"garmin_get_scheduled_workouts", func(ctx context.Context) (any, error) { return c.ScheduledWorkouts(ctx, week, ahead) }},
		{"garmin_get_calendar_month", func(ctx context.Context) (any, error) { return c.CalendarMonth(ctx, day.Year(), day.Month()) }},
		{"garmin_list_workouts", func(ctx context.Context) (any, error) { return c.Workouts(ctx, 0, 20) }},
		{"garmin_get_devices", c.Devices},
		{"garmin_get_gear", c.Gear},
	}
}

// record writes the shape of one response and reports whether it could.
func record(out, name string, value any, err error) bool {
	if err != nil {
		fmt.Printf("fail %s: %s\n", name, safeError(err))
		return false
	}
	data, err := json.MarshalIndent(shape(value), "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(out, name+".json"), append(data, '\n'), 0o644)
	}
	if err != nil {
		fmt.Printf("fail %s: %v\n", name, err)
		return false
	}
	fmt.Println("ok  ", name)
	return true
}

// safeError describes a failed request without account data: a response body,
// a URL (whose path can hold the display name and query the profile id), or a
// GraphQL error message can each echo it.
func safeError(err error) string {
	if httpErr, ok := errors.AsType[*garmin.HTTPError](err); ok {
		return fmt.Sprintf("HTTP %d", httpErr.StatusCode)
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Sprintf("couldn't reach Garmin: %v", urlErr.Err)
	}
	// Written to be shown, never holding a token or account data.
	if errors.Is(err, garmin.ErrConnectLogin) {
		return err.Error()
	}
	return "the response couldn't be read (details withheld, as they can hold account data)"
}

// smallestID finds the smallest positive numeric value of key anywhere in a
// response, so a run picks the same plan or workout every time.
func smallestID(value any, key string) (int64, bool) {
	var best int64
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if n, ok := v[key].(json.Number); ok {
				if id, err := n.Int64(); err == nil && id > 0 && (best == 0 || id < best) {
					best = id
				}
			}
			for _, item := range v {
				walk(item)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(value)
	return best, best > 0
}
