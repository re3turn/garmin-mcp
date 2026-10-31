package garmin

import (
	"context"
	"fmt"
	"time"
)

type Stat string

const (
	StatSteps     Stat = "steps"
	StatStress    Stat = "stress"
	StatHydration Stat = "hydration"
)

var statMaxDays = map[Stat]int{StatSteps: stepsMaxDays, StatStress: stressMaxDays, StatHydration: hydrationMaxDays}

func (c *Client) DailySummary(ctx context.Context, day time.Time) (any, error) {
	return c.Get(ctx, "/usersummary-service/usersummary/daily", query{}.date("calendarDate", day).values())
}

// DailySummaries keeps the endpoint's {"data": [...]} shape across windows.
func (c *Client) DailySummaries(ctx context.Context, start, end time.Time) (any, error) {
	const path = "/usersummary-service/v2/dailysummaries"
	rows, err := inWindows(start, end, dailySummariesMaxDays, func(first, last time.Time) ([]any, error) {
		page, err := c.Get(ctx, path, query{}.date("startDate", first).date("endDate", last).values())
		if err != nil || page == nil {
			return nil, err
		}
		object, ok := page.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("GET %s returned %T, expected a JSON object", path, page)
		}
		return asList(object["data"], path)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"data": rows}, nil
}

func (c *Client) Stats(ctx context.Context, stat Stat, start, end time.Time) (any, error) {
	maxDays, ok := statMaxDays[stat]
	if !ok {
		return nil, fmt.Errorf("unknown stat %q", stat)
	}
	return inWindows(start, end, maxDays, func(first, last time.Time) ([]any, error) {
		path := fmt.Sprintf("/usersummary-service/stats/%s/daily/%s/%s", stat, isoDate(first), isoDate(last))
		page, err := c.Get(ctx, path, nil)
		if err != nil {
			return nil, err
		}
		return asList(page, path)
	})
}
