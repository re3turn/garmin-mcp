package garmin

import (
	"context"
	"fmt"
	"time"
)

// Endpoints of the unofficial API that the Personal API has no equivalent
// for. Data the Personal API
// serves (sleep, HRV, daily summaries, Body Battery events, activities, ...)
// is read through it instead, so nothing here duplicates a Personal API tool.
//
// Every request here is a read. Some GETs on these services change the
// account and are left out on purpose: trainingplan-service's
// /trainingplan/schedule/{planId} adds a plan to the calendar.

// raceRangeMaxDays caps a race predictions range; a longer one is refused
// before any request is sent.
const raceRangeMaxDays = 366

func (c *ConnectClient) TrainingReadiness(ctx context.Context, day time.Time) (any, error) {
	return c.Get(ctx, "/metrics-service/metrics/trainingreadiness/"+isoDate(day), nil)
}

func (c *ConnectClient) TrainingStatus(ctx context.Context, day time.Time) (any, error) {
	return c.Get(ctx, "/metrics-service/metrics/trainingstatus/aggregated/"+isoDate(day), nil)
}

func (c *ConnectClient) MaxMetrics(ctx context.Context, start, end time.Time) (any, error) {
	return c.Get(ctx, fmt.Sprintf("/metrics-service/metrics/maxmet/daily/%s/%s", isoDate(start), isoDate(end)), nil)
}

// RacePredictions returns the latest predictions when start is nil, and
// otherwise the daily or monthly predictions over the range.
func (c *ConnectClient) RacePredictions(ctx context.Context, start, end *time.Time, aggregation string) (any, error) {
	ids, err := c.ids(ctx)
	if err != nil {
		return nil, err
	}
	name := pathSegment(ids.DisplayName)
	if start == nil || end == nil {
		return c.Get(ctx, "/metrics-service/metrics/racepredictions/latest/"+name, nil)
	}
	if end.After(start.AddDate(0, 0, raceRangeMaxDays)) {
		return nil, fmt.Errorf("race predictions cover at most %d days per request", raceRangeMaxDays)
	}
	return c.Get(ctx, "/metrics-service/metrics/racepredictions/"+aggregation+"/"+name,
		query{}.date("fromCalendarDate", *start).date("toCalendarDate", *end).values())
}

func (c *ConnectClient) EnduranceScore(ctx context.Context, start, end time.Time, aggregation string) (any, error) {
	return c.Get(ctx, "/metrics-service/metrics/endurancescore/stats",
		query{}.date("startDate", start).date("endDate", end).str("aggregation", aggregation).values())
}

func (c *ConnectClient) HillScore(ctx context.Context, start, end time.Time) (any, error) {
	return c.Get(ctx, "/metrics-service/metrics/hillscore/stats",
		query{}.date("startDate", start).date("endDate", end).str("aggregation", "daily").values())
}

func (c *ConnectClient) RunningTolerance(ctx context.Context, start, end time.Time, aggregation string) (any, error) {
	return c.Get(ctx, "/metrics-service/metrics/runningtolerance/stats",
		query{}.date("startDate", start).date("endDate", end).str("aggregation", aggregation).values())
}

func (c *ConnectClient) LactateThreshold(ctx context.Context) (any, error) {
	return c.Get(ctx, "/biometric-service/biometric/latestLactateThreshold", nil)
}

func (c *ConnectClient) CyclingFTP(ctx context.Context) (any, error) {
	return c.Get(ctx, "/biometric-service/biometric/latestFunctionalThresholdPower/CYCLING", nil)
}

func (c *ConnectClient) PersonalRecords(ctx context.Context) (any, error) {
	ids, err := c.ids(ctx)
	if err != nil {
		return nil, err
	}
	return c.Get(ctx, "/personalrecord-service/personalrecord/prs/"+pathSegment(ids.DisplayName), nil)
}

func (c *ConnectClient) Respiration(ctx context.Context, day time.Time) (any, error) {
	return c.Get(ctx, "/wellness-service/wellness/daily/respiration/"+isoDate(day), nil)
}

func (c *ConnectClient) StressTimeline(ctx context.Context, day time.Time) (any, error) {
	return c.Get(ctx, "/wellness-service/wellness/dailyStress/"+isoDate(day), nil)
}

func (c *ConnectClient) Floors(ctx context.Context, day time.Time) (any, error) {
	return c.Get(ctx, "/wellness-service/wellness/floorsChartData/daily/"+isoDate(day), nil)
}

func (c *ConnectClient) BloodPressure(ctx context.Context, start, end time.Time) (any, error) {
	includeAll := true
	return c.Get(ctx, fmt.Sprintf("/bloodpressure-service/bloodpressure/range/%s/%s", isoDate(start), isoDate(end)),
		query{}.boolean("includeAll", &includeAll).values())
}

func (c *ConnectClient) ActiveTrainingPlans(ctx context.Context, day time.Time) (any, error) {
	return c.Get(ctx, "/trainingplan-service/trainingplan/active-plans", query{}.date("startDate", day).values())
}

// TrainingPlan reads a plan's phases and workouts. Garmin Coach's adaptive
// plans live under a different path from the other plans.
func (c *ConnectClient) TrainingPlan(ctx context.Context, planID int64, adaptive bool) (any, error) {
	id, err := positiveID("plan_id", planID)
	if err != nil {
		return nil, err
	}
	kind := "phased"
	if adaptive {
		kind = "fbt-adaptive"
	}
	return c.Get(ctx, "/trainingplan-service/trainingplan/"+kind+"/"+id, nil)
}

// CoachWorkouts is the window of plan workouts Garmin generates around day,
// typically the current week.
func (c *ConnectClient) CoachWorkouts(ctx context.Context, day time.Time) (any, error) {
	return c.graphQL(ctx, "trainingPlanScalar",
		graphQLArg{"calendarDate", isoDate(day)},
		graphQLArg{"lang", "en-US"},
		graphQLArg{"firstDayOfWeek", "monday"})
}

func (c *ConnectClient) ScheduledWorkouts(ctx context.Context, start, end time.Time) (any, error) {
	return c.graphQL(ctx, "workoutScheduleSummariesScalar",
		graphQLArg{"startDate", isoDate(start)},
		graphQLArg{"endDate", isoDate(end)})
}

// CalendarMonth is the Garmin Connect calendar for one month: scheduled
// workouts, plan days, races and events, and recorded activities.
func (c *ConnectClient) CalendarMonth(ctx context.Context, year int, month time.Month) (any, error) {
	if year < 2000 || month < time.January || month > time.December {
		return nil, fmt.Errorf("year must be 2000 or later and month 1-12, got %d-%d", year, month)
	}
	// The month in the path counts from 0.
	return c.Get(ctx, fmt.Sprintf("/calendar-service/year/%d/month/%d", year, month-1), nil)
}

func (c *ConnectClient) Workouts(ctx context.Context, offset, limit int) (any, error) {
	return c.Get(ctx, "/workout-service/workouts", query{}.integer("start", &offset).integer("limit", &limit).values())
}

func (c *ConnectClient) Workout(ctx context.Context, workoutID int64) (any, error) {
	id, err := positiveID("workout_id", workoutID)
	if err != nil {
		return nil, err
	}
	return c.Get(ctx, "/workout-service/workout/"+id, nil)
}

func (c *ConnectClient) Devices(ctx context.Context) (any, error) {
	return c.Get(ctx, "/device-service/deviceregistration/devices", nil)
}

func (c *ConnectClient) Gear(ctx context.Context) (any, error) {
	ids, err := c.ids(ctx)
	if err != nil {
		return nil, err
	}
	if ids.ProfileID == "" {
		return nil, fmt.Errorf("the Garmin Connect profile has no profileId, which the gear list needs")
	}
	return c.Get(ctx, "/gear-service/gear/filterGear", query{}.str("userProfilePk", ids.ProfileID).values())
}
