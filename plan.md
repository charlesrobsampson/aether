# Project Plan for Aether Integration

## 1. Objective
Develop Aether as a central messaging router that integrates with the existing
messenger implementation (`msg`) and satellite services such as
`wind-predictor`. Ensure seamless message routing, multi-step session handling,
and extensibility for future satellite services.

## 1.1 Source of Truth & Conventions

This plan is the **single source of truth**. `manifest.md` is background design
material; where the two disagree, this plan wins. `checklist.md` is the
execution order and must not introduce contracts that are not defined here.

| Convention | Value |
|---|---|
| Module path | `github.com/charlesrobsampson/aether` |
| Go version | as pinned in `go.mod` (currently 1.27.3) |
| Binaries | `cmd/aether` (router), `cmd/...` for any helper tools |
| Default branch | `master` |
| Internal packages | `internal/{app,config,model,auth,router,session,store,queue,transport,metrics,api,scheduler,testutil,integration,deploy,docs,satellite/...}` |
| Public SDK | `pkg/sdk` (importable by satellites, including `wind-predictor`); owns command parsing, arg conversion, timezone conversion, patching |
| Config root | `AETHER_CONFIG_DIR`, default `~/.config/aether`; always injected, never hard-coded in tests |
| Config format | JSON only |
| Time | all persisted timestamps are UTC RFC3339; user-local conversion happens at the parser boundary |
| Tests | table-driven where a table adds signal; hermetic (no `$HOME`, no network, no sleeps) |

**Verification protocol** (applies to every checklist task):

```bash
go test -v ./<pkg> -run '^TestName$'   # must print --- PASS: TestName
go test ./...                          # full suite, no regressions
gofmt -l . && go vet ./...             # no output
```

A task is not complete until the named test shows `--- PASS`. `go test -run` with
no matching test exits 0, so `-v` and the `--- PASS` line are mandatory evidence.

## 2. High-Level Milestones

Owners/ETAs are carried over from the original draft and are indicative; re-baseline
them after T01.

| Milestone | Description | Owner | ETA |
|---|---|---|---|
| **M1** – Core Router Setup | Config loading, inbound receiver, routing engine, outbound queue, bulk/group send. | core team | 2 weeks |
| **M2** – Messenger Integration | `msg` exposes a reusable Messenger facade; inbound forwarding into `/receive`. | `msg` maintainer | 1 week |
| **M3** – Satellite SDK | `ParseWebhook`, `SendResponse`, `SessionManager`, typed `CommandSpec`, JSON-Patch helper. | SDK lead | 1 week |
| **M4** – Built-in Test/Reminder Satellite | Echo command and relative/absolute reminders. | core team | 1 week |
| **M5** – Wind-Predictor Integration | Alerts delivered through Aether via the SDK; config endpoint; text-friendly prediction command. | integration lead | 1 week |
| **M6** – Config & Admin Satellites | Config-manager (user patches) and Admin-manager (approval workflow, roles). | config engineer | 1 week |
| **M7** – Satellite Registration | Protected registration, trigger uniqueness, route regeneration. | admin/maintainer | 1 week |
| **M8** – Sessions & Stateful Threads | TTL session store, `#thread-id` targeting, lock/extend/close lifecycle. | core team | TBD |
| **M9** – Persistence | SQLite (modernc, CGO-free) with WAL, route log, registry, patches, approvals. | core team | TBD |
| **M10** – Observability | `/healthz`, `/metrics` (Prometheus), structured logs, bulk/outbound counters. | ops | 1 week |
| **M11** – End-to-End Tests & CI | Cross-package integration tests and GitHub Actions. | QA | 1 week |
| **M12** – Documentation & Packaging | README, "add a satellite" guide, Dockerfile, compose. | docs | 3 days |

## 3. Canonical Contracts

### 3.1 HTTP Endpoints

| Endpoint | Caller | Auth | Success | Errors |
|---|---|---|---|---|
| `POST /receive` | transport (`msg`, webhook) | loopback + optional `X-Aether-Token` | `202` | `400` malformed, `401` bad token |
| `POST /message` | satellites, internal scheduler | JWT (`admin` or `maintainer`) | `202` | `400`, `401`, `403` |
| `POST /register-satellite` | admin-manager / operators | JWT (`admin` or `maintainer`) | `201` | `400`, `401`, `403`, `409` conflict |
| `GET /healthz` | ops | none (loopback) | `200` | `503` |
| `GET /metrics` | ops | none (loopback) | `200` | — |

All endpoints are registered on one `http.ServeMux` owned by `internal/api`; the
router never registers handlers itself. `cmd/aether` is the only place that
binds a port.

### 3.2 `POST /receive` (transport → Aether)

```json
{
  "sender": "+1234567890",
  "text": "!watch sci-fi",
  "provider": "signal",
  "group_id": "optional",
  "message_id": "optional-upstream-id"
}
```

- `sender` and `text` are required. **Denied** senders are acknowledged with
  `202` and dropped silently (no error, no outbound). **Unknown** (not yet
  approved) senders trigger the admin-manager approval request exactly once per
  phone (deduplicated against pending approvals) and are dropped until approved.

### 3.3 `POST /message` (satellite/scheduler → Aether)

```json
{
  "recipients": ["+1234567890", "+1987654321"],
  "group_chat_id": "optional",
  "text": "…",
  "source": "satellite",
  "satellite": "wind-predictor",
  "thread_id": "optional",
  "session": { "action": "lock", "ttl_seconds": 300 }
}
```

- `recipients` is canonical and may contain one or many entries. A single-recipient
  send is `"recipients": ["+1…"]`; there is no separate `recipient` field.
- `group_chat_id` and `recipients` are mutually exclusive (`400` if both).
  A group send is one outbound job addressed to the group conversation, and the
  group must appear in `approved_groups`.
- Denied recipients are dropped silently: the request still returns `202`, but no
  job is enqueued for them.
- One request produces N outbound jobs but **one** increment of
  `aether_bulk_message_requests_total` (when `len(recipients) > 1`) and N
  increments of `aether_outbound_jobs_total`.
- `session.action` is one of `lock`, `extend`, `close` (empty = no session change).
  `thread_id` defaults to the satellite name when a session action is present.
- `text` is required unless a session action is present.

### 3.4 Authentication (JWT)

- Algorithm HS256. Secret from `auth.jwt_secret` in `system.json`, overridable by
  `AETHER_JWT_SECRET`.
- Header: `Authorization: Bearer <token>`.
- Claims: `{"sub": "+1…", "role": "admin"|"maintainer", "exp": <unix>}`.
- Missing/invalid token → `401`; valid token with insufficient role → `403`.
- `/receive` is not JWT-protected because transports do not hold user roles; it is
  bound to loopback and optionally guarded by `AETHER_INGRESS_TOKEN`.
- Tokens are minted with `aether token --sub +1… --role admin` (a small `cmd/aether`
  subcommand) so operators and tests do not need an external issuer.

### 3.5 Roles & Capabilities

Roles are **linear**: `Denied(0) < User(1) < Admin(2) < Maintainer(3)`.
`CheckAccess(phone, needed) bool` is `role(phone) >= needed`. Notification routing
is a separate concern and does not affect `CheckAccess`.

| Capability | User | Admin | Maintainer |
|---|---|---|---|
| Edit own `users/<phone>.json` | ✅ | ✅ | ✅ |
| Edit any user config | — | ✅ | ✅ |
| Register/modify satellites | — | ✅ | ✅ |
| Manage `admin.json` (admins, approved/denied lists) | — | ✅ | ✅ |
| Receive rotating approval requests | — | — | ✅ |
| Mute/reroute system notifications | — | — | ✅ |

- `admin.json` is the access-control file:
  `{"admins": [...], "approved_numbers": [...], "denied_numbers": [...], "approved_groups": [...]}`.
  A number in `denied_numbers` is `Denied` even if it also appears in `approved_numbers`.
- `maintainer.json`: `{"maintainers": [...], "last_assigned": 0, "notify_approvals": true}`.
- Approval requests rotate: `maintainer = maintainers[last_assigned % len(maintainers)]`
  and `last_assigned` increments **after** a successful write.
- Pending requests expire after **7 days**; a later message from the same number
  starts a new request (the expired row stays in `approvals` with status `expired`).
- Approval is idempotent: approving an already-approved number is a no-op `200`.

### 3.6 Command Parsing

- Prefixes (`!`, `/`, `Aether `) are stripped first, from `system.json` plus the
  user's `prefixes`.
- The first token selects a satellite by `trigger` (user `satellite_aliases` are
  resolved before the global lookup). Unknown trigger → no route, `404` is not
  returned to the user; the message is logged and dropped.
- Patterns match the text **including the trigger**, so
  `trigger: "remind"` + pattern `"remind me <msg> in <num> <unit>"` matches the
  full command. Patterns are anchored (`^…$`), whitespace runs match `\s+`, and
  each `<arg>` compiles to a lazy `(?P<arg>.+?)` group.
- Ambiguous patterns are resolved in declaration order (first match wins).
- Built-in commands (`config`, `admin`, `remind`, `test`) are registered as
  internal satellites and take precedence over file-registered triggers with the
  same name. A file registration that collides with a built-in name is `409`.

| Command | Minimum role | Behavior |
|---|---|---|
| `!config set <key> <value>` | User (self) | patch own `users/<phone>.json` |
| `!config set @<phone> <key> <value>` | Admin | patch another user |
| `!config set <satellite>.<key> <value>` | Admin | forward patch to the satellite |
| `!approve <phone>` / `!deny <phone>` | Maintainer | resolve a pending request |
| `!pending` | Maintainer | list pending requests |
| `!admin add <phone>` / `!admin remove <phone>` | Admin | edit `admins` |
| `!remind ...` | User | relative/absolute reminder |
| `!test` | User | echo `test received` |
- `MatchCommand(input string, spec CommandSpec) (map[string]string, bool)` returns
  the raw strings. `ConvertArgs(spec CommandSpec, raw map[string]string, loc *time.Location) (map[string]any, error)`
  applies `ArgDef.Type` (`string`, `int`, `enum`, `datetime`), validates enum
  membership, and converts datetimes to UTC. The payload posted to the satellite
  contains the converted values.
- Supported datetime input layouts, tried in order: RFC3339, `2006-01-02 15:04`,
  `2006-01-02T15:04`, `15:04` (today in the user's timezone), and
  `Monday 15:04` (next occurrence). `import _ "time/tzdata"` is mandatory so
  containers without zoneinfo still resolve `America/Chicago`.
- Aether POSTs this envelope to `endpoint + path`; satellites parse it with
  `sdk.ParseWebhook`:

  ```json
  {
    "command": "remind",
    "trigger": "remind",
    "args": { "msg": "call bob", "num": 5, "unit": "minutes" },
    "text": "remind me call bob in 5 minutes",
    "sender": "+1234567890",
    "satellite": "reminder",
    "thread_id": "optional"
  }
  ```

  `args` contains the converted (typed) values; metadata fields are always
  present so a satellite can reply to the right user/thread.

### 3.7 Sessions & Threads

`internal/session` holds an in-memory store with an injectable `Clock`.

- Key: `(phone, satellite)` plus an optional thread ID.
- `Start/Get/Extend/Close`; default TTL 5 minutes, configurable per satellite.
- Inbound text from a phone with an active session is routed to that satellite
  **before** trigger parsing.
- `#thread-id` inside the message (e.g. `#jellyfin 2`) targets a specific thread;
  without a tag, the most recently active session for that phone wins.
- Sessions are intentionally not persisted; SQLite only records audit events.

### 3.8 Storage (SQLite)

`modernc.org/sqlite`, WAL mode, `busy_timeout=5000`, single writer connection.

| Table | Purpose |
|---|---|
| `route_log` | inbound/outbound audit: direction, sender, recipient, satellite, thread, text, status, error, created_at |
| `satellites` | registry mirror: name, trigger, endpoint, config JSON, registered_by, timestamps |
| `patches` | config patches: phone, patch JSON, applied_by, applied_at |
| `approvals` | pending/resolved new-number requests: phone, maintainer, status, requested_at, resolved_at, resolved_by |

`store.Open(path)` runs migrations on startup. `:memory:` is used by tests.

### 3.9 Outbound Queue

- `internal/queue` exposes `Enqueue(job)`, a buffered channel, and a worker pool.
- Workers call `transport.Messenger.Send(ctx, recipient, text)`.
- A per-provider token bucket enforces rate limits (default 1 msg/s, burst 5,
  overridable via `limits` in `system.json`); a per-user `muted` flag in
  `users/<phone>.json` drops jobs at enqueue time (counted, not errored).
- The queue is bounded (default depth 256) and drained on shutdown with a bounded
  wait.

### 3.10 Messenger & `msg` Integration

`msg` stays a CLI. Add a narrow library facade without changing existing UX:

```go
// github.com/charlesrobsampson/msg/client
type Messenger interface {
    Send(ctx context.Context, recipient, text string) error
    Name() string
}

type MessengerConfig struct {
    Provider string // "signal" or "openmessage"
    Account  string // signal account, e.g. "+1234567890"
    BaseURL  string // signal-cli REST base URL
    DBPath   string // optional local cache used by the provider
}

func NewMessenger(cfg MessengerConfig) (Messenger, error)
```

- `Send` maps `recipient` (phone number or conversation ID) onto the existing
  `Provider.SendMessage(recipient, text, false)` and normalizes its
  `(bool, error)` result: `false` with no error becomes an error.
- `MessengerConfig.DBPath` is honoured through a new `OpenSignalDBAt(path)` and
  `NewSignalProviderWithDB(...)`; an empty `DBPath` keeps the existing
  `OpenSignalDB()` behavior. This seam exists so tests never touch `$HOME`, and it
  does not change the CLI.
- Aether defines its own `transport.Messenger` interface with the same shape and
  ships an adapter over `msg/client.Messenger`.
- Outbound: Aether → `msg` library → signal-cli REST / openmessage.
- Inbound: `msg signal-receiver` gains `--forward URL` (env `MSG_FORWARD_URL`) that
  POSTs normalized `{sender, text, provider, group_id, message_id}` to
  `/receive` with an optional `X-Aether-Token` header. Forwarding is best-effort:
  failures are logged and retried with bounded backoff, and they never block the
  existing DB write. The existing `/api/signal/webhook` behavior is unchanged when
  the flag is absent.
- Aether depends on `msg` via a `replace`/`go.work` entry during development.


### 3.11 Satellite SDK (`pkg/sdk`)

- `ParseWebhook(r *http.Request) (*Transmission, error)`
- `SendResponse(w http.ResponseWriter, resp Response)` where `Response` marshals
  to the `/message` envelope (recipients, text, satellite, thread_id, session).
- `NewClient(baseURL, token string) *Client` and `Client.Send(ctx, Response) error`
  for satellites (including `wind-predictor`) that push outbound messages.
- `SessionManager` with `Lock`, `Extend`, `Close` helpers that emit the
  `session.action` envelope.
- `CommandSpec`, `ArgDef`, `MatchCommand`, `ConvertArgs` (shared with the router;
  the router imports `pkg/sdk`, satellites import `pkg/sdk`).
- `Patch` helpers for RFC6902 config updates (`add`, `replace`, `remove`,
  `test` on JSON Pointer paths), implemented in `pkg/sdk/patch.go`; no external
  JSON-Patch dependency.

### 3.12 Wind-Predictor Integration

`wind-predictor` already has the scheduler we need: `workflow.Service` ticks every
30 s and dispatches through `alerts.Notifier`. No new scheduler is required.

- **Alerts:** add an `AetherNotifier` implementing `alerts.Notifier` that formats
  `AlertPayload` as text and posts to Aether `/message` via `pkg/sdk`. It uses a
  new `aether_recipient` field on `UserConfig` (the phone number Aether knows);
  an empty value is a skipped delivery, not a crash. Errors are returned so the
  existing workflow records a failed delivery; the notifier has a bounded HTTP
  timeout and no retries of its own.
- **Config:** add `POST /api/v1/config` accepting a **user-scoped** RFC6902 patch:

  ```json
  {
    "user": "alice",
    "patch": [
      { "op": "replace", "path": "/subscriptions/0/personal_constraints/no_fly_days", "value": ["Sunday"] }
    ]
  }
  ```

  Only user-specific paths are allowed: `/display_name`,
  `/subscriptions/*/personal_constraints/{window_start_local,window_end_local,no_fly_days}`,
  `/alert_timing/*`, and
  `/alert_rules/*/{min_desirability,min_confidence,priority,sound}`.
  Reject `/alert_channels` (credentials), subscription station/profile identity,
  and every global key. Apply the patch to a copy, run `config.ValidateUser`,
  write `users/<user>.json` atomically, and return `204`. Invalid patch or
  validation failure → `400`; unknown or disallowed path → `422`. The endpoint
  receives an injected `UserStore` rooted at a config dir (tests use
  `t.TempDir()`), because `GetConfigDir` currently hard-codes
  `~/.config/wind-predictor`. Aether's config-manager resolves the sender to a
  wind-predictor user via `satellite_ids` and forwards the patch; wind-predictor
  owns schema validation. The endpoint binds to loopback.
- **Predictions:** add `POST /api/v1/predict/text` accepting the standard
  Aether dispatch envelope (`{"command":"fly","args":{"station":"FPS"},...}`,
  parsed with `sdk.ParseWebhook`) and returning `{"text": "..."}` optimized for
  messaging (short lines, no tables, mph/°F), with `404` for an unknown station
  and `503` when no model is loaded. POST (not GET) so Aether's uniform JSON
  dispatch can call it. Register a satellite `wind-predictor` in Aether with
  `trigger: "fly"`, pattern `fly <station>`, and
  `path: "/api/v1/predict/text"`. The existing JSON endpoints stay unchanged.
- **Config routing:** Aether's config-manager treats a `<satellite>.<key>` target
  as satellite-owned config, looks up the sender's `satellite_ids[<satellite>]`,
  and forwards `{user, patch}` to that satellite's `POST /api/v1/config`;
  otherwise it edits the local user file. The role check always happens in
  Aether.
- No changes to the prediction/training algorithms or existing routes.


### 3.13 Scheduler

- `internal/scheduler` uses `robfig/cron` **only to compute** the next activation
  of a 5-field calendar schedule (minimum granularity: 1 minute). Firing is driven
  by the injectable clock (`Clock.AfterFunc`), not robfig's internal goroutine, so
  tests advance a fake clock instead of sleeping.
- Relative reminders (`in <num> seconds|minutes|hours|days`) use `time.AfterFunc`
  against the injectable clock; `seconds` is therefore supported.
- Absolute reminders (`at <datetime>`) convert to a single `AfterFunc` at the
  computed instant. Cron is only for repeating schedules.
- Repeating satellite triggers come from `schedules` in `system.json`:
  `{"name":"morning_fly","cron":"0 7 * * 1-5","satellite":"wind-predictor","command":"fly","args":{"station":"FPS"}}`.
  On fire, Aether dispatches the named command as if the user had sent it,
  using the satellite's configured endpoint/path.

### 3.14 Configuration Layout

All JSON under `AETHER_CONFIG_DIR` (default `~/.config/aether`):

```
~/.config/aether/
├─ system.json            # server, auth, providers, persona, access defaults
├─ maintainer.json        # maintainers, last_assigned, notify_approvals
├─ admin.json             # admins, approved_numbers, denied_numbers, approved_groups
├─ users/
│   └─ +1234567890.json   # prefixes, aliases, timezone, muted
├─ satellites/
│   ├─ wind-predictor.json
│   └─ reminder.json
└─ routes.json            # regenerated from satellites/* (never appended)
```

`system.json` keys used by the checklist:

```json
{
  "server": { "port": 8080, "db_path": "./data/aether.db" },
  "persona": { "name": "Aether", "prefixes": ["!", "/", "Aether "] },
  "auth": { "jwt_secret": "dev-only-change-me" },
  "limits": { "rate_per_second": 1, "burst": 5, "queue_depth": 256 },
  "default_provider": "signal",
  "providers": {
    "signal": { "type": "msg", "account": "+1234567890" }
  },
  "schedules": [
    { "name": "morning_fly", "cron": "0 7 * * 1-5", "satellite": "wind-predictor", "command": "fly", "args": { "station": "FPS" } }
  ]
}
```

`routes.json` is rebuilt from `satellites/*.json` after every registration or
deletion; registration never appends to a stale file. Shape:

```json
{
  "routes": [
    {
      "trigger": "fly",
      "satellite": "wind-predictor",
      "endpoint": "http://127.0.0.1:9463",
      "commands": [
        { "name": "fly", "path": "/api/v1/predict/text", "patterns": ["fly <station>"] }
      ]
    }
  ]
}
```

`users/<phone>.json`:

```json
{
  "prefixes": ["!"],
  "timezone": "America/Chicago",
  "muted": false,
  "satellite_ids": { "wind-predictor": "alice" },
  "aliases": {
    "satellites": { "wind": "wind-predictor" },
    "commands": { "w": "fly" }
  }
}
```

`satellite_ids` maps an Aether phone to a satellite's own user ID and is
admin-managed; it is **not** part of the user `!config set` whitelist.

`/metrics` uses a hand-written Prometheus text exposition (counters and gauges);
no `prometheus/client_golang` dependency.

### 3.15 Satellite Registration

- Payload is the satellite document in §3.6.
- Validate: `name` and `trigger` non-empty and unique; `name` matches
  `^[a-z0-9][a-z0-9-]*$` (prevents path traversal); `endpoint` is an absolute
  `http(s)` URL; every command has at least one pattern and a leading-slash path.
- Write `satellites/<name>.json` atomically (temp file + rename), mirror the row
  into SQLite, then regenerate `routes.json`.
- Conflict on duplicate `trigger` or `name` → `409`.

### 3.16 Test & Reminder Satellite

- Built-in handlers (not HTTP servers): `test` echoes the request text back
  through `/message` (`test received`).
- `remind` supports relative (`in <num> <unit>`) and absolute (`at <datetime>`)
  schedules via the scheduler.
- A reminder fires exactly one `/message` back to the originating user.

### 3.17 Testing & CI

- `go test ./...` per repo; `gofmt -l` and `go vet ./...` must be clean.
- Unit tests use fakes for `Messenger`, `Clock`, and satellite HTTP servers
  (`httptest`).
- Integration tests start Aether in-process with `t.TempDir()` config and SQLite
  `:memory:`; no external services and no real `msg`/signal-cli.
- GitHub Actions runs build, vet, test for `aether`, `msg`, and `wind-predictor`.

### 3.18 Deployment

- Primary target is a single host; `docker-compose.yml` is for local development.
- Multi-stage Dockerfile (builder → Alpine, not scratch, so zoneinfo and CA certs
  exist; `time/tzdata` is imported regardless).
- `signal-cli`/`msg` run as sidecars or host processes; Aether only needs the
  messenger endpoint/credentials.

## 4. Risks & Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| `msg` API drift | Integration delays | Narrow `Messenger` facade + adapter tests |
| SQLite locking under load | Message loss or stalls | WAL, `busy_timeout`, single writer, bounded queue |
| Scheduler drift / DST | Missed reminders | UTC storage, injectable clock, timezone tests |
| Unauthorized config changes | Security breach | JWT roles, path-safe registry writes, access checks in config-manager |
| Unbounded outbound retries | Provider bans | Token bucket, capped retries, dead-letter log |

## 5. Acceptance Criteria

- `aether` binary starts from a config dir containing only `system.json`, serves
  `/healthz`, and routes a test `/receive` message to a satellite.
- `msg` sends outbound messages through its existing provider path and forwards
  inbound messages to `/receive`.
- Satellites can be registered, receive triggers, and respond with typed args.
- Bulk `/message` expands to individual jobs with one bulk metric increment.
- Multi-step sessions lock, extend, close, and support `#thread-id`.
- `wind-predictor` sends scheduled alerts through Aether and exposes config and
  text-prediction endpoints.
- `go test ./...`, `go vet ./...`, and `gofmt -l .` are clean in CI.
- README documents adding a satellite and configuring triggers.

## 6. Resolved Decisions

| Decision | Resolution |
|---|---|
| `msg` integration | Library `Messenger` facade + `signal-receiver --forward URL`; CLI UX unchanged |
| Wind-predictor config | RFC6902 patch; role check in Aether, schema validation in wind-predictor |
| Prediction piping | `/api/v1/predict/{station}/text` in wind-predictor, trigger `fly` in Aether |
| `/message` auth | JWT (HS256, Bearer) on all satellite/scheduler traffic |
| Approval TTL | Pending requests expire after 7 days |
| Roles | Linear `Denied < User < Admin < Maintainer`; notification routing separate |
| Config root | `AETHER_CONFIG_DIR` + explicit `root` argument; tests use `t.TempDir()` |
| Reminders | `AfterFunc` for relative/absolute, `robfig/cron` for repeating schedules |
| Config format | JSON only (manifest's YAML is superseded) |
| `msg` forward failures | HTTP POST + bounded retry; no spool; msg's DB remains the replay source |
| Approved groups | Opaque conversation ID, matched literally |
| User `!config set` keys | `timezone`, `prefixes`, `muted`, satellite/command aliases; whitelist is extensible |
| Wind-predictor config | User files only: no-fly days, personal windows, alert timing, per-rule thresholds; no credentials/global keys |
| Service token | `AETHER_TOKEN` env first, 0600 `aether.token` file fallback |
| CI | Independent per-repo CI; Aether CI generates a `go.work` after checking out siblings |
| Rate limits | 1 msg/s per provider, burst 5, queue depth 256, configurable under `limits` |
| Session actions | `text` optional for `lock`/`extend`/`close` |

---
*Prepared based on the architecture manifest and latest user feedback.*
