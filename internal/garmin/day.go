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

// Day fetches the sections concurrently. Garmin answers 4xx for data that
// doesn't exist yet (today's HRV before sleep), so a failed section becomes
// {"error": ...}. Failures that make every section untrustworthy (see
// isFatalForDay) cancel the rest and are returned instead.
func (c *Client) Day(parent context.Context, day time.Time) (map[string]any, error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	sections := []struct {
		name string
		read func() (any, error)
		// emptyIsNoData marks sections that return one row per day, so an empty
		// list means Garmin had nothing for the day (its 204), the same as a nil
		// value. An empty activity list is a real answer: no activities.
		emptyIsNoData bool
	}{
		{"summary", func() (any, error) { return c.DailySummary(ctx, day) }, false},
		{"sleep", func() (any, error) { return c.Sleep(ctx, day) }, false},
		{"hrv", func() (any, error) { return c.HRVSummaries(ctx, day, day) }, false},
		{"body_battery", func() (any, error) { return c.BodyBattery(ctx, day, day, nil) }, true},
		{"stress", func() (any, error) { return c.Stats(ctx, StatStress, day, day) }, true},
		{"activities", func() (any, error) {
			return c.SearchActivities(ctx, ActivitySearch{Start: &day, End: &day, ExcludeChildren: true, Limit: DayActivityLimit})
		}, false},
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
	// The first fatal error is the cause; sections it cancelled only report
	// that they were cancelled. A cancelled parent is the cause too.
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}

	record := map[string]any{"date": isoDate(day)}
	for i, section := range sections {
		value, err := results[i].value, results[i].err
		switch {
		case err != nil:
			record[section.name] = map[string]any{"error": err.Error()}
		case value == nil, section.emptyIsNoData && isEmptyList(value):
			record[section.name] = map[string]any{"no_data": NoDataMessage}
		default:
			record[section.name] = value
		}
	}
	return record, nil
}

func isEmptyList(value any) bool {
	list, ok := value.([]any)
	return ok && len(list) == 0
}

func isFatalForDay(err error) bool {
	var transportErr *url.Error
	return IsUnauthorized(err) || errors.Is(err, ErrAPIKey) || isTokenError(err) || errors.As(err, &transportErr)
}
