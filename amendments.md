# Aether Amendments

**Source of truth:** [`plan.md`](plan.md). This file records post-completion
contract changes. It does not replace the plan: every amendment cites the plan
section it changes, and the plan is updated in the same change.

**Process:** every task below follows [`process.md`](process.md) exactly like a
checklist task — Gate 1, Gate 2, one branch, one commit, session/resume, the full
gate, and `USER VERIFY`.

**Status:** `[ ]` not started · `[?]` awaiting approval · `[~]` in progress ·
`[x]` verified · `[!]` superseded (do not execute).

## How amendments work

- Amend only after the checklist is complete, or when the human explicitly
  starts an amendment. Never continue from a `T` task into an `A` task.
- One amendment, one concern. Split unrelated changes.
- An amendment task must carry `**Amends:**` (the plan section) and `**Why:**`
  (one short sentence).
- Update `plan.md` and this file's index in the same change.
- Never delete a completed amendment. If a later one replaces it, mark the old
  one `[!]` and add `**Superseded by:** A(nn)`.
- Cross-repo amendments follow **Cross-repo tasks** in `process.md`: the code
  commit lands in the sibling repo, the tick commit lands in Aether, and the tick
  records the sibling commit hash.

## Amendment index

| ID | Amendment | Repo | Depends on |
|---|---|---|---|
| A01 | Per-profile opt-in `msg` forwarding | `../msg` | T30 |
| A02 | Bound every hop (timeouts, contexts, retry classification) | `../msg` + `.` | A01 |
| A03 | Decouple ACK from dispatch (async inbound) | `../msg` + `.` | A02 |
| A04 | Inbound idempotency by `(sender, message_id)` | `.` | A03 |
| A05 | Outbound retries, dead-letter, queue-full `429` | `.` | A04 |
| A06 | Prometheus observability and correlation | `../msg` + `.` | A05 |
| A07 | Durable `msg` outbox + replay (reserved, not scheduled) | `../msg` | A06 |

---

## Phase A1 — `msg` integration

### [x] A01 — Per-profile opt-in `msg` forwarding
**Depends on:** T30
**Amends:** `plan.md` §3.10 (inbound forwarding contract); supersedes the T30 `--forward` / `MSG_FORWARD_URL` enable path.
**Why:** forwarding must be opt-in per `msg` profile and toggleable without restarting the receiver.
**Repo:** `../msg` (code) + `.` (plan and tick)
**Branch:** `amendment/a01-per-profile-forwarding` (in both repos)
**Write first:**
- `../msg/client/config_test.go::TestForwardingConfig` — absent `forwarding` is disabled; `{"enabled":true,"url":"…"}` loads; two profile files produce independent values; malformed JSON returns an error and never enables forwarding; the URL is preserved exactly.
- `../msg/cmd/msg/forward_test.go::TestPerProfileForwarding` — only the profile with `enabled:true` POSTs; switching that profile to `enabled:false` stops the next message; a profile with `enabled:true` and an empty URL does not POST; the `X-Aether-Token` header comes from `AETHER_INGRESS_TOKEN`; a missing config does not forward. Tests use a `t.TempDir()` config root, never the real `$HOME`.
**Implement:** add `ForwardingConfig{Enabled bool `json:"enabled"`; URL string `json:"url,omitempty"`}` to `client.Config` under `json:"forwarding,omitempty"`. Add a pure `LoadConfigForProfile(profile string) (Config, error)` (and a path-based helper for tests) that does not depend on the global `currentProfile` or rewrite config. Add `ForwardingForAccount(account string) (ForwardingConfig, error)` that resolves the account's profile with `FindProfileForAccount`, falls back to the process profile, and reads `config.json` for the default profile. In `signal-receiver`, replace the startup-only `inboundForwarder` with per-message resolution after the DB write: if `enabled` is true and the URL is non-empty, POST the existing normalized envelope with a reusable bounded-timeout HTTP client. Read `AETHER_INGRESS_TOKEN` from the environment only; never store it in config. Remove `--forward`, `--forward-token`, and `MSG_FORWARD_URL` as enable paths. Treat a malformed config or an enabled-but-empty URL as "disabled for this message", log it, and never fail ingestion. Keep the existing `ForwardMessage` wire shape and `/api/signal/webhook` behavior unchanged.
**Verify:**
```bash
(cd ../msg && go test -v ./client -run '^TestForwardingConfig$')
(cd ../msg && go test -v ./cmd/msg -run '^TestPerProfileForwarding$')
(cd ../msg && go test ./...)
```
**Done when:** both named tests print `--- PASS`; forwarding is off by default; only a profile with `forwarding.enabled: true` and a non-empty URL forwards; no `MSG_FORWARD_URL` or `--forward` enable path remains; the token is read only from `AETHER_INGRESS_TOKEN`; `git -C ../msg status --short` shows only intended files; `plan.md` and this file are updated in the Aether tick commit.
**Session:** `opencode:ses_e59baf2a3ffeeLyyjRcU27NbvS` (A01 per-profile msg forwarding)
**Resume:** `git switch amendment/a01-per-profile-forwarding && git -C ../msg switch amendment/a01-per-profile-forwarding && opencode run -s ses_e59baf2a3ffeeLyyjRcU27NbvS "Resume A01 (per-profile msg forwarding). Read plan.md, process.md, and amendments.md, then continue."`
**Completed:** `2026-11-05` · `msg` commit `4f557f1` on `amendment/a01-per-profile-forwarding`.

---

## Phase A2 — Reliability

The amendments below make the existing at-least-once behavior explicit and
observable. They are ordered: each depends on the one before it. Do not start
A(nn) until A(nn-1) is verified and merged.

### [x] A02 — Bound every hop
**Depends on:** A01
**Amends:** `plan.md` §3.9 (outbound send deadline), §3.10 (provider and receiver timeouts), §6 (retry classification).
**Why:** one hung HTTP call must not pin the webhook or Aether's only queue worker forever.
**Repo:** `../msg` (code) + `.` (code and tick)
**Branch:** `amendment/a02-bounded-sends` (in both repos)
**Write first:**
- `../msg/client/messenger_test.go::TestMessengerSendTimeout` — an `httptest` server that sleeps past `MessengerConfig.Timeout`; `Send` returns within that timeout with a deadline error and does not hang.
- `../msg/client/messenger_test.go::TestProviderStatusError` — a `429` returns a typed `*StatusError{StatusCode: 429}` carrying `Retry-After`; `500` is typed; the signal `404` path fallback still works.
- `../msg/cmd/msg/forward_test.go::TestForwardRetryClassification` — `400`/`401`/`403`/`404`/`409` make exactly one attempt; `429` and `5xx` retry; `Retry-After` is honoured; network errors retry; the total stays within the attempt cap.
- `../msg/cmd/msg/signal_receiver_test.go::TestReceiverServerTimeouts` — the receiver server has non-zero `ReadHeaderTimeout`, `ReadTimeout`, and `WriteTimeout`.
- `internal/queue/queue_test.go::TestSendTimeout` — a messenger that blocks until its context is done makes the worker return after `SendTimeout`.
- `internal/queue/queue_test.go::TestShutdownCancelsInflightSend` — a blocked send is cancelled and `Shutdown` returns within its deadline.
- `internal/transport/msg_test.go::TestStatusErrorMapping` — a `*msgclient.StatusError` becomes a `*transport.SendError` with the same status and `Retry-After`.
**Implement:** in `msg/client`, add `Timeout time.Duration` to `MessengerConfig` (0 means 10s) and one shared `*http.Client` per provider. Add `SendMessageContext(ctx, conversationID, text, styled)` to `SignalProvider` and `OpenMessageProvider`; use `http.NewRequestWithContext` + `Do` on the send paths and keep `SendMessage` as a `context.Background()` wrapper for the CLI. Add `StatusError{Provider, StatusCode, RetryAfter, Body}` and return it for non-2xx and `!Success`; `providerMessenger.Send` calls the context method and maps results. In `cmd/msg/forward.go`, centralize the retry policy: retry `net.Error`, deadline errors, `429`, and `5xx`; never retry `400`, `401`, `403`, `404`, `409`; parse `Retry-After` as seconds or an HTTP date; honour the attempt cap. In `signal_receiver.go`, build an `*http.Server` through a `newReceiverServer(addr)` helper with 5s/30s/30s timeouts instead of `http.ListenAndServe`. In Aether `internal/queue`, add `SendTimeout` to `Options`, derive a per-attempt `context.WithTimeout` from the queue context, pass it to `Messenger.Send`, and cancel it on `forceStop`. Add `limits.send_timeout_seconds` (default 10) to `internal/config` and wire it in `internal/app`. In `internal/transport`, add `SendError{StatusCode, RetryAfter, Err}` and map `*msgclient.StatusError` in the adapter; this type is consumed by A05.
**Verify:**
```bash
(cd ../msg && go test -v ./client -run '^(TestMessengerSendTimeout|TestProviderStatusError)$')
(cd ../msg && go test -v ./cmd/msg -run '^(TestForwardRetryClassification|TestReceiverServerTimeouts)$')
go test -v ./internal/queue -run '^(TestSendTimeout|TestShutdownCancelsInflightSend)$'
go test -v ./internal/transport -run '^TestStatusErrorMapping$'
(cd ../msg && go test ./... && go vet ./... && go build ./...)
go test ./... && gofmt -l . && go vet ./... && go build ./...
```
**Done when:** every named test prints `--- PASS`; no send path uses `http.Post`/`http.Get`; the receiver uses an `*http.Server` with all three timeouts; no queue send uses `context.Background()`; a blocked send cannot outlive `send_timeout_seconds`; both full gates are clean (the pre-existing repo-wide `msg` gofmt files remain out of scope); `git -C ../msg status --short` shows only intended files; `plan.md` and this file are updated in the Aether tick, which records the `../msg` commit hash.
**Session:** `opencode:ses_e59baf2a3ffeeLyyjRcU27NbvS` (A02 bounded sends)
**Resume:** `git switch amendment/a02-bounded-sends && git -C ../msg switch amendment/a02-bounded-sends && opencode run -s ses_e59baf2a3ffeeLyyjRcU27NbvS "Resume A02 (bounded sends). Read plan.md, process.md, and amendments.md, then continue."`
**Completed:** `2026-11-05` · `msg` commit `4e40999` on `amendment/a02-bounded-sends`.

---

### [x] A03 — Decouple ACK from dispatch
**Depends on:** A02
**Amends:** `plan.md` §3.2 (enqueue then `202`, `429`), §3.9 (inbound dispatch queue), §3.10 (persist-then-ACK forwarding), §6 (async boundaries).
**Why:** an ACK must mean stored or accepted, not that a satellite has answered.
**Repo:** `../msg` (code) + `.` (code and tick)
**Branch:** `amendment/a03-async-boundaries` (in both repos)
**Write first:**
- `../msg/cmd/msg/forward_test.go::TestForwardDoesNotBlockWebhook` — the forwarding server sleeps 500ms; the webhook returns `200` within 100ms of the DB write, and the forward arrives afterwards.
- `../msg/cmd/msg/forward_test.go::TestForwardQueueFullDoesNotBlock` — with the bounded forward queue saturated, the webhook still returns `200` and records a drop.
- `internal/dispatch/dispatch_test.go::TestDispatchQueueBounded` — with capacity 1, the second `Submit` returns `ErrFull` immediately; the worker drains accepted jobs in order.
- `internal/dispatch/dispatch_test.go::TestDispatchShutdownDrains` — accepted jobs run during `Shutdown`; an expired context drops the remainder without hanging.
- `internal/api/receive_test.go::TestReceiveQueueFull` — `Submit` returns `ErrQueueFull`; the response is `429` with `Retry-After`.
- `internal/integration/full_flow_test.go::TestReceiveReturnsBeforeSlowSatellite` — a satellite that sleeps 500ms does not delay the `/receive` `202` beyond 250ms, and the route log eventually shows the dispatch.
**Implement:** in `../msg`, add a bounded per-profile forward queue (depth 64) with one worker per profile. After `saveSignalPayload` commits, enqueue the forward and return from the webhook handler; a full queue drops the job, logs it, and increments an in-process counter. Preserve per-profile order; run the A02 retry policy inside the worker. In Aether, add `internal/dispatch`: a bounded queue with `Submit(ctx, model.Inbound) error`, `ErrFull`, `ErrClosed`, and `Shutdown(ctx)`; workers call `router.Dispatch`. Change `api.InboundHandler` to `func(context.Context, model.Inbound) error` and add `api.ErrQueueFull`; the app wrapper maps `dispatch.ErrFull` to `api.ErrQueueFull`. `/receive` maps `ErrQueueFull` to `429` + `Retry-After: 1` and any other submit error to `500`, while denied/unknown paths stay `202`. Wire `limits.dispatch_queue_depth` (default 256) in `internal/config`/`internal/app`, and shut the inbound queue down before the router and store. Leave `/message` and `queue.Enqueue` unchanged; A05 makes outbound fail-fast.
**Verify:**
```bash
(cd ../msg && go test -v ./cmd/msg -run '^(TestForwardDoesNotBlockWebhook|TestForwardQueueFullDoesNotBlock)$')
go test -v ./internal/dispatch -run '^(TestDispatchQueueBounded|TestDispatchShutdownDrains)$'
go test -v ./internal/api -run '^TestReceiveQueueFull$'
go test -v ./internal/integration -run '^TestReceiveReturnsBeforeSlowSatellite$'
(cd ../msg && go test ./...)
go test ./... && gofmt -l . && go vet ./... && go build ./...
```
**Done when:** the signal-cli webhook ACK does not wait on Aether; `/receive` returns `202` before a slow satellite finishes; a full inbound queue returns `429` with `Retry-After`; no inbound path blocks on a full queue; both gates are clean; `git -C ../msg status --short` shows only intended files; the tick records the `../msg` commit hash.
**Session:** `opencode:ses_e59baf2a3ffeeLyyjRcU27NbvS` (A03 async boundaries)
**Resume:** `git switch amendment/a03-async-boundaries && git -C ../msg switch amendment/a03-async-boundaries && opencode run -s ses_e59baf2a3ffeeLyyjRcU27NbvS "Resume A03 (async boundaries). Read plan.md, process.md, and amendments.md, then continue."`
**Completed:** `2026-11-05` · `msg` commit `015728e` on `amendment/a03-async-boundaries`.

---

### [x] A04 — Inbound idempotency by `(sender, message_id)`
**Depends on:** A03
**Amends:** `plan.md` §3.2 (dedupe), §3.6 (`message_id` in the envelope), §3.8 (`message_id` column and `inbound_dedupe`), §3.19 (delivery semantics), §6.
**Why:** at-least-once retries must not run a user command twice.
**Repo:** `.`
**Branch:** `amendment/a04-inbound-idempotency`
**Write first:**
- `internal/store/store_test.go::TestClaimInbound` — the first claim is fresh, the second is a duplicate, an empty `message_id` is always fresh, a row older than the TTL can be reclaimed, and concurrent claims produce exactly one fresh result.
- `internal/api/receive_test.go::TestReceiveDedupeHitReturnsAccepted` — a duplicate claim is acknowledged with `202` and `Submit` is not called.
- `internal/api/receive_test.go::TestReceiveDedupeErrorFailsOpen` — a claim error is logged and the message is still submitted.
- `internal/integration/full_flow_test.go::TestReceiveDeduplicatesByMessageID` — the same payload twice produces one satellite POST and one inbound `route_log` row; different IDs produce two; an absent ID produces two.
- `internal/router/dispatch_test.go::TestEnvelopeAndRouteLogCarryMessageID` — the satellite envelope contains `message_id` and `route_log` stores it.
**Implement:** in `internal/store`, add `MessageID` to `RouteLog` and an idempotent migration for `route_log.message_id TEXT NOT NULL DEFAULT ''`; `LogRoute` writes it. Add `inbound_dedupe(sender TEXT, message_id TEXT, seen_at TEXT, PRIMARY KEY(sender, message_id))` and `ClaimInbound(ctx, sender, messageID string, ttl time.Duration) (bool, error)`: insert the key and return true when the row was created; empty `messageID` returns true without writing; delete expired rows opportunistically. In `internal/api`, add `ReceiveOptions.Claim`; call it after auth/decode and before `Submit`; a duplicate returns `202`, a claim error fails open with a log line. In `internal/app`, wire `Claim` with `limits.dedupe_ttl_seconds` (default 900). In `internal/router`, add `MessageID` to `transmission`, populate it from `in.MessageID`, and include it in every `LogRoute` call. Add `DedupeTTLSeconds` to `internal/config`.
**Verify:**
```bash
go test -v ./internal/store -run '^TestClaimInbound$'
go test -v ./internal/api -run '^(TestReceiveDedupeHitReturnsAccepted|TestReceiveDedupeErrorFailsOpen)$'
go test -v ./internal/router -run '^TestEnvelopeAndRouteLogCarryMessageID$'
go test -v ./internal/integration -run '^TestReceiveDeduplicatesByMessageID$'
go test ./... && gofmt -l . && go vet ./... && go build ./...
```
**Done when:** a duplicate within the TTL is suppressed and survives a store restart because the key is in SQLite; `message_id` appears in the envelope and `route_log`; an absent `message_id` is explicitly best-effort; migrations keep an existing database working; the full gate is clean.
**Session:** `opencode:ses_e59baf2a3ffeeLyyjRcU27NbvS` (A04 inbound idempotency)
**Resume:** `git switch amendment/a04-inbound-idempotency && opencode run -s ses_e59baf2a3ffeeLyyjRcU27NbvS "Resume A04 (inbound idempotency). Read plan.md, process.md, and amendments.md, then continue."`
**Completed:** `2026-11-06` · `amendment/a04-inbound-idempotency`

---

### [x] A05 — Outbound retries, dead-letter, and queue-full `429`
**Depends on:** A04
**Amends:** `plan.md` §3.3 (all-or-nothing enqueue and `429`), §3.9 (retries, dead-letter, fail-fast enqueue), §3.19 (outbound delivery), §6 (outbound delivery).
**Why:** a `202` must not silently lose a message, and saturation must push back instead of blocking a handler.
**Repo:** `.`
**Branch:** `amendment/a05-outbound-reliability`
**Write first:**
- `internal/queue/queue_test.go::TestEnqueueFailFast` — a full queue makes `Enqueue` return `ErrFull` immediately instead of blocking.
- `internal/queue/queue_test.go::TestEnqueueBatchAtomic` — a batch larger than the free capacity enqueues nothing; a fitting batch enqueues all; muted jobs do not consume capacity.
- `internal/queue/queue_test.go::TestSendRetriesTransient` — a messenger that fails twice with a retryable error then succeeds produces exactly three attempts, one delivery, and advances the fake clock for backoff.
- `internal/queue/queue_test.go::TestSendStopsOnTerminalError` — a terminal `*transport.SendError{StatusCode: 400}` is attempted once and then dead-lettered.
- `internal/queue/queue_test.go::TestSendHonoursRetryAfter` — a `429` with `Retry-After: 2s` waits at least two seconds on the fake clock.
- `internal/queue/queue_test.go::TestDeadLetterAfterAttempts` — an always-retryable failure with `Attempts: 2` calls `OnDeadLetter` once and stops.
- `internal/api/message_test.go::TestMessageQueueFullReturns429` — `EnqueueBatch` returns `queue.ErrFull`; the response is `429` with `Retry-After` and no jobs were enqueued.
- `internal/integration/full_flow_test.go::TestMessageBatchAtomicOnFullQueue` — a batch larger than the queue leaves the accepted-job count unchanged.
**Implement:** in `internal/queue`, add `ErrFull`, make `Enqueue` non-blocking, and add `EnqueueBatch(jobs []Job) error`. Serialize enqueue with shutdown under one mutex; check free capacity before a batch, then send every job while holding it (sends are non-blocking, so the critical section is bounded); a muted job is counted and dropped without consuming capacity. Add `Options.Attempts` (default 3), `Options.RetryBackoff` (default 500ms, capped at 30s), and `Options.OnDeadLetter func(Job, error)`. In `deliver`, wait for a token, send with the A02 context, call `OnError` on each failure, retry only when `transport.Retryable` says so and attempts remain, wait on `clock.AfterFunc` for the larger of the backoff and `Retry-After`, and call `OnDeadLetter` on exhaustion. In `internal/transport`, use the `SendError` type added in A02 and add `Retryable(err) (bool, time.Duration)`: `net.Error`/context errors are retryable, `429`/`5xx` are retryable, other `SendError` values are terminal, and unknown errors are terminal. In `internal/api`, extend `Enqueuer` with `EnqueueBatch`, build every job for a `/message` request, call it once, map `queue.ErrFull` to `429` + `Retry-After: 1`, and map other errors to `500`. In `internal/app`, wire `OnError` to a log line and `OnDeadLetter` to one `route_log` row with `direction="outbound"`, `status="dead_letter"`, and the last error; wire `limits.send_attempts` and `limits.retry_backoff_ms` in `internal/config`.
**Verify:**
```bash
go test -v ./internal/queue -run '^(TestEnqueueFailFast|TestEnqueueBatchAtomic|TestSendRetriesTransient|TestSendStopsOnTerminalError|TestSendHonoursRetryAfter|TestDeadLetterAfterAttempts)$'
go test -v ./internal/api -run '^TestMessageQueueFullReturns429$'
go test -v ./internal/integration -run '^TestMessageBatchAtomicOnFullQueue$'
go test ./... && gofmt -l . && go vet ./... && go build ./...
```
**Done when:** a full outbound queue returns `429` with `Retry-After` and enqueues no partial batch; only network errors, `429`, and `5xx` retry; `Retry-After` is honoured; exhausted jobs produce one dead-letter `route_log` row and stop; the full gate is clean.
**Session:** `opencode:ses_e59baf2a3ffeeLyyjRcU27NbvS` (A05 outbound reliability)
**Resume:** `git switch amendment/a05-outbound-reliability && opencode run -s ses_e59baf2a3ffeeLyyjRcU27NbvS "Resume A05 (outbound retries and dead-letter). Read plan.md, process.md, and amendments.md, then continue."`
**Completed:** `2026-11-06` · `amendment/a05-outbound-reliability`

---

### [x] A06 — Prometheus observability and correlation
**Depends on:** A05
**Amends:** `plan.md` §3.19 (metric families and correlation), §6 (observability).
**Why:** reliability behavior is invisible without counters, latency, and one correlation ID.
**Repo:** `../msg` (code) + `.` (code and tick)
**Branch:** `amendment/a06-observability` (in both repos)
**Write first:**
- `internal/metrics/metrics_test.go::TestReliabilityExposition` — the registry renders every Aether family in §3.19 with `# TYPE`, the documented labels, and `_sum`/`_count` for summaries; labels are enums, never phone numbers or message IDs.
- `internal/api/receive_test.go::TestReceiveMetrics` — `accepted`, `duplicate`, `queue_full`, `bad_request`, and `unauthorized` increment `aether_receive_requests_total`.
- `internal/queue/queue_test.go::TestQueueMetrics` — the depth gauge, send failures, retries, dead-letter, and `aether_queue_full_total{queue="outbound"}` move together with the queue.
- `internal/router/dispatch_test.go::TestDispatchMetrics` — the dispatch duration summary and failure counter record a satellite timeout.
- `../msg/cmd/msg/forward_test.go::TestForwardMetrics` — the `profile`-labeled attempts, duration, depth, and dropped families move with the forward worker; a terminal `401` increments `result="terminal"` and `reason="terminal"`.
- `../msg/cmd/msg/metrics_test.go::TestMetricsEndpoint` — `GET /metrics` on the loopback listener returns the text exposition.
**Implement:** extend `internal/metrics` with labeled counters, gauges, and summaries (sum/count only), keeping the existing names and `Default`. Add every family in §3.19 and wire it at the receive handler, inbound dispatch queue/worker, router, outbound queue, retry loop, and dead-letter path. Update queue depth on enqueue/dequeue. In `../msg`, add `cmd/msg/metrics.go` with a small registry and text exposition; start a separate loopback metrics listener from `signal-receiver` (`--metrics-addr`, default `127.0.0.1:9091`, empty disables), and record the §3.19 `msg` families around the forward worker. Ensure forward, receive, dispatch, retry, and dead-letter logs include `message_id` when present plus the profile/satellite name, and never log message bodies at info level. If an implementation name must differ from §3.19, update `plan.md` in the same change rather than diverging silently.
**Verify:**
```bash
go test -v ./internal/metrics -run '^TestReliabilityExposition$'
go test -v ./internal/api -run '^TestReceiveMetrics$'
go test -v ./internal/queue -run '^TestQueueMetrics$'
go test -v ./internal/router -run '^TestDispatchMetrics$'
(cd ../msg && go test -v ./cmd/msg -run '^(TestForwardMetrics|TestMetricsEndpoint)$')
(cd ../msg && go test ./... && go vet ./... && go build ./...)
go test ./... && gofmt -l . && go vet ./... && go build ./...
```
**Done when:** every §3.19 family is exposed; label cardinality is bounded; `msg` serves metrics only on its loopback listener; logs carry the correlation ID; both gates are clean; `git -C ../msg status --short` shows only intended files; the tick records the `../msg` commit hash.
**Session:** `opencode:ses_e59baf2a3ffeeLyyjRcU27NbvS` (A06 observability)
**Resume:** `git switch amendment/a06-observability && git -C ../msg switch amendment/a06-observability && opencode run -s ses_e59baf2a3ffeeLyyjRcU27NbvS "Resume A06 (observability). Read plan.md, process.md, and amendments.md, then continue."`
**Completed:** `2026-11-07` · `msg` commit `44a51a4` on `amendment/a06-observability`.

---

### [ ] A07 — Durable `msg` outbox and replay (reserved, do not start)
**Depends on:** A06
**Amends:** would add a durable `msg` outbox and replay command to `plan.md` §3.10/§3.19.
**Why:** reserved for the case where losing queued forwards across a restart becomes unacceptable.
No branch, failing tests, or session are defined until the human schedules it. Until then, `msg`'s SQLite row remains the manual replay source and restart-loss is an accepted risk.
