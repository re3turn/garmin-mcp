package garmin

import (
	"context"
	"fmt"
	"time"
)

const (
	bodyBatteryMaxDays    = 31
	sleepSummariesMaxDays = 31
)

// All-day sample arrays are large and rarely what a model needs, so they are
// dropped unless the caller asks for them.
var (
	heartRateSamples   = []string{"heartRateValues", "heartRateValueDescriptors"}
	spo2Samples        = []string{"spO2SingleValues", "spO2SingleValuesDescriptorList"}
	bodyBatterySamples = []string{"bodyBatteryValuesArray", "bodyBatteryValueDescriptorDTOList"}
)

// BodyBattery keeps the value arrays when chunks is set, because then they
// hold the per-interval lows and highs the caller asked for.
func (c *Client) BodyBattery(ctx context.Context, start, end time.Time, chunks *int) (any, error) {
	const path = "/wellness-service/wellness/bodyBattery/reports/daily"
	reports, err := inWindows(start, end, bodyBatteryMaxDays, func(first, last time.Time) ([]any, error) {
		page, err := c.Get(ctx, path, query{}.date("startDate", first).date("endDate", last).integer("chunks", chunks).values())
		if err != nil {
			return nil, err
		}
		return asList(page, path)
	})
	if err != nil || chunks != nil {
		return reports, err
	}
	return without(reports, bodyBatterySamples...), nil
}

func (c *Client) HeartRate(ctx context.Context, day time.Time, includeSamples bool) (any, error) {
	record, err := c.Get(ctx, "/wellness-service/wellness/dailyHeartRate", query{}.date("date", day).values())
	if err != nil || includeSamples {
		return record, err
	}
	return without(record, heartRateSamples...), nil
}

func (c *Client) SpO2(ctx context.Context, day time.Time, includeSamples bool) (any, error) {
	record, err := c.Get(ctx, "/wellness-service/wellness/daily/spo2acclimation/"+isoDate(day), nil)
	if err != nil || includeSamples {
		return record, err
	}
	return without(record, spo2Samples...), nil
}

func (c *Client) WellnessActivities(ctx context.Context, start, end time.Time) (any, error) {
	return c.Get(ctx, fmt.Sprintf("/wellnessactivity-service/activity/summary/range/%s/%s", isoDate(start), isoDate(end)), nil)
}

func (c *Client) Sleep(ctx context.Context, day time.Time) (any, error) {
	return c.Get(ctx, "/sleep-service/sleep/dailySleep", query{}.date("date", day).values())
}

func (c *Client) SleepSummaries(ctx context.Context, start, end time.Time) (any, error) {
	const path = "/sleep-service/sleep/dailySleepSummariesByDate"
	return inWindows(start, end, sleepSummariesMaxDays, func(first, last time.Time) ([]any, error) {
		page, err := c.Get(ctx, path, query{}.date("startDate", first).date("endDate", last).values())
		if err != nil {
			return nil, err
		}
		return asList(page, path)
	})
}

func (c *Client) HRV(ctx context.Context, day time.Time) (any, error) {
	return c.Get(ctx, "/hrv-service/hrv/"+isoDate(day), nil)
}

func (c *Client) HRVSummaries(ctx context.Context, start, end time.Time) (any, error) {
	return c.Get(ctx, fmt.Sprintf("/hrv-service/hrv/daily/%s/%s", isoDate(start), isoDate(end)), nil)
}
