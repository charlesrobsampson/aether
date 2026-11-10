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
| `POST /receive` | transport (`msg`, webhook) | loopback + optional `X-Aether-Token` | `202` | `400` malformed, `401` bad token, `429` queue full |
| `POST /message` | satellites, internal scheduler | JWT (`admin` or `maintainer`) | `202` | `400`, `401`, `403`, `429` queue full |
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
  identity (deduplicated against pending approvals) and are dropped until
  approved. An identity may be a phone number or a Signal UUID; when the
  transport also supplies `sender_number`, it is preferred for authorization.
- The handler validates, deduplicates, and enqueues the message onto a bounded
  inbound dispatch queue, then returns `202`; it does not wait for a satellite.
  When the queue is full it returns `429` with `Retry-After: <seconds>`.
- When `message_id` is present, `(sender, message_id)` is deduplicated for a
  configurable TTL (default 15 minutes). A duplicate returns `202` and is not
  dispatched again. An absent `message_id` skips dedupe.

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
- The handler enqueues the whole batch atomically. If the outbound queue cannot
  accept every job it returns `429` with `Retry-After` and enqueues none.

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
| `!approve <phone|uuid>` / `!deny <phone|uuid>` | Maintainer | resolve a pending request |
| `!pending` / `!admin list` | Maintainer | list pending requests |
| `!admin approve|deny <phone|uuid>` | Maintainer | resolve a pending request |
| `!admin add|remove <phone|uuid>` | Admin | edit `admins` |
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
    "thread_id": "optional",
    "message_id": "optional-upstream-id"
  }
  ```

  `args` contains the converted (typed) values; metadata fields are always
  present so a satellite can reply to the right user/thread. `message_id` is
  copied verbatim from the inbound transport payload when present, omitted
  otherwise, and recorded in `route_log` so a satellite and Aether share one
  correlation identifier (A04).

### 3.7 Sessions & Threads

`internal/session` holds an in-memory store with an injectable `Clock`.

- Key: `(phone, satellite)` plus an optional thread ID.
- `Start/Get/Extend/Close`; default TTL 5 minutes, configurable per satellite.
- Inbound text from a phone with an active session is routed to that satellite
  **before** trigger parsing.
- `#thread-id` inside the message (e.g. `#jellyfin 2`) targets a specific thread;
  without a tag, the most recently active session for that phone wins.
- A message intercepted by an active session is matched against that satellite's
  configured command patterns **without** a trigger: a satellite declares a
  reply pattern such as `<choice>` on the command that should handle session
  input. The matched command's `endpoint + path` receives the standard envelope
  with `thread_id` set. If no command matches, the message is logged and dropped.
  The leading `#thread-id` token is removed before matching.
- Session actions (`lock`/`extend`/`close`) arrive on `/message`; an empty
  `thread_id` defaults to the satellite name, and an unknown action is rejected.
- Sessions are intentionally not persisted; SQLite only records audit events.

### 3.8 Storage (SQLite)

`modernc.org/sqlite`, WAL mode, `busy_timeout=5000`, single writer connection.

| Table | Purpose |
|---|---|
| `route_log` | inbound/outbound audit: direction, sender, recipient, satellite, thread, text, status, error, message_id, created_at |
| `inbound_dedupe` | inbound idempotency keys: sender, message_id, seen_at (primary key `(sender, message_id)`) |
| `satellites` | registry mirror: name, trigger, endpoint, config JSON, registered_by, timestamps |
| `patches` | config patches: phone, patch JSON, applied_by, applied_at |
| `approvals` | pending/resolved new-number requests: phone, maintainer, status, requested_at, resolved_at, resolved_by |

`store.Open(path)` runs migrations on startup. `:memory:` is used by tests.
Migrations add columns idempotently so an existing database keeps working.
`inbound_dedupe` rows older than the dedupe TTL (default 15 minutes, §3.19) are
purged opportunistically; the table is not a permanent message archive.

### 3.9 Outbound Queue

- `internal/queue` exposes `Enqueue(job)` and `EnqueueBatch(jobs)`, a bounded
  channel, and a worker pool. `Enqueue` never blocks a caller: when the channel
  cannot accept the job it returns `ErrFull` immediately.
  `EnqueueBatch` is atomic — either every job is accepted or none is, and a
  caller that cannot enqueue the whole batch gets `ErrFull` (A05).
- Workers call `transport.Messenger.Send(ctx, recipient, text)` with a per-attempt
  deadline (`limits.send_timeout_seconds`, default 10s) derived from the queue's
  shutdown context. Shutdown cancels in-flight sends; it never waits forever
  (A02).
- A per-provider token bucket enforces rate limits (default 1 msg/s, burst 5,
  overridable via `limits` in `system.json`); a per-user `muted` flag in
  `users/<phone>.json` drops jobs at enqueue time (counted, not errored).
- Failed sends are retried up to `limits.send_attempts` (default 3) with
  exponential backoff (`limits.retry_backoff_ms`, default 500ms, capped at 30s).
  Only transport/network errors, `429`, and `5xx` are retryable; `400`, `401`,
  `403`, `404`, and `409` are terminal. A `429` response honours `Retry-After`
  when present (A05).
- A job that exhausts its attempts is dead-lettered: one `route_log` row with
  `status = "dead_letter"` and the last error, plus `aether_dead_letter_total`.
  Dead-lettering is best-effort and never blocks the worker (A05).
- The queue is bounded (default depth 256). On graceful shutdown it drains within
  a bounded wait; jobs still buffered after the deadline are counted as dropped
  and the queue's only worker exits promptly (A02).
- The default is one worker, which preserves per-provider delivery order. More
  workers are allowed only with per-provider sharding; changing the default is
  deferred until throughput requires it.

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
- Inbound: forwarding is **opt-in per `msg` profile** and disabled by default.
  Each `~/.config/msg/config-<profile>.json` (or `config.json` for the default
  profile) may contain:

  ```json
  "forwarding": {
    "enabled": true,
    "url": "http://127.0.0.1:8080/receive"
  }
  ```

  Only a profile with `enabled: true` and a non-empty `url` forwards its
  normalized `{sender, text, provider, group_id, message_id}` POST to `/receive`.
  `msg` reads the active profile's config for each inbound message, so toggling
  forwarding takes effect without restarting the receiver. The ingress token is
  read only from `AETHER_INGRESS_TOKEN` and is never stored in config. The
  inbound message and a `forward_outbox` row commit in one SQLite transaction
  before the signal-cli webhook ACK; a failed durable write returns `500` so the
  transport retries (A07). Forwarding is then handed to a bounded per-profile
  worker, so the ACK does not wait for Aether (A03). The worker retries network
  errors, `429`, and `5xx` with bounded backoff, honours `Retry-After`, and
  treats `400`/`401`/`403`/`404`/`409` as terminal (A02). When the forward queue
  is full the newest forward is counted in
  `msg_forward_dropped_total{reason="queue_full"}` and stays pending in the
  outbox for the next receiver start or `msg replay`; it never blocks ingestion
  (A07).
  When `signal-receiver` serves multiple accounts, it resolves each account to
  its profile with `FindProfileForAccount` and applies that profile's setting.
  The existing `/api/signal/webhook` behavior is unchanged. The legacy `--forward`
  / `MSG_FORWARD_URL` enable path is removed (A01).
- Outbound provider calls use one shared `http.Client` with a bounded timeout
  (default 10s) and a request context, so a hung signal-cli or openmessage
  process cannot pin Aether's only queue worker (A02).
- `signal-receiver` runs an `http.Server` with `ReadHeaderTimeout`, `ReadTimeout`,
  and `WriteTimeout` rather than bare `http.ListenAndServe` (A02).
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
  "limits": {
    "rate_per_second": 1,
    "burst": 5,
    "queue_depth": 256,
    "dispatch_queue_depth": 256,
    "send_timeout_seconds": 10,
    "send_attempts": 3,
    "retry_backoff_ms": 500,
    "dedupe_ttl_seconds": 900
  },
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

`/metrics` uses a hand-written Prometheus text exposition (counters, gauges, and
summaries); no `prometheus/client_golang` dependency. §3.19 lists the reliability
metric families.

`limits` values are optional; omitted fields take the defaults shown. They are
read when the app starts and are not hot-reloaded. `dispatch_queue_depth` bounds
the inbound dispatch queue (§3.2), `send_timeout_seconds` bounds one outbound
send attempt, `send_attempts` is the total attempt count including the first,
`retry_backoff_ms` is the base for exponential backoff, and
`dedupe_ttl_seconds` is the inbound idempotency window.

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

### 3.19 Delivery Semantics & Observability

**Delivery semantics**

- **Inbound is at-least-once.** `msg` persists the message, then enqueues the
  forward; a retry may deliver the same message twice. When `message_id` is
  present, Aether dedupes `(sender, message_id)` for `limits.dedupe_ttl_seconds`
  (default 900). A duplicate returns `202` and is not dispatched again. A missing
  `message_id` skips dedupe and is documented as best-effort.
- **`/receive` means accepted, not dispatched.** It validates, authenticates,
  dedupes, enqueues onto the bounded inbound dispatch queue, and returns `202`.
  A bounded dispatch worker calls `router.Dispatch`. A full queue returns `429`
  with `Retry-After`; the caller must retry with backoff (A03, A04).
- **Outbound is at-least-once within bounded attempts.** `/message` enqueues the
  whole batch atomically before returning `202`. If the queue cannot accept every
  job it returns `429` with `Retry-After` and enqueues none. After
  `limits.send_attempts`, a job is dead-lettered rather than retried forever
  (A05).
- **Retry classification is shared by both repos.** Retry transport/network
  errors, `429`, and `5xx`. Never retry `400`, `401`, `403`, `404`, or `409`.
  `429` is the only retryable `4xx`; when `Retry-After` is present, wait at least
  that long. Backoff is exponential with jitter, capped at 30s.
- **Inbound forwarding is durable; Aether's queues are not.** `msg` commits the
  message and one `forward_outbox` row before its webhook ACK, replays pending
  rows when an account DB opens or via `msg replay`, and deletes the row when
  the worker finishes. A crash can duplicate an in-flight forward, which Aether's
  `(sender, message_id)` dedupe absorbs. Aether's inbound and outbound queues
  remain in memory, so queued Aether work is lost on a crash (A07).

**Correlation**

`message_id` flows from the transport payload into the satellite envelope and
`route_log`. Log lines for receive, dispatch, forward, retry, and dead-letter
include `message_id` when present plus the satellite/profile name. Phone numbers
and message IDs are never used as metric labels.

**Metrics**

All metrics use the hand-written Prometheus text exposition in
`internal/metrics` (Aether) and an equivalent small registry in `msg`. Latency is
exposed as a summary (`_sum` and `_count`; no client-side quantiles). `/metrics`
stays loopback-only.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `aether_receive_requests_total` | counter | `result` | `/receive` outcomes: `accepted`, `duplicate`, `queue_full`, `bad_request`, `unauthorized` |
| `aether_inbound_messages_total` | counter | — | accepted inbound messages (existing) |
| `aether_inbound_queue_depth` | gauge | — | inbound dispatch queue occupancy |
| `aether_dispatch_duration_seconds` | summary | `status` | `router.Dispatch` latency |
| `aether_dispatch_failures_total` | counter | `reason` | satellite post/timeout/parse failures |
| `aether_dedupe_hits_total` | counter | — | inbound duplicates suppressed |
| `aether_queue_full_total` | counter | `queue` | rejected work (`queue = "inbound"` or `"outbound"`) |
| `aether_outbound_jobs_total` | counter | — | outbound jobs accepted (existing) |
| `aether_outbound_queue_depth` | gauge | — | outbound queue occupancy |
| `aether_outbound_send_duration_seconds` | summary | `provider`, `result` | one send attempt |
| `aether_outbound_send_failures_total` | counter | `provider`, `reason` | failed send attempts |
| `aether_outbound_retries_total` | counter | `provider`, `reason` | retries scheduled |
| `aether_dead_letter_total` | counter | `provider` | jobs exhausted and dead-lettered |
| `aether_dropped_total` | counter | — | jobs dropped without delivery (existing) |
| `msg_forward_attempts_total` | counter | `profile`, `result` | forward POST outcomes (`ok`, `retryable`, `terminal`, `queue_full`) |
| `msg_forward_duration_seconds` | summary | `profile`, `result` | forward POST latency |
| `msg_forward_queue_depth` | gauge | `profile` | bounded forward queue occupancy |
| `msg_forward_dropped_total` | counter | `profile`, `reason` | forwards dropped (`queue_full`, `terminal`, `shutdown`) |
| `msg_forward_outbox_pending` | gauge | `profile` | durable forward outbox rows awaiting delivery |
| `msg_forward_outbox_replayed_total` | counter | `profile` | outbox rows re-enqueued after a restart or replay |

Labels are limited to small enums and the configured profile/provider names.

## 4. Risks & Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| `msg` API drift | Integration delays | Narrow `Messenger` facade + adapter tests |
| SQLite locking under load | Message loss or stalls | WAL, `busy_timeout`, single writer, bounded queue |
| Scheduler drift / DST | Missed reminders | UTC storage, injectable clock, timezone tests |
| Unauthorized config changes | Security breach | JWT roles, path-safe registry writes, access checks in config-manager |
| Unbounded outbound retries | Provider bans | Token bucket, capped retries, dead-letter log |
| Slow or hung upstream (signal-cli, satellite, Aether) | Pinned workers, stalled ACKs | Per-hop timeouts and contexts; ACK before dispatch; async forwarding |
| Retry-induced duplicate delivery | Commands run twice | `(sender, message_id)` dedupe (15m TTL); `message_id` in envelope and `route_log` |
| Queue saturation | Blocked handlers, request pile-up | Bounded queues, fail-fast enqueue, `429` + `Retry-After` |
| Silent delivery failure | Lost replies with a `202` already returned | `OnError` wired, dead-letter `route_log` rows, failure metrics |
| Crash loses queued work | Message loss until manual replay | `msg` forwards replay from the outbox; Aether in-memory queues still lose queued work |

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
- `/receive` returns `202` after enqueue; a slow satellite cannot delay it, and a
  full inbound queue returns `429` with `Retry-After`.
- A repeated `/receive` with the same `(sender, message_id)` dispatches once
  within the dedupe TTL.
- A full outbound queue rejects the whole `/message` batch with `429` and
  `Retry-After`, enqueueing nothing.
- Outbound sends retry only network errors, `429`, and `5xx`, honour
  `Retry-After`, and dead-letter after the configured attempts; terminal `4xx`
  is never retried.
- `/metrics` exposes the §3.19 Aether families, and `msg` exposes its forward
  families.
- `go test ./...`, `go vet ./...`, and `gofmt -l .` are clean in CI.
- README documents adding a satellite and configuring triggers.

## 6. Resolved Decisions

| Decision | Resolution |
|---|---|
| `msg` integration | Library `Messenger` facade; CLI UX unchanged except opt-in per-profile forwarding |
| `msg` inbound forwarding | Opt-in per profile via `forwarding: {enabled, url}`; config re-read per message; token only from `AETHER_INGRESS_TOKEN`; the flag/env enable path is removed |
| Wind-predictor config | RFC6902 patch; role check in Aether, schema validation in wind-predictor |
| Prediction piping | `/api/v1/predict/{station}/text` in wind-predictor, trigger `fly` in Aether |
| `/message` auth | JWT (HS256, Bearer) on all satellite/scheduler traffic |
| Approval TTL | Pending requests expire after 7 days |
| Roles | Linear `Denied < User < Admin < Maintainer`; notification routing separate |
| Config root | `AETHER_CONFIG_DIR` + explicit `root` argument; tests use `t.TempDir()` |
| Reminders | `AfterFunc` for relative/absolute, `robfig/cron` for repeating schedules |
| Config format | JSON only (manifest's YAML is superseded) |
| `msg` forward failures | Persist-then-ACK; durable outbox before ACK; bounded per-profile worker; retry network/`429`/`5xx` only; replay pending rows on restart and via `msg replay` |
| Queue full | `429` + `Retry-After`; `/message` batches are all-or-nothing; `429` is the only retryable `4xx` |
| Retry classification | Retry network errors, `429`, and `5xx`; `400`, `401`, `403`, `404`, `409` are terminal |
| Inbound delivery | At-least-once with `(sender, message_id)` dedupe for 15 minutes |
| Outbound delivery | At-least-once within bounded attempts, then dead-letter |
| Async boundaries | `msg` ACKs after its DB write; `/receive` returns `202` after enqueue; dispatch runs on a bounded worker |
| Correlation | `message_id` propagates into the satellite envelope and `route_log` |
| Observability | Hand-rolled Prometheus counters/gauges/summaries in both repos; metric families in §3.19 |
| Durable outbox | `msg` inbound forwards use a SQLite outbox committed before ACK; Aether's own queues remain in memory (A07) |
| Approved groups | Opaque conversation ID, matched literally |
| User `!config set` keys | `timezone`, `prefixes`, `muted`, satellite/command aliases; whitelist is extensible |
| Wind-predictor config | User files only: no-fly days, personal windows, alert timing, per-rule thresholds; no credentials/global keys |
| Service token | `AETHER_TOKEN` env first, 0600 `aether.token` file fallback |
| CI | Independent per-repo CI; Aether CI generates a `go.work` after checking out siblings |
| Rate limits | 1 msg/s per provider, burst 5, outbound and inbound queue depth 256, configurable under `limits` |
| Session actions | `text` optional for `lock`/`extend`/`close` |

## 7. Amendments

Contracts may be amended after the initial build. Each amendment is an
`A`-numbered task in [`amendments.md`](amendments.md), cites the sections it
changes, and updates this plan in the same change.

| ID | Sections | Summary |
|---|---|---|
| A01 | §3.10, §6 | Opt-in per-profile `msg` forwarding; removes the flag/env enable path. |
| A02 | §3.9, §3.10, §6 | Bound every hop: provider/queue contexts, `signal-receiver` server timeouts, retry classification. |
| A03 | §3.2, §3.9, §3.10, §6 | Decouple ACK from dispatch: `msg` persist-then-ACK + forward worker; `/receive` enqueue + dispatch worker; inbound queue-full `429`. |
| A04 | §3.2, §3.6, §3.8, §3.19, §6 | Inbound idempotency by `(sender, message_id)`; propagate `message_id` to envelope and `route_log`. |
| A05 | §3.3, §3.9, §3.19, §6 | Outbound retries, dead-letter, all-or-nothing batch enqueue, queue-full `429`. |
| A06 | §3.19, §6 | Prometheus observability and correlation across both repos. |
| A07 | §3.10, §3.19, §6 | Durable `msg` forward outbox committed before ACK, automatic replay on open, and `msg replay`. |

---
*Prepared based on the architecture manifest and latest user feedback.*
