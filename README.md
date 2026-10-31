# garmin-personal-mcp

An MCP server that reads your own Garmin data (sleep, HRV, Body Battery, stress, heart rate, activities, and more)
from any MCP client, using Garmin Connect's official
[Personal API](https://developer.garmin.com/gc-developer-program/personal/personal-api/).
It is written in Go and runs on Linux, macOS, and Windows.

- No password: a Personal API key you create in Garmin Connect is exchanged for bearer tokens that last 24 hours
- Read-only: the Personal API can't change anything in your Garmin account
- Any date range: ranges longer than an endpoint allows (28 days for steps and stress; 31 days for sleep,
  hydration, and Body Battery; a year for daily summaries) are split into several requests
- Model-sized responses: all-day sample arrays (heart rate, SpO2, Body Battery) are left out unless you ask for them

## Install

Requires Go 1.27.3 or later.

```sh
go install github.com/re3turn/garmin-personal-mcp/cmd/garmin-personal-mcp@latest
```

## Authentication

1. Sign in to Garmin Connect on the web and create a key on the
   [Personal API Key](https://connect.garmin.com/app/settings/personalApiToken) page. Keys last from 30 days to
   a year, and Garmin shows each key only once.
2. Save the key. The input is hidden.

   ```sh
   garmin-personal-mcp auth
   ```

   Without a terminal, pipe the key in instead (e.g. `pbpaste | garmin-personal-mcp auth`).
3. Run `garmin-personal-mcp auth --check` to confirm the saved key works.

The key is checked with Garmin and then saved to `~/.config/garmin/connect-personal-api-key`
(`%USERPROFILE%\.config\garmin\` on Windows). Bearer tokens are cached in
`connect-personal-api-bearer-token.json` in the same directory. Both use the same location and format as Garmin's
official Python client (`gc-personal`), so a key saved by either one works for both.

`GARMIN_PERSONAL_API_KEY`, when set, takes precedence over the key file.

## Add it to your MCP client

Claude Code:

```sh
claude mcp add garmin -- garmin-personal-mcp
```

Claude Desktop, Cursor, and other clients that use the `mcpServers` format:

```json
{
  "mcpServers": {
    "garmin": {
      "command": "garmin-personal-mcp"
    }
  }
}
```

If the `go install` directory (`$(go env GOPATH)/bin`) isn't on your PATH, put the full path in `command`.

## Tools

All dates are `YYYY-MM-DD`. Range tools default to the 7 days ending today.
Tools that return a list wrap it as `{"results": [...]}`, and when Garmin has no data (HTTP 204) a tool returns
`{"no_data": ...}`.

| Tool | What it returns |
| --- | --- |
| `garmin_get_day` | One day at a glance: summary, sleep, HRV, Body Battery, stress, and activities |
| `garmin_get_daily_summary` | Every all-day wellness total for one date |
| `garmin_get_daily_summaries` | Daily summaries across a range |
| `garmin_get_steps` | Daily steps, distance, and goal |
| `garmin_get_stress` | Daily stress level and time in each stress band |
| `garmin_get_hydration` | Daily water intake, goal, and sweat loss |
| `garmin_get_body_battery` | Daily charge and drain with the events behind them; `chunks` for per-interval lows and highs |
| `garmin_get_heart_rate` | Resting, minimum, and maximum HR and abnormal HR alerts; all-day samples with `include_samples` |
| `garmin_get_spo2` | SpO2 averages, hourly averages, and altitude acclimation |
| `garmin_get_wellness_activities` | Wellness activities recorded by the device |
| `garmin_get_sleep` | One night of sleep: score, stages, and overnight vitals |
| `garmin_get_sleep_summaries` | Nightly sleep records across a range |
| `garmin_get_hrv` | One night of HRV with every reading |
| `garmin_get_hrv_summaries` | Nightly HRV summaries, baseline, and status across a range |
| `garmin_get_weight` | Daily weight and the range average; every weigh-in with `include_all` |
| `garmin_get_weight_goals` | Weight goals in effect during a range |
| `garmin_get_food_log` | Nutrition log: foods, nutrients, and goals |
| `garmin_search_activities` | Activity search by type, dates, distance, power, start area, dive filters, and more |
| `garmin_get_activity` | One activity in full, with splits |
| `garmin_get_activity_workouts` | The structured workout followed during an activity |
| `garmin_download_activity` | Save an activity's original file (a ZIP holding the .fit) locally |
| `garmin_get_profile` | Profile settings: units, display formats, time zone |
| `garmin_get_heart_rate_zones` | Heart rate zones per sport |
| `garmin_get_fitness_age` | Fitness age and its components |
| `garmin_get_menstrual_cycle` | Menstrual cycle summary |
| `garmin_get_menstrual_day` | One day's logged entries and cycle detail |

Weights are in grams, distances in meters, and durations in seconds, as Garmin returns them.

`/weight-service/weight/all` from the Swagger reference has no tool, because the reference implementation reports
that it returns HTTP 400 for every range. `garmin_get_weight` with `include_all` returns the same entries.

## Environment variables

| Variable | Purpose | Default |
| --- | --- | --- |
| `GARMIN_PERSONAL_API_KEY` | Personal API key; takes precedence over the key file | – |
| `GARMIN_MCP_DOWNLOAD_DIR` | Where `garmin_download_activity` saves files; nothing is written outside it. Must be an absolute path or start with `~/`, which expands to your home directory | `~/Downloads/garmin` |
| `GARMIN_PERSONAL_API_BASE_URL` | API base URL (`https://personal.connectapi.garmin.cn` for the China region) | `https://personal.connectapi.garmin.com` |

## Development

```sh
go test ./...   # no network and no Garmin account needed
go vet ./...
```

## License

[MIT](LICENSE). Garmin and Garmin Connect are trademarks of Garmin Ltd. or its subsidiaries.
This project is not affiliated with or endorsed by Garmin.
