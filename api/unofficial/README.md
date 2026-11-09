# Unofficial Garmin Connect API responses

Garmin publishes no spec for the Garmin Connect API its own apps use, so each file here records what one
`--unofficial` tool's endpoint returned for a real account. Every value is replaced by its type (`"string"`,
`"number"`, `"boolean"`, `"null"`, or several joined with `|`), so the files hold field names and no account data.
Arrays show one merged element, `(key)` stands for map keys that are data (dates, device ids), and
`"(nullable)": true` marks an object that is `null` in some records. A field that is an object or array in some
records and a scalar in others shows only the object or array, and an array that is sometimes `null` isn't marked.

A file reflects one account on one day. A field the account had no data for is missing or `null`, and an empty
array (`[]`) means the endpoint returned nothing to learn the shape from.

To record them again (`garmin-mcp auth --unofficial` first; every request is a read):

```sh
go run ./tools/capture-unofficial/   # -date YYYY-MM-DD picks the day, default yesterday
```

## How the tools relate to the Personal API

Data the Personal API serves is read through the Personal API, so the unofficial tools cover only what it lacks.
Comparing the field names in these files with those in [`../openapi.json`](../openapi.json), leaving out
generic ones such as `calendarDate` and `deviceId` (a shared name says the field is the same, not that the values
were checked to match):

| Tool | Endpoint | Relation to the Personal API |
| --- | --- | --- |
| `garmin_get_training_readiness` | `/metrics-service/metrics/trainingreadiness/{date}` | Not in the Personal API |
| `garmin_get_training_status` | `/metrics-service/metrics/trainingstatus/aggregated/{date}` | Not in the Personal API. Its `fitnessAge` field has the same name as the one in `garmin_get_fitness_age` |
| `garmin_get_vo2max` | `/metrics-service/metrics/maxmet/daily/{start}/{end}` | Daily VO2 max is not in the Personal API, which has VO2 max per activity only. `fitnessAge` as above |
| `garmin_get_race_predictions` | `/metrics-service/metrics/racepredictions/...` | Not in the Personal API |
| `garmin_get_endurance_score` | `/metrics-service/metrics/endurancescore/stats` | Not in the Personal API |
| `garmin_get_hill_score` | `/metrics-service/metrics/hillscore/stats` | Not in the Personal API |
| `garmin_get_running_tolerance` | `/metrics-service/metrics/runningtolerance/stats` | Not in the Personal API (shape not yet recorded) |
| `garmin_get_lactate_threshold` | `/biometric-service/biometric/latestLactateThreshold` | Not in the Personal API |
| `garmin_get_cycling_ftp` | `/biometric-service/biometric/latestFunctionalThresholdPower/CYCLING` | Not in the Personal API |
| `garmin_get_personal_records` | `/personalrecord-service/personalrecord/prs/{displayName}` | Not in the Personal API |
| `garmin_get_respiration` | `/wellness-service/wellness/daily/respiration/{date}` | Adds readings through the day (`respirationValuesArray`); the daily average, high, and low are also in `garmin_get_daily_summary` |
| `garmin_get_stress_timeline` | `/wellness-service/wellness/dailyStress/{date}` | Adds stress readings through the day (`stressValuesArray`); `garmin_get_stress` has daily totals. Its `bodyBatteryValuesArray` is also in `garmin_get_body_battery` |
| `garmin_get_floors` | `/wellness-service/wellness/floorsChartData/daily/{date}` | Adds floors interval by interval; `garmin_get_daily_summary` has the day's totals |
| `garmin_get_blood_pressure` | `/bloodpressure-service/bloodpressure/range/{start}/{end}` | Not in the Personal API |
| `garmin_get_active_training_plans` | `/trainingplan-service/trainingplan/active-plans` | Not in the Personal API |
| `garmin_get_training_plan` | `/trainingplan-service/trainingplan/{phased,fbt-adaptive}/{planId}` | Not in the Personal API |
| `garmin_get_coach_workouts` | GraphQL `trainingPlanScalar` | Not in the Personal API |
| `garmin_get_scheduled_workouts` | GraphQL `workoutScheduleSummariesScalar` | Not in the Personal API |
| `garmin_get_calendar_month` | `/calendar-service/year/{year}/month/{month}` | Scheduled workouts, plan days, events, and badges are not in the Personal API; its recorded activities are also in `garmin_search_activities` |
| `garmin_list_workouts` | `/workout-service/workouts` | Not in the Personal API, which has only the workouts an activity followed (`garmin_get_activity_workouts`) |
| `garmin_get_workout` | `/workout-service/workout/{workoutId}` | As above |
| `garmin_get_devices` | `/device-service/deviceregistration/devices` | Not in the Personal API |
| `garmin_get_gear` | `/gear-service/gear/filterGear` | Not in the Personal API |

The Personal API endpoint `/weight-service/weight/all/{startDate}/{endDate}` is the only one in `openapi.json` no
tool reads; `garmin_get_weight` uses `/weight-service/weight/range/{startDate}/{endDate}`.
