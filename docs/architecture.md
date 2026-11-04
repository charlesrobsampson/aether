# Aether architecture

Aether is one Go process with a single `http.ServeMux`, an in-memory session
store, a bounded outbound queue, and a SQLite audit log. Inbound transports
(`msg`, webhooks) post to `/receive`; satellites and the scheduler post outbound
messages to `/message`. Aether never talks to a messenger provider directly: it
calls a `transport.Messenger`, which is backed by the `msg` library facade in
production and by a fake in tests.

The canonical contracts live in [../plan.md](../plan.md). This page is a map of
how the packages fit together.

## Message flow

```text
                 +---------------------+
   inbound ----> | internal/api        |  /receive (optional X-Aether-Token)
                 |  receive handler    |
                 +----------+----------+
                            |
                            v
                 +---------------------+
                 | internal/router     |  prefixes -> aliases -> trigger lookup
                 |  dispatch           |  MatchCommand -> ConvertArgs -> POST
                 +----+-----------+----+
                      |           |
          session hit |           | no session
                      v           v
              session store   routes.json        satellite endpoint
                      |                               |
                      +--------> envelope POST <------+
                                      |
                                      v
                 +---------------------+         +------------------+
                 | internal/api        | <------ | satellite        |
                 |  /message handler   |         | SDK              |
                 +----------+----------+         +------------------+
                            |
                            v
                 +---------------------+
                 | internal/queue      |  mute check, token bucket, workers
                 +----------+----------+
                            |
                            v
                 +---------------------+
                 | transport.Messenger |  msg / fake
                 +---------------------+
```

## Packages

| Package | Responsibility |
|---|---|
| `internal/config` | Loads JSON from `AETHER_CONFIG_DIR`, validates the tree, watches for changes, and regenerates `routes.json`. |
| `internal/model` | Shared `Inbound`, `Outbound`, and `SessionAction` JSON types used by the API, router, and queue. |
| `internal/auth` | Linear roles (`Denied < User < Admin < Maintainer`), HS256 JWT issue/verify, and HTTP middleware. |
| `internal/api` | The single `http.ServeMux`: `/receive`, `/message`, `/register-satellite`, `/healthz`, `/metrics`, plus satellite registration and route regeneration. |
| `internal/router` | Prefix stripping, alias resolution, `routes.json` lookup, command matching and typed argument conversion, session interception, and satellite dispatch. |
| `internal/session` | In-memory `(phone, satellite, thread)` sessions with a TTL and an injectable clock. |
| `internal/store` | CGO-free SQLite (`modernc.org/sqlite`) with WAL, `busy_timeout=5000`, and migrations for `route_log`, `satellites`, `patches`, and `approvals`. |
| `internal/queue` | Bounded outbound queue, per-provider token bucket, mute predicate, metrics, and graceful drain. |
| `internal/transport` | The narrow `Messenger` interface, a recording fake, and the `msg` adapter. |
| `internal/metrics` | Atomic counters and a hand-written Prometheus text exposition. |
| `internal/scheduler` | Relative/absolute timers and cron expressions driven by an injectable clock. |
| `internal/satellite/*` | Built-in satellites: config manager, admin/approval manager, and the test/reminder handler. |
| `internal/app` | Wires configuration, API, router, queue, scheduler, and built-in satellites and owns graceful shutdown. |
| `pkg/sdk` | The public satellite SDK: webhook parsing, outbound client, session helpers, command specs, argument conversion, timezone handling, and RFC6902 patches. |

`pkg/sdk` never imports `internal/`, so satellites such as `wind-predictor` can
depend on the public module alone.

## Endpoints

All endpoints are registered once in `internal/api`. Only `cmd/aether` binds a
port.

| Endpoint | Auth | Purpose |
|---|---|---|
| `POST /receive` | loopback plus optional `X-Aether-Token` | Normalized inbound transport messages. Denied senders are dropped; unknown senders trigger one approval request. |
| `POST /message` | JWT (`admin` or `maintainer`) | Outbound delivery, bulk fan-out, group sends, and session actions. |
| `POST /register-satellite` | JWT (`admin` or `maintainer`) | Validates a satellite document, writes it atomically, mirrors it to SQLite, and rebuilds `routes.json`. |
| `GET /healthz` | none (loopback) | `200` when configuration is loaded and the store answers a ping, otherwise `503`. |
| `GET /metrics` | none (loopback) | Prometheus text exposition. |

## Sessions and threads

A session is keyed by phone, satellite, and optional thread ID and lives only in
memory. When a phone has an active session, the router intercepts the message
before trigger parsing and matches it against the satellite's reply patterns.
`#thread-id` targets a specific thread; otherwise the most recently active
session wins. Satellites control the lifecycle through `session.action` on
`/message`: `lock`, `extend`, or `close`. Sessions are never persisted; SQLite
records audit events only.

## Reliability and security

- The queue is bounded (`limits.queue_depth`), drains on shutdown, and enforces a
  per-provider token bucket. Exceeding the rate delays delivery; it does not drop
  the job. Muted recipients are dropped at enqueue time and counted.
- Denied senders and denied recipients are acknowledged with `202` but produce no
  delivery.
- JWTs are HS256 only; `auth.jwt_secret` can be overridden with
  `AETHER_JWT_SECRET`. Missing or invalid tokens are `401`; insufficient roles
  are `403`.
- Satellite names are validated against `^[a-z0-9][a-z0-9-]*$` and endpoints must
  be absolute `http(s)` URLs, which keeps registry writes inside the config root.
- All persisted timestamps are UTC RFC3339; user-local conversion happens at the
  parser boundary, with `time/tzdata` compiled in so containers without zoneinfo
  still resolve zones.

## Tests and deployment

Tests use `t.TempDir()`, `httptest`, and the fake clock, so they never touch
`$HOME`, a real network, or the wall clock. `internal/integration` exercises the
full flow in-process with an in-memory database.

`docker-compose.yml` runs Aether and a `msg` receiver sidecar for local
development. The `Dockerfile` builds both binaries in a Go builder stage and
copies them onto Alpine, which keeps CA certificates and zoneinfo available. See
[../plan.md](../plan.md) §3.17–§3.18 for the contracts.
