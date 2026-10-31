package garmin

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"
)

const dayActivityLimit = 20

// Day combines the sections that answer "how was my day". Garmin answers 4xx
// for data that doesn't exist yet (today's HRV before you've slept), so a
// failed section becomes {"error": ...} instead of sinking the rest. Get has
// already retried a 401, so one that still fails means the key is bad, and
// that is returned as an error like any other key problem. So is a request
// that never got an answer (Garmin unreachable, or ctx done), because then no
// section can be trusted. The sections are independent, so they are fetched
// concurrently.
func (c *Client) Day(ctx context.Context, day time.Time) (map[string]any, error) {
	sections := []struct {
		name string
		read func() (any, error)
	}{
		{"summary", func() (any, error) { return c.DailySummary(ctx, day) }},
		{"sleep", func() (any, error) { return c.Sleep(ctx, day) }},
		{"hrv", func() (any, error) { return c.HRVSummaries(ctx, day, day) }},
		{"body_battery", func() (any, error) { return c.BodyBattery(ctx, day, day, nil) }},
		{"stress", func() (any, error) { return c.Stats(ctx, StatStress, day, day) }},
		{"activities", func() (any, error) {
			return c.SearchActivities(ctx, ActivitySearch{Start: &day, End: &day, ExcludeChildren: true, Limit: dayActivityLimit})
		}},
	}
	type result struct {
		value any
		err   error
	}
	results := make([]result, len(sections))
	var wg sync.WaitGroup
	for i, section := range sections {
		wg.Go(func() {
			value, err := section.read()
			results[i] = result{value, err}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	record := map[string]any{"date": isoDate(day)}
	for i, section := range sections {
		value, err := results[i].value, results[i].err
		var transportErr *url.Error
		switch {
		case err == nil:
			record[section.name] = value
		case IsUnauthorized(err), errors.Is(err, ErrAPIKey), errors.As(err, &transportErr):
			return nil, err
		default:
			record[section.name] = map[string]any{"error": err.Error()}
		}
	}
	return record, nil
}
