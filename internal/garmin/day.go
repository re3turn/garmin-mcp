package garmin

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"
)

const (
	DayActivityLimit = 20
	// NoDataMessage explains Garmin's empty 204, which carries no reason.
	NoDataMessage = "Garmin returned nothing for this request: nothing has been recorded " +
		"for it yet, or your device doesn't measure it."
)

// Day combines the sections that answer "how was my day". Garmin answers 4xx
// for data that doesn't exist yet (today's HRV before you've slept), so a
// failed section becomes {"error": ...} instead of sinking the rest. Some
// failures mean no section can be trusted, and Day returns them as its error:
// a 401 that Get already retried (the key is bad), a key problem, a failure to
// get a token, or a request that never got an answer (Garmin unreachable, or
// ctx done). The sections are independent, so they are fetched concurrently,
// and the first such failure cancels the rest.
func (c *Client) Day(parent context.Context, day time.Time) (map[string]any, error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
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
			return c.SearchActivities(ctx, ActivitySearch{Start: &day, End: &day, ExcludeChildren: true, Limit: DayActivityLimit})
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
			if isFatalForDay(err) {
				cancel(err)
			}
			results[i] = result{value, err}
		})
	}
	wg.Wait()
	if err := parent.Err(); err != nil {
		return nil, err
	}
	// The first fatal error is the cause; sections it cancelled only report
	// that they were cancelled.
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}

	record := map[string]any{"date": isoDate(day)}
	for i, section := range sections {
		value, err := results[i].value, results[i].err
		switch {
		case err != nil:
			record[section.name] = map[string]any{"error": err.Error()}
		case value == nil:
			record[section.name] = map[string]any{"no_data": NoDataMessage}
		default:
			record[section.name] = value
		}
	}
	return record, nil
}

func isFatalForDay(err error) bool {
	var transportErr *url.Error
	return IsUnauthorized(err) || errors.Is(err, ErrAPIKey) || isTokenError(err) || errors.As(err, &transportErr)
}
