# Aether

Aether is a central messaging router that sits between a messenger (`msg`) and
satellite services such as `wind-predictor`. It receives inbound messages,
resolves command triggers to satellites, manages short-lived multi-step sessions,
and delivers outbound replies through a rate-limited queue.

`plan.md` is the source of truth for contracts and behaviour. `checklist.md` is
the dependency-ordered implementation plan; work through it one task at a time.

## Quick start

Aether needs Go 1.27+ and a sibling `msg` checkout, because `go.mod` replaces
`github.com/charlesrobsampson/msg` with `../msg`.

```bash
# From the repository root, with ../msg present.
go build ./...
go run ./cmd/aether serve --config-dir ./examples/config
```

The server listens on `127.0.0.1:8080` by default. Probe it from another shell:

```bash
curl -fsS http://127.0.0.1:8080/healthz   # -> ok
curl -fsS http://127.0.0.1:8080/metrics
```

Mint a JWT for the API endpoints:

```bash
go run ./cmd/aether token --sub +15555550102 --role admin
```

Send a test message to a satellite with that token:

```bash
curl -fsS -X POST http://127.0.0.1:8080/message \
  -H "Authorization: Bearer $AETHER_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"recipients":["+15555550102"],"text":"hello","satellite":"test"}'
```

## Configuration

All configuration is JSON under `AETHER_CONFIG_DIR` (default
`~/.config/aether`). `examples/config` is a complete, runnable example. See
[plan.md §3.14](plan.md) for the file layout and every key.

- `system.json` holds server, auth, providers, limits, and schedules.
- `admin.json` is the access-control file (admins, approved/denied numbers and
  groups).
- `users/<phone>.json` holds per-user prefixes, timezone, mute flag, aliases, and
  satellite IDs.
- `satellites/<name>.json` declares a satellite's trigger, endpoint, and command
  patterns; `routes.json` is regenerated from these files and is never edited by
  hand.

Set `AETHER_JWT_SECRET` to override `auth.jwt_secret`, and `AETHER_INGRESS_TOKEN`
to require `X-Aether-Token` on `/receive`.

Built-in chat commands for per-user settings: `!config list` shows effective
values with defaults in parentheses, `!config get <key>` shows one setting,
`!config export` prints pasteable `!config set` lines, and
`!config set <key> <value>` changes a value. Admins can add `@<phone>` to target
another user. To use commands with no prefix, set `prefixes` to an empty list:

```
!config set prefixes []
```

The same works with `!config set prefixes ""`. This overrides the system
persona prefixes for that user; commands are then typed bare, for example
`config list`.

Command names, subcommands, satellite triggers, aliases, and prefixes are
case-insensitive, so a phone that autocapitalizes the first word still works
(`!Approve 2`, `Config List`, `Fly FPS`). Arguments and message text keep their
original case.

Maintainers resolve new senders and groups with `!pending` (or `!admin list`),
`!approve [id[,id...]|phone|uuid|group]`, and
`!deny [id[,id...]|phone|uuid|group]`. `!pending` numbers each request, so
`!approve 1,3,4` resolves a batch, and a bare `!approve` resolves the newest
pending request. Notifications include the short row ID and the sender's name
and phone number when Signal exposes them. Approving a group writes
`approved_groups`; denying one writes `denied_groups`. Admins can use
`!admin add|remove <phone|uuid>`.

Group messages answer in the group by default. A satellite can still address
an individual explicitly by using that recipient in its response. In a group
the bot only responds when it is mentioned with `@` (Signal's native mention
or a literal `@<persona name>`); other group messages are ignored. An approved
sender's first message in an unapproved group creates a group approval request
instead of dispatching.

Group chats are capped at User privileges: admin and maintainer actions
(`!admin`, `!approve`, `!deny`, `!pending`, editing another user's config) must
originate from a private chat. Group names are resolved from the Signal groups
API when the transport does not include one, so approvals and logs show the
group's display name rather than its opaque ID.

## Documentation

- [Architecture](docs/architecture.md) — component map, message flow, and
  package responsibilities.
- [Adding a satellite](docs/adding-a-satellite.md) — registration, command
  patterns, typed arguments, and the SDK contract.
- [Plan](plan.md) — canonical contracts and acceptance criteria.
- [Checklist](checklist.md) — dependency-ordered implementation tasks.
- [Process](process.md) — approval gates, branches, session/resume, and the full gate.
- [Amendments](amendments.md) — post-completion contract changes.
- [Open questions](docs/open-questions.md) — resolved decisions and follow-ups.

## Development

```bash
go test ./...                      # unit and integration tests
test -z "$(gofmt -l .)"            # formatting
go vet ./...                       # static checks
go build ./...                     # every package
./scripts/check-docs.sh            # documentation and CI contract
./scripts/smoke.sh                 # build, start, probe /healthz, stop
```

Tests are hermetic: they use `t.TempDir()`, `httptest`, and the fake clock from
`internal/testutil` rather than `$HOME`, real networks, or sleeps.

## Deployment

`docker-compose.yml` runs Aether with a `msg` receiver sidecar for local
development; `Dockerfile` is a multi-stage build ending on Alpine so CA
certificates and zoneinfo are present. Both are described in
[plan.md §3.18](plan.md).

```bash
docker compose up --build
```
