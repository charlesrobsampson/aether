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
another user.

Maintainers resolve new senders with `!pending` (or `!admin list`),
`!approve <phone|uuid>`, and `!deny <phone|uuid>`. Admins can use
`!admin add|remove <phone|uuid>`.

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
