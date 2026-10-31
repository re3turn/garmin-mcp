package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/re3turn/garmin-personal-mcp/internal/garmin"
)

const (
	defaultActivityLimit = 10
	rangeDoc             = "start, end: YYYY-MM-DD inclusive (default: the 7 days ending today)."
)

// Tool and field descriptions are what clients show the model, so they carry
// units, defaults, and cross-tool hints. Describe only fields Garmin's
// reference documents.

type dayInput struct {
	Date string `json:"date,omitempty" jsonschema:"YYYY-MM-DD (default: today)"`
}

type nightInput struct {
	Date string `json:"date,omitempty" jsonschema:"YYYY-MM-DD, the morning you woke up (default: today, i.e. last night)"`
}

type rangeInput struct {
	Start string `json:"start,omitempty" jsonschema:"first day, YYYY-MM-DD inclusive (default: 6 days before end)"`
	End   string `json:"end,omitempty" jsonschema:"last day, YYYY-MM-DD inclusive (default: today)"`
}

type bodyBatteryInput struct {
	rangeInput
	Chunks *int `json:"chunks,omitempty" jsonschema:"split each day into this many intervals (e.g. 24 for hourly) and include each interval's lowest and highest level"`
}

type samplesInput struct {
	dayInput
	IncludeSamples bool `json:"include_samples,omitempty" jsonschema:"also return the all-day readings (large)"`
}

type weightInput struct {
	rangeInput
	IncludeAll bool `json:"include_all,omitempty" jsonschema:"list every weigh-in per day with its time and body composition, not just the latest"`
}

type foodLogInput struct {
	dayInput
	MealID           *int  `json:"meal_id,omitempty" jsonschema:"limit to one meal; ids appear in mealDetails of an unfiltered call"`
	IncludeGoals     *bool `json:"include_goals,omitempty" jsonschema:"include nutrition goals (default true)"`
	IncludeNutrition *bool `json:"include_nutrition,omitempty" jsonschema:"include nutrients consumed (default true)"`
	IncludeMedia     bool  `json:"include_media,omitempty" jsonschema:"also return logged food photos"`
}

type searchInput struct {
	Start                    string   `json:"start,omitempty" jsonschema:"earliest local start date, YYYY-MM-DD"`
	End                      string   `json:"end,omitempty" jsonschema:"latest local start date, YYYY-MM-DD (default: today when start is set)"`
	Search                   string   `json:"search,omitempty" jsonschema:"text in the activity name or description"`
	ActivityType             string   `json:"activity_type,omitempty" jsonschema:"parent type key, e.g. running, cycling, swimming, strength_training, hiking, walking"`
	ActivitySubtype          string   `json:"activity_subtype,omitempty" jsonschema:"child type key such as trail_running or indoor_cycling; must belong to activity_type when both are set"`
	ExcludedActivitySubtypes []string `json:"excluded_activity_subtypes,omitempty" jsonschema:"child type keys to leave out"`
	EventType                string   `json:"event_type,omitempty" jsonschema:"race, training, recreation, fitness, specialEvent, or uncategorized"`
	Favorite                 *bool    `json:"favorite,omitempty" jsonschema:"true for favorites only, false for non-favorites (default: both)"`
	ExcludeChildren          *bool    `json:"exclude_children,omitempty" jsonschema:"hide individual legs of multisport activities (default true)"`
	SortBy                   string   `json:"sort_by,omitempty" jsonschema:"a field such as distance, duration, or name (default: start time)"`
	SortOrder                string   `json:"sort_order,omitempty" jsonschema:"asc or desc (default desc)"`
	MinDistance              *int     `json:"min_distance,omitempty" jsonschema:"meters"`
	MaxDistance              *int     `json:"max_distance,omitempty" jsonschema:"meters"`
	MinDuration              *int     `json:"min_duration,omitempty" jsonschema:"timer seconds"`
	MaxDuration              *int     `json:"max_duration,omitempty" jsonschema:"timer seconds"`
	MinElapsedDuration       *int     `json:"min_elapsed_duration,omitempty" jsonschema:"seconds including pauses"`
	MaxElapsedDuration       *int     `json:"max_elapsed_duration,omitempty" jsonschema:"seconds including pauses"`
	MinElevation             *int     `json:"min_elevation,omitempty" jsonschema:"elevation gain, meters"`
	MaxElevation             *int     `json:"max_elevation,omitempty" jsonschema:"elevation gain, meters"`
	MinAvgPower              *int     `json:"min_avg_power,omitempty" jsonschema:"watts"`
	North                    *float64 `json:"north,omitempty" jsonschema:"start-location bounding box: northern latitude in decimal degrees; give all four edges or none"`
	South                    *float64 `json:"south,omitempty" jsonschema:"start-location bounding box: southern latitude"`
	East                     *float64 `json:"east,omitempty" jsonschema:"start-location bounding box: eastern longitude"`
	West                     *float64 `json:"west,omitempty" jsonschema:"start-location bounding box: western longitude"`
	MinMaxDepth              *int     `json:"min_max_depth,omitempty" jsonschema:"dives: lower bound on maximum depth, centimeters"`
	MaxMaxDepth              *int     `json:"max_max_depth,omitempty" jsonschema:"dives: upper bound on maximum depth, centimeters"`
	DecoDive                 *bool    `json:"deco_dive,omitempty" jsonschema:"dives: true for decompression dives, false for non-decompression dives"`
	MinBottomTime            *int     `json:"min_bottom_time,omitempty" jsonschema:"dives: seconds"`
	MaxBottomTime            *int     `json:"max_bottom_time,omitempty" jsonschema:"dives: seconds"`
	RequiredFields           []string `json:"required_fields,omitempty" jsonschema:"activity fields that must be present, e.g. avgPower"`
	Limit                    *int     `json:"limit,omitempty" jsonschema:"page size (default 10); each activity is about 3-4 KB"`
	Offset                   *int     `json:"offset,omitempty" jsonschema:"zero-based index of the first activity to return (default 0)"`
}

type activityInput struct {
	ActivityID int64 `json:"activity_id" jsonschema:"activityId from garmin_search_activities or garmin_get_day"`
}

type downloadInput struct {
	activityInput
	Directory string `json:"directory,omitempty" jsonschema:"a subfolder of the download folder to save in, e.g. races (default: the download folder itself)"`
}

type zonesInput struct {
	GenerateDefaults *bool `json:"generate_defaults,omitempty" jsonschema:"return Garmin's default zones when none are configured (default true)"`
}

type fitnessAgeInput struct {
	dayInput
	FullHistoryScan *bool `json:"full_history_scan,omitempty" jsonschema:"search all history for the latest value (default true); false limits it to the 28 days before date"`
}

type menstrualDayInput struct {
	dayInput
	Today            string `json:"today,omitempty" jsonschema:"reference date for predictions, YYYY-MM-DD (default: today)"`
	Reduced          bool   `json:"reduced,omitempty" jsonschema:"return a lighter response"`
	SkipBabyMovement bool   `json:"skip_baby_movement,omitempty" jsonschema:"leave out kick counts"`
	IncludeAnalysis  bool   `json:"include_analysis,omitempty" jsonschema:"add Garmin's cycle analysis"`
}

func (s *Server) addTools(server *mcp.Server) {
	s.addSummaryTools(server)
	s.addWellnessTools(server)
	s.addSleepTools(server)
	s.addBodyTools(server)
	s.addActivityTools(server)
	s.addProfileTools(server)
}

func (s *Server) addSummaryTools(server *mcp.Server) {
	addTool(server, &mcp.Tool{
		Name: "garmin_get_day", Title: "Get day overview", Annotations: readsGarmin,
		Description: "One day at a glance: daily summary (steps, calories, intensity minutes, resting HR, " +
			"stress, body battery), last night's sleep, HRV, body battery, stress, and the day's activities. " +
			`A section Garmin has no data for yet comes back as {"error": ...} while the rest still load.`,
	}, func(ctx context.Context, in dayInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		return s.client.Day(ctx, day)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_daily_summary", Title: "Get daily summary", Annotations: readsGarmin,
		Description: "Every all-day wellness total for one date: steps, distance, floors, total/active/BMR " +
			"calories, intensity minutes, heart rate, stress, body battery, SpO2, respiration, and time " +
			"active/sedentary/asleep.",
	}, func(ctx context.Context, in dayInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		return s.client.DailySummary(ctx, day)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_daily_summaries", Title: "Get daily summaries", Annotations: readsGarmin,
		Description: "Daily wellness summaries across a date range, grouped per day (movement, calories, heart " +
			"rate, intensity minutes, stress, body battery, hydration, respiration, pulse ox). About 1-2 KB " +
			"per day, so for a long trend in one metric prefer garmin_get_steps, garmin_get_stress, or " +
			"garmin_get_sleep_summaries. " + rangeDoc + " Ranges over a year are fetched a year at a time.",
	}, func(ctx context.Context, in rangeInput) (any, error) {
		first, last, err := s.parseRange(in.Start, in.End)
		if err != nil {
			return nil, err
		}
		return s.client.DailySummaries(ctx, first, last)
	})

	stats := []struct {
		name, title, description string
		stat                     garmin.Stat
	}{
		{"garmin_get_steps", "Get steps", "Daily total steps, distance (meters), and step goal. " + rangeDoc +
			" Any length works; long ranges are fetched 28 days at a time.", garmin.StatSteps},
		{"garmin_get_stress", "Get stress", "Daily overall stress level (0-100) and seconds spent in high, " +
			"medium, low, and rest stress. " + rangeDoc + " Any length works; long ranges are fetched 28 days at a time.",
			garmin.StatStress},
		{"garmin_get_hydration", "Get hydration", "Daily water intake, goal, estimated sweat loss, and intake " +
			"logged during activities, all in mL. " + rangeDoc + " Any length works; long ranges are fetched 31 days at a time.",
			garmin.StatHydration},
	}
	for _, stat := range stats {
		addTool(server, &mcp.Tool{
			Name: stat.name, Title: stat.title, Annotations: readsGarmin, Description: stat.description,
		}, func(ctx context.Context, in rangeInput) (any, error) {
			first, last, err := s.parseRange(in.Start, in.End)
			if err != nil {
				return nil, err
			}
			return s.client.Stats(ctx, stat.stat, first, last)
		})
	}
}

func (s *Server) addWellnessTools(server *mcp.Server) {
	addTool(server, &mcp.Tool{
		Name: "garmin_get_body_battery", Title: "Get body battery", Annotations: readsGarmin,
		Description: "Daily Body Battery: amount charged and drained, plus the events that moved it (sleep, " +
			"activities, stress) with Garmin's feedback. " + rangeDoc + " Any length works; long ranges are " +
			"fetched 31 days at a time. Without chunks, the every-few-minutes readings are left out to keep " +
			"the response small.",
	}, func(ctx context.Context, in bodyBatteryInput) (any, error) {
		first, last, err := s.parseRange(in.Start, in.End)
		if err != nil {
			return nil, err
		}
		return s.client.BodyBattery(ctx, first, last, in.Chunks)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_heart_rate", Title: "Get heart rate", Annotations: readsGarmin,
		Description: "Resting, minimum, and maximum heart rate for a day, the 7-day resting average, and any " +
			"abnormal high/low heart rate alerts.",
	}, func(ctx context.Context, in samplesInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		return s.client.HeartRate(ctx, day, in.IncludeSamples)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_spo2", Title: "Get SpO2", Annotations: readsGarmin,
		Description: "Blood oxygen (pulse ox) for a day: average, lowest, latest, sleep average, 7-day average, " +
			"hourly averages, and altitude acclimation.",
	}, func(ctx context.Context, in samplesInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		return s.client.SpO2(ctx, day, in.IncludeSamples)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_wellness_activities", Title: "Get wellness activities", Annotations: readsGarmin,
		Description: "Wellness activities recorded by the device, each with its type, start and end time, " +
			"per-metric summary values, and time in zones. Regular workouts like runs and rides are in " +
			"garmin_search_activities instead. " + rangeDoc,
	}, func(ctx context.Context, in rangeInput) (any, error) {
		first, last, err := s.parseRange(in.Start, in.End)
		if err != nil {
			return nil, err
		}
		return s.client.WellnessActivities(ctx, first, last)
	})
}

func (s *Server) addSleepTools(server *mcp.Server) {
	addTool(server, &mcp.Tool{
		Name: "garmin_get_sleep", Title: "Get sleep", Annotations: readsGarmin,
		Description: "The full sleep record for the night ending on date: sleep score and its components, " +
			"deep/light/REM/awake seconds, start and end times, overnight heart rate, SpO2, respiration, " +
			"sleep stress, naps, and Garmin's feedback.",
	}, func(ctx context.Context, in nightInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		return s.client.Sleep(ctx, day)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_sleep_summaries", Title: "Get sleep summaries", Annotations: readsGarmin,
		Description: "Nightly sleep records across a date range, same fields as garmin_get_sleep. Use it for " +
			"sleep trends. " + rangeDoc + " Any length works; long ranges are fetched 31 days at a time.",
	}, func(ctx context.Context, in rangeInput) (any, error) {
		first, last, err := s.parseRange(in.Start, in.End)
		if err != nil {
			return nil, err
		}
		return s.client.SleepSummaries(ctx, first, last)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_hrv", Title: "Get HRV night", Annotations: readsGarmin,
		Description: "One night of heart-rate variability: the summary (last-night average, 5-minute high, " +
			"weekly average, baseline band, status) plus every reading taken during sleep.",
	}, func(ctx context.Context, in nightInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		return s.client.HRV(ctx, day)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_hrv_summaries", Title: "Get HRV summaries", Annotations: readsGarmin,
		Description: "Nightly HRV summaries across a date range: last-night average, weekly average, baseline " +
			"band, status, and Garmin's feedback. " + rangeDoc,
	}, func(ctx context.Context, in rangeInput) (any, error) {
		first, last, err := s.parseRange(in.Start, in.End)
		if err != nil {
			return nil, err
		}
		return s.client.HRVSummaries(ctx, first, last)
	})
}

func (s *Server) addBodyTools(server *mcp.Server) {
	addTool(server, &mcp.Tool{
		Name: "garmin_get_weight", Title: "Get weight", Annotations: readsGarmin,
		Description: "Daily weight summaries over a range, the range average, and the nearest weigh-ins before " +
			"and after it (handy when the range itself has none). Weights are in grams; body composition " +
			"(BMI, body fat %, muscle and bone mass) appears when a smart scale recorded it. " + rangeDoc,
	}, func(ctx context.Context, in weightInput) (any, error) {
		first, last, err := s.parseRange(in.Start, in.End)
		if err != nil {
			return nil, err
		}
		return s.client.Weight(ctx, first, last, in.IncludeAll)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_weight_goals", Title: "Get weight goals", Annotations: readsGarmin,
		Description: "Weight goals in effect during a date range and their target ranges. " + rangeDoc,
	}, func(ctx context.Context, in rangeInput) (any, error) {
		first, last, err := s.parseRange(in.Start, in.End)
		if err != nil {
			return nil, err
		}
		return s.client.WeightGoals(ctx, first, last)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_food_log", Title: "Get food log", Annotations: readsGarmin,
		Description: "Foods logged in Garmin Connect's nutrition tracker for a day, with serving sizes, " +
			"calories and nutrients consumed, nutrition goals, and a per-meal breakdown.",
	}, func(ctx context.Context, in foodLogInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		return s.client.FoodLog(ctx, day, garmin.FoodLogOptions{
			MealID:           in.MealID,
			IncludeGoals:     valueOr(in.IncludeGoals, true),
			IncludeNutrition: valueOr(in.IncludeNutrition, true),
			IncludeMedia:     in.IncludeMedia,
		})
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_menstrual_cycle", Title: "Get menstrual cycle", Annotations: readsGarmin,
		Description: "Menstrual cycle summary as of a date: current phase, day in cycle, days until the next " +
			"phase, period length, fertile window, predicted ovulation and cycle length, and pregnancy " +
			"details when tracking a pregnancy.",
	}, func(ctx context.Context, in dayInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		return s.client.MenstrualCycle(ctx, day)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_menstrual_day", Title: "Get menstrual cycle day", Annotations: readsGarmin,
		Description: "One day in detail: logged symptoms, flow, mood, and other entries, that day's cycle " +
			"summary, and weekly weight goal ranges during pregnancy.",
	}, func(ctx context.Context, in menstrualDayInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		today, err := s.parseDay(in.Today)
		if err != nil {
			return nil, err
		}
		return s.client.MenstrualDay(ctx, day, garmin.MenstrualDayOptions{
			Today:            today,
			Reduced:          in.Reduced,
			SkipBabyMovement: in.SkipBabyMovement,
			IncludeAnalysis:  in.IncludeAnalysis,
		})
	})
}

func (s *Server) addActivityTools(server *mcp.Server) {
	addTool(server, &mcp.Tool{
		Name: "garmin_search_activities", Title: "Search activities", Annotations: readsGarmin,
		Description: "Recorded activities (runs, rides, swims, strength, dives, ...), newest first, with name, " +
			"type, start time, distance, duration, pace/speed, HR, power, cadence, calories, elevation, and " +
			"training effect. All filters are optional and combine. Pass an activityId to garmin_get_activity " +
			"for splits and full detail.",
	}, s.searchActivities)

	addTool(server, &mcp.Tool{
		Name: "garmin_get_activity", Title: "Get activity", Annotations: readsGarmin,
		Description: "Full detail for one activity: summary metrics (distance, time, pace, HR, power, cadence, " +
			"elevation, training effect, calories), device and location metadata, and split/lap summaries.",
	}, func(ctx context.Context, in activityInput) (any, error) {
		return s.client.Activity(ctx, in.ActivityID)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_activity_workouts", Title: "Get activity workouts", Annotations: readsGarmin,
		Description: "The structured workout(s) followed during an activity, step by step with targets.",
	}, func(ctx context.Context, in activityInput) (any, error) {
		return s.client.ActivityWorkouts(ctx, in.ActivityID)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_download_activity", Title: "Download activity file", Annotations: savesLocalFile,
		Description: "Download an activity's original file (a ZIP, usually holding a .fit) and save it on the " +
			"machine running this server as activity-<activity_id>.zip, replacing any earlier download. " +
			"Returns the saved path, size, and the files inside. The download folder is " +
			garmin.DownloadDirEnvVar + ", else a garmin folder in the Downloads directory; directory must be " +
			"a subfolder inside it.",
	}, func(ctx context.Context, in downloadInput) (any, error) {
		target, err := garmin.DownloadTarget(in.Directory)
		if err != nil {
			return nil, err
		}
		return s.client.DownloadActivity(ctx, in.ActivityID, target)
	})
}

func (s *Server) searchActivities(ctx context.Context, in searchInput) (any, error) {
	search := garmin.ActivitySearch{
		Search:             in.Search,
		ActivityType:       in.ActivityType,
		ActivitySubtype:    in.ActivitySubtype,
		ExcludedSubtypes:   in.ExcludedActivitySubtypes,
		EventType:          in.EventType,
		Favorite:           in.Favorite,
		ExcludeChildren:    valueOr(in.ExcludeChildren, true),
		SortBy:             in.SortBy,
		SortOrder:          in.SortOrder,
		MinDistance:        in.MinDistance,
		MaxDistance:        in.MaxDistance,
		MinDuration:        in.MinDuration,
		MaxDuration:        in.MaxDuration,
		MinElapsedDuration: in.MinElapsedDuration,
		MaxElapsedDuration: in.MaxElapsedDuration,
		MinElevation:       in.MinElevation,
		MaxElevation:       in.MaxElevation,
		MinAvgPower:        in.MinAvgPower,
		MinMaxDepth:        in.MinMaxDepth,
		MaxMaxDepth:        in.MaxMaxDepth,
		DecoDive:           in.DecoDive,
		MinBottomTime:      in.MinBottomTime,
		MaxBottomTime:      in.MaxBottomTime,
		RequiredFields:     in.RequiredFields,
		Limit:              defaultActivityLimit,
	}
	if in.Limit != nil {
		search.Limit = *in.Limit
	}
	if in.Offset != nil {
		search.Offset = *in.Offset
	}
	switch {
	case in.Start != "":
		first, last, err := s.parseRange(in.Start, in.End)
		if err != nil {
			return nil, err
		}
		search.Start, search.End = &first, &last
	case in.End != "":
		// end alone means "up to end", not parseRange's 7-day default span.
		last, err := s.parseDay(in.End)
		if err != nil {
			return nil, err
		}
		search.End = &last
	}
	box, err := garmin.NewBoundingBox(in.North, in.South, in.East, in.West)
	if err != nil {
		return nil, err
	}
	search.StartArea = box
	return s.client.SearchActivities(ctx, search)
}

func (s *Server) addProfileTools(server *mcp.Server) {
	addTool(server, &mcp.Tool{
		Name: "garmin_get_profile", Title: "Get profile settings", Annotations: readsGarmin,
		Description: "Profile settings: display name, measurement system and display formats, time zone, " +
			"hydration units and containers, and preferred training days. Check measurementSystem before " +
			"converting units for the user.",
	}, func(ctx context.Context, _ struct{}) (any, error) {
		return s.client.ProfileSettings(ctx)
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_heart_rate_zones", Title: "Get heart rate zones", Annotations: readsGarmin,
		Description: "Heart rate zone floors per sport, with the training method and the resting, max, and " +
			"lactate threshold heart rates the zones were built on.",
	}, func(ctx context.Context, in zonesInput) (any, error) {
		return s.client.HeartRateZones(ctx, valueOr(in.GenerateDefaults, true))
	})

	addTool(server, &mcp.Tool{
		Name: "garmin_get_fitness_age", Title: "Get fitness age", Annotations: readsGarmin,
		Description: "Fitness age versus chronological age as of a date, the achievable fitness age, and the " +
			"components behind it (vigorous days and minutes, resting heart rate, BMI or body fat %), or the " +
			"reason none is available.",
	}, func(ctx context.Context, in fitnessAgeInput) (any, error) {
		day, err := s.parseDay(in.Date)
		if err != nil {
			return nil, err
		}
		return s.client.FitnessAge(ctx, day, valueOr(in.FullHistoryScan, true))
	})
}
