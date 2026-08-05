# bazbet_logger

One logging package for the suite. An application initialises its Postgres pool
and reads its Telegram settings, hands both to `logger.Init`, and from then on
logs typed values from anywhere in the tree.

No per-app `packages/logging`. No per-app `packages/telegram` *for logging*
(DAVO keeps its own — that one polls and downloads files, which is a different
job).

## The consumer API

Three types, five functions. That's the whole surface — there are no string
attribute keys, so a typo can't silently drop a value into the wrong column.

```go
logger.Debug(logger.InfoLog{Message: "pool acquired"})
logger.Info (logger.InfoLog{Message: "server started"})
logger.Warn (logger.ErrorLog{Message: "reconnect failed", Error: err})
logger.Error(logger.ErrorLog{Message: "bet failed", Error: err, Request: req, Response: resp})
logger.Bet  (logger.BetLog{Venue: "ASCOT", RaceNumber: 7, RunnerNumber: 3, ...})
```

| Type | Used by | Carries |
|---|---|---|
| `InfoLog` | `Debug`, `Info` | identity, message, `*RaceDetails` |
| `ErrorLog` | `Warn`, `Error` | the above + `Error error`, `Request`/`Response any`, `Trace *string` |
| `BetLog` | `Bet` | the above + endpoint, market, stake, odds, target, result, race state |

`RaceDetails` (venue, race number, runner number, runner name) is shared by all
three and is a pointer — leave it nil on program-level logs and the column stays
NULL.

You never set `Application`, the source location, or the timestamp. They come
from `Config`, the captured call site, and the moment of the call.

`ErrorLog.Message` falls back to `Error.Error()` when empty, so
`logger.Error(logger.ErrorLog{Error: err})` is fine on its own.

`ErrorLog.Trace` is **additive**: the call site is captured either way, so
passing a panic stack adds to the trace column rather than replacing the file
and line.

## Setup

```go
import "github.com/bazcampbell/bazbet_logger"

func main() {
    godotenv.Load()

    db, err := store.New(...)          // app's own pool, as today
    if err != nil { ... }

    tg, err := db.GetAdminTelegramSettings()
    if err != nil {
        slog.Warn("telegram logging disabled", "error", err)
    }

    if err := logger.Init(logger.Config{
        Application:   "pegasus",
        DefaultUserID: adminUserID,
        DB:            db.Pool(),
        Telegram: &logger.TelegramSetup{
            BotToken:          tg.BotToken,
            InfoLogChannelID:  tg.InfoChannelID,
            WarnLogChannelID:  tg.WarnChannelID,
            ErrorLogChannelID: tg.ErrorChannelID,
            BetLogChannelID:   tg.BetChannelID,
        },
    }); err != nil {
        // Non-fatal: only Telegram failed. DB + stderr are live.
        logger.Warn(logger.ErrorLog{Message: "telegram logging unavailable", Error: err})
    }
    defer logger.Stop()   // BEFORE closing the pool — the final batch needs it
}
```

Anything logged before `Init` still reaches stderr — the package installs a
stderr-only logger in `init()`, so config-parsing and pool-setup failures aren't
lost.

## Sinks

| Sink | Enabled by | Behaviour |
|---|---|---|
| stderr | always | `slog.TextHandler`, from process start |
| Postgres | `Config.DB` | bounded queue → background goroutine → batched `INSERT` |
| Telegram | `Config.Telegram` | bounded queue → single sender, rate limited + deduped |

Both async sinks **drop rather than block** when their queue is full. Logging
must never stall the application.

`Init` calls `slog.SetDefault`, so third-party code logging through plain `slog`
reaches stderr. It does *not* reach Postgres or Telegram — those are fed by the
typed API, which a generic `slog.Record` can't satisfy.

## Columns

| Field | Column |
|---|---|
| `UserID` (or `Config.DefaultUserID`) | `user_id` |
| `Username` | `username` |
| `ProcessID` | `process_id` |
| `Message` | `message` |
| `Error` | `error` |
| `RaceDetails` | `race_details` JSONB |
| `BetLog` fields below `RaceDetails` | `bet_details` JSONB |
| `Request` / `Response` | `request` / `response` JSONB |
| captured call site + `ErrorLog.Trace` | `trace` JSONB |

JSONB keys are snake_case (`race_number`, `runner_name`, `target_lia`).
Zero-valued fields are omitted, and an object with nothing in it is written as
NULL rather than `{}` — so `WHERE race_details IS NULL` finds program-level
logs.

`Request`/`Response` take anything — structs, maps, raw JSON strings or
`[]byte`. Values that already parse as JSON are stored verbatim, so you can
pass a raw response body straight through.

## Log level

Read from the environment only: `<APPLICATION>_LOG_LEVEL`, else `LOG_LEVEL`,
else info. Deliberately not in `Config` — it's deployment config, and the
existing `PEGASUS_LOG_LEVEL` / `DAVO_LOG_LEVEL` keep working untouched.

## Telegram

Levels route to channels independently. A nil channel for a level falls back to
`DefaultChannelID`; if that's also nil the level isn't sent. **Debug is never
sent** regardless of config. Set only `DefaultChannelID` to reproduce PEGASUS's
current single-channel behaviour.

Two things keep an error storm from becoming a 429 storm:

- **Rate limiting** — a token bucket per channel (18/min) plus a global one
  (25/s). Telegram's limits are per *bot*, and every app in the suite sends
  through the same token from a different process, so each has to be
  conservative on its own. A `429` reply's `retry_after` is honoured.
- **Dedupe** — identical repeated messages inside `DedupeWindow` (default 60s)
  collapse into one `"+N more"` summary instead of N sends.

## Tuning

`Config.Setup` fields all default sensibly; env vars are the fallback.

| Field | Env | Default |
|---|---|---|
| `QueueSize` | `LOG_QUEUE_SIZE` | 1000 |
| `BatchSize` | `LOG_BATCH_SIZE` | 100 |
| `FlushInterval` | `LOG_FLUSH_INTERVAL_MS` | 5s |
| `TelegramQueueSize` | `LOG_TG_QUEUE_SIZE` | 1000 |
| `DedupeWindow` | `LOG_TG_DEDUPE_MS` | 60s |

## Migrating an app

1. **Run `migrations/001_logs_schema.sql` first.** It adds `username`,
   `race_details` and `bet_details`, backfills them from the old
   `venue_name`/`race_number`/`bet` shape, and adds expression indexes so the
   admin UI's venue/race filters stay fast now that those live inside JSONB.
   Without it every insert fails — the sink reports it on stderr and drops the
   batch rather than crashing, but you'd lose the logs.
2. `go get github.com/bazcampbell/bazbet_logger`
3. Delete `packages/logging/`.
4. Rewrite call sites to the typed API. This is the real work — the old
   variadic form has no automatic translation:

   ```go
   // before
   logger.Error("unable to send bet notification",
       "user_id", p.Settings.UserID, "process_id", p.Settings.ID,
       "venue_name", race.Venue.Name, "race_number", race.RaceNumber,
       "error", err, "request", notification, "response", resp)

   // after
   logger.Error(logger.ErrorLog{
       UserID:    p.Settings.UserID,
       ProcessID: p.Settings.ID,
       Message:   "unable to send bet notification",
       Error:     err,
       Request:   notification,
       Response:  resp,
       RaceDetails: &logger.RaceDetails{
           Venue:      race.Venue.Name,
           RaceNumber: race.RaceNumber,
       },
   })
   ```
5. Replace the `Configure()` / `SetApplication()` / `SetDefaultUserID()` /
   `SetDB()` sequence in `main.go` with the single `logger.Init(...)` above.
6. PEGASUS only: drop `packages/telegram` and the `SetAdminTelegram` wiring in
   `runtime/supervisor.go`.
7. DAVO only: keep `packages/telegram` (it polls and downloads); it just stops
   being used for logging.

### Caveats

- **Telegram config timing.** PEGASUS currently builds its Telegram client
  inside `Supervisor.startLocked()`, so `/api/system/restart` picks up changed
  settings. `Init` reads them once at startup. If you want restart-time reload,
  call `Init` again — it stops the previous logger's sinks before swapping, so
  it doesn't leak goroutines.
- **Private module.** The Dockerfiles run `go mod download` against the network.
  A private repo needs `GOPRIVATE=github.com/bazcampbell/*` and a token in the
  build stage. For local work across the suite, a `go.work` avoids tagging on
  every edit.

## Fixed on the way over

- **Bet payloads were nested.** Call sites passed the whole payload as one
  `"bet"` attr (`PEGASUS/packages/tenant/betting.go:201`). The old logger stored
  that under a `bet` key *inside* the bet JSONB, while the Telegram formatter
  read the top level — so runner, market and target never rendered and every bet
  card was a bare venue line. `RaceDetails` and `BetLog` now write their columns
  directly, with no key-guessing in between.
- **Race context only reached Telegram on bet logs.** The old formatter pulled
  runner and market out of the bet JSONB, so an ERROR during a race showed no
  race at all. `RaceDetails` is shared across all three types, so every level
  renders it.
- **Trace column pointed at the wrong function.** `traceJSON` used
  `runtime.FuncForPC`, which doesn't expand inlined frames, so call sites inside
  inlined functions were attributed to whatever they were inlined into. Now uses
  `runtime.CallersFrames`, as `slog` does. Guarded by
  `TestCallerSourceIsCallSite`.
