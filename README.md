# garmin-mcp

An MCP server that reads your own Garmin data (sleep, HRV, Body Battery, stress, heart rate, activities, and more)
from any MCP client, including ChatGPT, using Garmin Connect's official
[Personal API](https://developer.garmin.com/gc-developer-program/personal/personal-api/).
It is written in Go, runs on Linux, macOS, and Windows, and serves over stdio or streamable HTTP.

- No password: a Personal API key you create in Garmin Connect is exchanged for bearer tokens that last 24 hours
  (only the opt-in [unofficial API](#unofficial-garmin-connect-api-optional) needs a sign-in)
- Read-only: the Personal API can't change anything in your Garmin account
- Any date range: ranges longer than an endpoint allows (28 days for steps and stress; 31 days for sleep,
  hydration, and Body Battery; a year for daily summaries) are split into several requests
- Model-sized responses: all-day sample arrays (heart rate, SpO2, Body Battery) are left out unless you ask for them

## Install

Requires Go 1.27.3 or later.

```sh
go install github.com/re3turn/garmin-mcp/cmd/garmin-mcp@latest
```

## Authentication

1. Sign in to Garmin Connect on the web and create a key on the
   [Personal API Key](https://connect.garmin.com/app/settings/personalApiToken) page. Keys last from 30 days to
   a year, and Garmin shows each key only once.
2. Save the key. The input shows as `*`; Esc or Ctrl-C cancels.

   ```sh
   garmin-mcp auth
   ```

   Without a terminal, pipe the key in instead (e.g. `pbpaste | garmin-mcp auth`).
3. Run `garmin-mcp auth --check` to confirm the saved key works.

The key is checked with Garmin and then saved to `~/.config/garmin/connect-personal-api-key`
(`%USERPROFILE%\.config\garmin\` on Windows). Bearer tokens are cached in
`connect-personal-api-bearer-token.json` in the same directory.

`GARMIN_PERSONAL_API_KEY`, when set, takes precedence over the key file.

## Add it to your MCP client

Claude Code:

```sh
claude mcp add garmin -- garmin-mcp
```

Codex CLI:

```sh
codex mcp add garmin -- garmin-mcp
```

This writes `~/.codex/config.toml`, which the Codex IDE extension and Codex in the ChatGPT desktop app share,
so the server is available there too.

Claude Desktop, Cursor, and other clients that use the `mcpServers` format:

```json
{
  "mcpServers": {
    "garmin": {
      "command": "garmin-mcp"
    }
  }
}
```

If the `go install` directory (`$(go env GOPATH)/bin`) isn't on your PATH, put the full path in `command`.

## Use it from ChatGPT

ChatGPT connects to MCP servers from OpenAI's side, so it can't start a server on your machine the way the clients
above do. Use OpenAI's [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels):
`tunnel-client` runs on your machine, makes only outbound HTTPS requests to OpenAI, and passes ChatGPT's requests
to this server, so nothing is exposed to the internet.

You need a ChatGPT workspace that allows custom MCP servers (developer mode) and an OpenAI Platform organization
where you can create and use tunnels. See OpenAI's docs for the current plan and permission requirements; the
steps below follow those docs as of October 2026 and haven't been run end to end against ChatGPT.

1. Save your Garmin key with `garmin-mcp auth` (see [Authentication](#authentication)).
2. Create a tunnel in the OpenAI Platform tunnel settings and download `tunnel-client` from
   [openai/tunnel-client](https://github.com/openai/tunnel-client/releases).
3. Start the server in HTTP mode and keep it running:

   ```sh
   garmin-mcp --http      # http://127.0.0.1:8491/mcp
   ```

4. Point the tunnel at it:

   ```sh
   export CONTROL_PLANE_API_KEY="sk-..."   # an OpenAI API key allowed to use the tunnel
   tunnel-client init --profile garmin --tunnel-id <your tunnel_id> \
     --mcp-server-url http://127.0.0.1:8491/mcp
   tunnel-client run --profile garmin
   ```

   `tunnel-client help quickstart` lists the samples `init` accepts. Use HTTP mode rather than letting
   `tunnel-client` start the server over stdio: only HTTP mode leaves out `garmin_download_activity`.
5. In ChatGPT, open Plugins, select **+** and then **Add custom MCP server**. Under **Connection**, choose
   **Tunnel** and select your tunnel. Choose **No authentication**: the server has none of its own, and the
   tunnel is reachable only through your OpenAI workspace and Platform organization. Anyone there who can use
   the app or the tunnel reads your Garmin data, so don't share them.
6. Keep the server and `tunnel-client run` going while you use it; ChatGPT's requests fail while either is
   stopped.

## Remote / HTTP mode

```sh
garmin-mcp --http                       # http://127.0.0.1:8491/mcp
garmin-mcp --http --port 8491 --allowed-host garmin.example.ts.net   # behind tailscale serve
```

This serves streamable HTTP at `/mcp` with every tool except `garmin_download_activity`, which saves a file on
the machine running the server and so is for local clients over stdio only. **There is no built-in
authentication, so never expose it to the internet.** Keep it on `127.0.0.1` and reach it through something that authenticates for you, such as the tunnel
above, a VPN or tailnet (e.g. `tailscale serve`), or an authenticating reverse proxy. Use `--host 0.0.0.0` only
inside a container, and publish the port to the host's `127.0.0.1` only. `127.0.0.1` keeps out other machines,
not other users of this one: on a shared machine, anyone logged in can connect and read your Garmin data.

Requests are accepted only when their `Host` header is `127.0.0.1`, `localhost`, `::1`, or a name passed with
`--allowed-host` (repeatable), and a browser `Origin` header must name one of those hosts too. This keeps web
pages from reaching the server through DNS rebinding. Behind a reverse proxy, pass the hostname clients use with
`--allowed-host`.

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
| `garmin_download_activity` | Save an activity's original file (a ZIP holding the .fit) locally; stdio only |
| `garmin_get_profile` | Profile settings: units, display formats, time zone |
| `garmin_get_heart_rate_zones` | Heart rate zones per sport |
| `garmin_get_fitness_age` | Fitness age and its components |
| `garmin_get_menstrual_cycle` | Menstrual cycle summary |
| `garmin_get_menstrual_day` | One day's logged entries and cycle detail |

Weights are in grams, distances in meters, and durations in seconds, as Garmin returns them.

`/weight-service/weight/all` from the Swagger reference has no tool, because the reference implementation reports
that it returns HTTP 400 for every range. `garmin_get_weight` with `include_all` returns the same entries.

## Unofficial Garmin Connect API (optional)

Training readiness, training plans, and some other data aren't in the Personal API. With `--unofficial`, the
server also reads them from the Garmin Connect API that Garmin's own apps use, following
[python-garminconnect](https://github.com/cyberjunky/python-garminconnect) and
[tamcore/garmin-mcp](https://github.com/tamcore/garmin-mcp). Data the Personal API has is still read through the
Personal API, so these tools only add what it lacks.

**This API is unofficial.** Garmin publishes nothing about it and can change or block it at any time, and using it
may conflict with Garmin's terms of service. That's your decision to make. It also needs your Garmin password
instead of a Personal API key. Without `--unofficial`, none of this is used.

1. Sign in at a terminal. The password input shows as `*`, and an MFA code is asked for when your account uses
   one. The password goes to Garmin only and is never saved. The sign-in pauses a few seconds between steps, as
   a browser does, so it can take up to half a minute.

   ```sh
   garmin-mcp auth --unofficial
   garmin-mcp auth --unofficial --check   # confirm the saved sign-in works
   ```

2. Add `--unofficial` to the server's arguments, e.g. `claude mcp add garmin -- garmin-mcp --unofficial`.
   If there is no saved sign-in, the server says so on stderr when it starts and serves the other tools as usual;
   signing in while it runs takes effect without a restart.

The tokens Garmin returns are saved to `~/.config/garmin/connect-unofficial-tokens.json` with owner-only
permissions and renewed automatically. They give full access to your Garmin account, so keep that file private.
When Garmin refuses to renew them, run `auth --unofficial` again.

Garmin often blocks sign-ins from clients that aren't a browser. python-garminconnect gets past more of these
blocks by imitating a browser's TLS fingerprint, which this Go client can't. If every attempt is blocked, sign in
with python-garminconnect and copy its `~/.garminconnect/garmin_tokens.json` to the path above; the file format is
the same. Don't keep using both tools with the same file: each renewal replaces the refresh token, and the other
tool's copy then stops working.

HTTP mode leaves these tools out unless you also pass `--allow-unofficial-over-http`, because anyone who can reach
the port can use your account's sign-in through them.

| Tool | What it returns |
| --- | --- |
| `garmin_get_training_readiness` | Training readiness score and the factors behind it for a day |
| `garmin_get_training_status` | Training status, training load, and VO2 max as of a day |
| `garmin_get_vo2max` | Daily VO2 max estimates across a range |
| `garmin_get_race_predictions` | Predicted 5K to marathon times: latest, or daily or monthly across a range |
| `garmin_get_endurance_score` | Endurance score across a range, daily or weekly |
| `garmin_get_hill_score` | Daily hill score across a range |
| `garmin_get_running_tolerance` | Running tolerance across a range, daily or weekly |
| `garmin_get_lactate_threshold` | Latest lactate threshold heart rate and speed |
| `garmin_get_cycling_ftp` | Latest cycling FTP |
| `garmin_get_personal_records` | Personal records across sports |
| `garmin_get_respiration` | Respiration rate through a day |
| `garmin_get_stress_timeline` | Stress level through a day (`garmin_get_stress` has daily totals) |
| `garmin_get_floors` | Floors climbed and descended through a day |
| `garmin_get_blood_pressure` | Blood pressure measurements across a range |
| `garmin_get_active_training_plans` | Training plans active on a day |
| `garmin_get_training_plan` | One training plan's phases and workouts |
| `garmin_get_coach_workouts` | Garmin Coach and training plan workouts around a day, usually the current week |
| `garmin_get_scheduled_workouts` | Workouts on the calendar across a range |
| `garmin_get_calendar_month` | One month of the Garmin Connect calendar: workouts, plan days, races, and activities |
| `garmin_list_workouts` | Workouts in the workout library |
| `garmin_get_workout` | One workout's steps and targets |
| `garmin_get_devices` | Devices registered to the account |
| `garmin_get_gear` | Gear such as shoes and bikes |

All of them only read. GETs that change the account, such as the one that adds a training plan to the calendar,
are left out.

## Environment variables

| Variable | Purpose | Default |
| --- | --- | --- |
| `GARMIN_PERSONAL_API_KEY` | Personal API key; takes precedence over the key file | – |
| `GARMIN_MCP_DOWNLOAD_DIR` | Where `garmin_download_activity` saves files; nothing is written outside it. Must be an absolute path or start with `~/`, which expands to your home directory | `~/Downloads/garmin` |
| `GARMIN_PERSONAL_API_BASE_URL` | API base URL (`https://personal.connectapi.garmin.cn` for the China region) | `https://personal.connectapi.garmin.com` |
| `GARMIN_CONNECT_DOMAIN` | With `--unofficial`: the Garmin region, `garmin.com` or `garmin.cn`; nothing else is accepted, since the sign-in sends your password there | `garmin.com` |

## Development

```sh
go test ./...   # no network and no Garmin account needed
go vet ./...
```

Garmin does not publish the OpenAPI spec as a file; the
[Swagger docs page](https://developer.garmin.com/gc-developer-program/personal/swagger-docs/)
embeds it in a JavaScript bundle. To pull it out into `api/openapi.json`:

```sh
go run ./tools/fetch-openapi/   # -o - prints it to stdout instead
```

## License

[MIT](LICENSE). Garmin and Garmin Connect are trademarks of Garmin Ltd. or its subsidiaries.
This project is not affiliated with or endorsed by Garmin.
