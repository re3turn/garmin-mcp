package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tools for data the Personal API doesn't have, read from the unofficial
// Garmin Connect API. They are registered only with --unofficial. Data both
// APIs serve stays on the Personal API tools.

const (
	unofficialNote = " Unofficial Garmin Connect API: Garmin can change or break it without notice."
	maxWorkouts    = 100
)

type aggregationInput struct {
	rangeInput
	Aggregation string `json:"aggregation,omitempty" jsonschema:"daily or weekly (default weekly)"`
}

type raceInput struct {
	Start       string `json:"start,omitempty" jsonschema:"first day, YYYY-MM-DD; leave start and end out for the latest predictions"`
	End         string `json:"end,omitempty" jsonschema:"last day, YYYY-MM-DD (default: today when start is set); at most 366 days after start"`
	Aggregation string `json:"aggregation,omitempty" jsonschema:"with a range: daily or monthly (default daily)"`
}

type planInput struct {
	PlanID   int64 `json:"plan_id" jsonschema:"a plan's id from garmin_get_active_training_plans"`
	Adaptive bool  `json:"adaptive,omitempty" jsonschema:"true for a Garmin Coach adaptive plan, whose details live under a different path; if one setting returns an error, try the other"`
}

type monthInput struct {
	Year  *int `json:"year,omitempty" jsonschema:"four-digit year (default: this year)"`
	Month *int `json:"month,omitempty" jsonschema:"1-12 (default: this month)"`
}

type workoutsInput struct {
	Limit  *int `json:"limit,omitempty" jsonschema:"page size (default 20, at most 100)"`
	Offset *int `json:"offset,omitempty" jsonschema:"zero-based index of the first workout (default 0)"`
}

type workoutInput struct {
	WorkoutID int64 `json:"workout_id" jsonschema:"workoutId from garmin_list_workouts, garmin_get_scheduled_workouts, or garmin_get_coach_workouts"`
}

func (s *Server) addUnofficialTools(server *mcp.Server) {
	s.addUnofficialTrainingTools(server)
	s.addUnofficialHealthTools(server)
	s.addUnofficialPlanTools(server)
	s.addUnofficialGearTools(server)
}

func unofficialTool(name, title, description string) *mcp.Tool {
	return &mcp.Tool{Name: name, Title: title + " (unofficial)", Annotations: readsGarmin, Description: description + unofficialNote}
}

func (s *Server) dayTool(server *mcp.Server, name, title, description string, read func(context.Context, time.Time) (any, error)) {
	addTool(server, unofficialTool(name, title, description), func(ctx context.Context, in dayInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		return read(ctx, day)
	})
}

func (s *Server) rangeTool(server *mcp.Server, name, title, description string, read func(context.Context, time.Time, time.Time) (any, error)) {
	addTool(server, unofficialTool(name, title, description+" "+rangeDoc), func(ctx context.Context, in rangeInput) (any, error) {
		first, last, err := s.parseRange(in.Start, in.End)
		if err != nil {
			return nil, err
		}
		return read(ctx, first, last)
	})
}

func (s *Server) aggregatedTool(server *mcp.Server, name, title, description string, read func(context.Context, time.Time, time.Time, string) (any, error)) {
	addTool(server, unofficialTool(name, title, description+" "+rangeDoc), func(ctx context.Context, in aggregationInput) (any, error) {
		first, last, err := s.parseRange(in.Start, in.End)
		if err != nil {
			return nil, err
		}
		aggregation, err := oneOf("aggregation", in.Aggregation, "weekly", "daily", "weekly")
		if err != nil {
			return nil, err
		}
		return read(ctx, first, last, aggregation)
	})
}

func noInputTool(server *mcp.Server, name, title, description string, read func(context.Context) (any, error)) {
	addTool(server, unofficialTool(name, title, description), func(ctx context.Context, _ struct{}) (any, error) {
		return read(ctx)
	})
}

// oneOf checks a value that goes into a Garmin URL against the ones Garmin
// accepts.
func oneOf(name, value, fallback string, allowed ...string) (string, error) {
	if value == "" {
		return fallback, nil
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if !slices.Contains(allowed, value) {
		return "", fmt.Errorf("%s must be one of %s, got %q", name, strings.Join(allowed, ", "), value)
	}
	return value, nil
}

func (s *Server) addUnofficialTrainingTools(server *mcp.Server) {
	c := s.connect
	s.dayTool(server, "garmin_get_training_readiness", "Get training readiness",
		"Training readiness for a day: the score, Garmin's feedback, and the factors behind it. A day can "+
			"have several entries; the one taken after waking has inputContext AFTER_WAKEUP_RESET.",
		c.TrainingReadiness)
	s.dayTool(server, "garmin_get_training_status", "Get training status",
		"Training status as of a day (such as productive or detraining), with training load and VO2 max as "+
			"Garmin reports them per device.", c.TrainingStatus)
	s.rangeTool(server, "garmin_get_vo2max", "Get VO2 max",
		"Daily VO2 max estimates over a date range.", c.MaxMetrics)
	addTool(server, unofficialTool("garmin_get_race_predictions", "Get race predictions",
		"Predicted 5K, 10K, half marathon, and marathon times. Without start and end it returns the latest "+
			"predictions; with them, the daily or monthly predictions over the range."),
		func(ctx context.Context, in raceInput) (any, error) {
			if in.Start == "" && in.End == "" {
				return c.RacePredictions(ctx, nil, nil, "")
			}
			if in.Start == "" {
				return nil, fmt.Errorf("end needs a start; leave both out for the latest predictions")
			}
			first, last, err := s.parseRange(in.Start, in.End)
			if err != nil {
				return nil, err
			}
			aggregation, err := oneOf("aggregation", in.Aggregation, "daily", "daily", "monthly")
			if err != nil {
				return nil, err
			}
			return c.RacePredictions(ctx, &first, &last, aggregation)
		})
	s.aggregatedTool(server, "garmin_get_endurance_score", "Get endurance score",
		"Endurance score over a range, aggregated daily or weekly.", c.EnduranceScore)
	s.rangeTool(server, "garmin_get_hill_score", "Get hill score",
		"Daily hill score over a range.", c.HillScore)
	s.aggregatedTool(server, "garmin_get_running_tolerance", "Get running tolerance",
		"Running tolerance over a range, aggregated daily or weekly.", c.RunningTolerance)
	noInputTool(server, "garmin_get_lactate_threshold", "Get lactate threshold",
		"The latest lactate threshold heart rate and running speed Garmin detected.", c.LactateThreshold)
	noInputTool(server, "garmin_get_cycling_ftp", "Get cycling FTP",
		"The latest cycling functional threshold power (FTP).", c.CyclingFTP)
	noInputTool(server, "garmin_get_personal_records", "Get personal records",
		"Personal records across sports. For running (activityType running), typeId 1 is 1 km, 2 is 1 mile, "+
			"3 is 5 km, 4 is 10 km, 5 is a half marathon, 6 is a marathon, and 7 is the longest run (in meters).",
		c.PersonalRecords)
}

func (s *Server) addUnofficialHealthTools(server *mcp.Server) {
	c := s.connect
	s.dayTool(server, "garmin_get_respiration", "Get respiration",
		"Respiration rate (breaths per minute) through a day, reading by reading. garmin_get_daily_summary "+
			"has the day's averages only.", c.Respiration)
	s.dayTool(server, "garmin_get_stress_timeline", "Get stress timeline",
		"Stress level reading by reading through a day; negative values mean there was no reading, such as "+
			"during activity. garmin_get_stress has the daily totals only.", c.StressTimeline)
	s.dayTool(server, "garmin_get_floors", "Get floors",
		"Floors climbed and descended interval by interval through a day. garmin_get_daily_summary has the "+
			"day's totals only.", c.Floors)
	s.rangeTool(server, "garmin_get_blood_pressure", "Get blood pressure",
		"Blood pressure measurements logged in Garmin Connect over a range.", c.BloodPressure)
}

func (s *Server) addUnofficialPlanTools(server *mcp.Server) {
	c := s.connect
	s.dayTool(server, "garmin_get_active_training_plans", "Get active training plans",
		"Training plans active on a day, such as Garmin Coach plans, with their ids. Pass a plan's id to "+
			"garmin_get_training_plan for its schedule.", c.ActiveTrainingPlans)
	addTool(server, unofficialTool("garmin_get_training_plan", "Get training plan",
		"One training plan in full: its phases and the workouts scheduled on each day."),
		func(ctx context.Context, in planInput) (any, error) {
			return c.TrainingPlan(ctx, in.PlanID, in.Adaptive)
		})
	s.dayTool(server, "garmin_get_coach_workouts", "Get Garmin Coach workouts",
		"The training plan workouts Garmin has scheduled around a day, with each one's date, sport, planned "+
			"duration, and whether it was done. Garmin picks the window, usually the current week, so a date "+
			"further ahead can return nothing while a plan is active. Daily suggested workouts are made on the "+
			"watch and no API serves them.", c.CoachWorkouts)
	s.rangeTool(server, "garmin_get_scheduled_workouts", "Get scheduled workouts",
		"Workouts on the Garmin Connect calendar over a range, from training plans or scheduled by hand, with "+
			"each one's date, workoutId, sport, and whether an activity completed it.", c.ScheduledWorkouts)
	addTool(server, unofficialTool("garmin_get_calendar_month", "Get calendar month",
		"The Garmin Connect calendar for one month: scheduled workouts, training plan days, races and other "+
			"events, and recorded activities, each with its date."),
		func(ctx context.Context, in monthInput) (any, error) {
			now := s.today()
			year, month := now.Year(), now.Month()
			if in.Year != nil {
				year = *in.Year
			}
			if in.Month != nil {
				month = time.Month(*in.Month)
			}
			return c.CalendarMonth(ctx, year, month)
		})
	addTool(server, unofficialTool("garmin_list_workouts", "List workouts",
		"Workouts saved in the Garmin Connect workout library. Pass a workoutId to garmin_get_workout for "+
			"the steps."),
		func(ctx context.Context, in workoutsInput) (any, error) {
			limit, offset := 20, 0
			if in.Limit != nil {
				limit = *in.Limit
			}
			if in.Offset != nil {
				offset = *in.Offset
			}
			if limit < 1 || limit > maxWorkouts || offset < 0 {
				return nil, fmt.Errorf("limit must be 1-%d and offset 0 or more", maxWorkouts)
			}
			return c.Workouts(ctx, offset, limit)
		})
	addTool(server, unofficialTool("garmin_get_workout", "Get workout",
		"One workout's structure: its steps and repeats, each with its duration or distance and target."),
		func(ctx context.Context, in workoutInput) (any, error) {
			return c.Workout(ctx, in.WorkoutID)
		})
}

func (s *Server) addUnofficialGearTools(server *mcp.Server) {
	c := s.connect
	noInputTool(server, "garmin_get_devices", "Get devices",
		"Garmin devices registered to the account.", c.Devices)
	noInputTool(server, "garmin_get_gear", "Get gear",
		"Gear tracked in Garmin Connect, such as shoes and bikes.", c.Gear)
}
