package garmin

import (
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"time"
)

const DateLayout = "2006-01-02"

func isoDate(day time.Time) string { return day.Format(DateLayout) }

type query url.Values

func (q query) date(name string, day time.Time) query {
	q[name] = []string{isoDate(day)}
	return q
}

func (q query) str(name, value string) query {
	if value != "" {
		q[name] = []string{value}
	}
	return q
}

func (q query) strs(name string, values []string) query {
	if len(values) > 0 {
		q[name] = values
	}
	return q
}

func (q query) boolean(name string, value *bool) query {
	if value != nil {
		q[name] = []string{strconv.FormatBool(*value)}
	}
	return q
}

func (q query) integer(name string, value *int) query {
	if value != nil {
		q[name] = []string{strconv.Itoa(*value)}
	}
	return q
}

func (q query) number(name string, value *float64) query {
	if value != nil {
		q[name] = []string{strconv.FormatFloat(*value, 'f', -1, 64)}
	}
	return q
}

func (q query) values() url.Values { return url.Values(q) }

// Per-request range limits from Garmin's Personal API reference
// (https://developer.garmin.com/gc-developer-program/personal/swagger-docs/).
const (
	stepsMaxDays          = 28
	stressMaxDays         = 28
	hydrationMaxDays      = 31
	sleepSummariesMaxDays = 31
	bodyBatteryMaxDays    = 31
	dailySummariesMaxDays = 365
)

// inWindows splits a range that exceeds an endpoint's documented maximum into
// consecutive requests and concatenates the rows each returns.
func inWindows(start, end time.Time, maxDays int, fetch func(first, last time.Time) ([]any, error)) ([]any, error) {
	if maxDays < 1 {
		// A zero window would never advance first.
		return nil, fmt.Errorf("window must be at least one day, got %d", maxDays)
	}
	rows := []any{}
	for first := start; !first.After(end); {
		last := first.AddDate(0, 0, maxDays-1)
		if last.After(end) {
			last = end
		}
		page, err := fetch(first, last)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page...)
		first = last.AddDate(0, 0, 1)
	}
	return rows, nil
}

// asList treats Garmin's empty response as no rows.
func asList(value any, path string) ([]any, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("GET %s returned %T, expected a JSON array", path, value)
	}
	return list, nil
}

func without(value any, fields ...string) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = without(item, fields...)
		}
		return out
	case map[string]any:
		out := maps.Clone(v)
		for _, field := range fields {
			delete(out, field)
		}
		return out
	default:
		return value
	}
}
