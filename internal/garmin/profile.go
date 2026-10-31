package garmin

import (
	"context"
	"fmt"
	"time"
)

func (c *Client) ProfileSettings(ctx context.Context) (any, error) {
	return c.Get(ctx, "/userprofile-service/userprofile/settings", nil)
}

func (c *Client) HeartRateZones(ctx context.Context, generateDefaults bool) (any, error) {
	return c.Get(ctx, "/biometric-service/heartRateZones/v2", query{}.boolean("generateDefaults", &generateDefaults).values())
}

func (c *Client) FitnessAge(ctx context.Context, day time.Time, fullHistoryScan bool) (any, error) {
	return c.Get(ctx, "/fitnessage-service/fitnessage/v2/"+isoDate(day), query{}.boolean("fullHistoryScan", &fullHistoryScan).values())
}

func (c *Client) Weight(ctx context.Context, start, end time.Time, includeAll bool) (any, error) {
	return c.Get(ctx, fmt.Sprintf("/weight-service/weight/range/%s/%s", isoDate(start), isoDate(end)), query{}.boolean("includeAll", &includeAll).values())
}

func (c *Client) WeightGoals(ctx context.Context, start, end time.Time) (any, error) {
	return c.Get(ctx, fmt.Sprintf("/goal-service/goal/user/effective/weightgoal/%s/%s", isoDate(start), isoDate(end)), nil)
}

type FoodLogOptions struct {
	MealID           *int
	IncludeGoals     bool
	IncludeNutrition bool
	IncludeMedia     bool
}

func (c *Client) FoodLog(ctx context.Context, day time.Time, opts FoodLogOptions) (any, error) {
	q := query{}.
		integer("mealId", opts.MealID).
		boolean("includeGoals", &opts.IncludeGoals).
		boolean("includeNutrition", &opts.IncludeNutrition).
		boolean("includeMedia", &opts.IncludeMedia)
	return c.Get(ctx, "/nutrition-service/food/logs/"+isoDate(day), q.values())
}

func (c *Client) MenstrualCycle(ctx context.Context, day time.Time) (any, error) {
	return c.Get(ctx, "/periodichealth-service/menstrualcycle/summary/"+isoDate(day), nil)
}

type MenstrualDayOptions struct {
	Today            time.Time
	Reduced          bool
	SkipBabyMovement bool
	IncludeAnalysis  bool
}

func (c *Client) MenstrualDay(ctx context.Context, day time.Time, opts MenstrualDayOptions) (any, error) {
	q := query{}.
		date("todayDate", opts.Today).
		boolean("reducedResponse", &opts.Reduced).
		boolean("skipbm", &opts.SkipBabyMovement).
		boolean("includeAnalysis", &opts.IncludeAnalysis)
	return c.Get(ctx, "/periodichealth-service/menstrualcycle/dayview/"+isoDate(day), q.values())
}
