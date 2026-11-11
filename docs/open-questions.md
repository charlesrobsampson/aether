# Open Questions

All questions from the first alignment pass are now resolved. `plan.md` remains
the source of truth; this file records the answers and the few follow-ups that
can be tuned later without changing a contract.

## Resolved

| # | Question | Resolution |
|---|---|---|
| 1 | `msg` inbound forwarding when Aether is down | Persist-then-ACK, then a bounded per-profile forward worker retries network/`429`/`5xx` with backoff; failures are logged and dropped after the budget. **No spool**. `msg`'s own DB remains the manual replay source. |
| 2 | Ingress token key | `AETHER_INGRESS_TOKEN` / `X-Aether-Token` only; the `--forward-token` flag and `MSG_FORWARD_URL` enable path were removed in A01. |
| 3 | User `!config set` keys | `timezone`, `prefixes`, `muted`, `aliases.satellites.*`, `aliases.commands.*`. The whitelist is intentionally extensible; adding a key requires a test and a plan update. `satellite_ids` is admin-only. An explicitly empty `prefixes` list (`[]` or `""`) means no command prefix and overrides the system persona defaults. |
| 4 | Approved group identifier | Opaque conversation ID, matched literally. Signal and openmessage formats both work without normalization. |
| 5 | Service token for wind-predictor | `AETHER_TOKEN` env first, then a 0600 `aether.token` file in the wind-predictor config dir. |
| 6 | Wind-predictor config surface | **User files only**: `/display_name`, `/subscriptions/*/personal_constraints/{window_start_local,window_end_local,no_fly_days}`, `/alert_timing/*`, `/alert_rules/*/{min_desirability,min_confidence,priority,sound}`. Credentials and global/station/training keys are rejected. |
| 7 | `routes.json` after a manual satellite edit | Derived cache; regenerate on `LoadConfig`/watch. Never edited by hand. |
| 8 | Default rate limit / queue depth | 1 msg/s per provider, burst 5, outbound queue depth 256, inbound dispatch queue depth 256; 3 send attempts with a 10s per-attempt timeout and 500ms base backoff; 15-minute dedupe TTL. All overridable via `limits` in `system.json`. |
| 9 | Multi-repo CI | Each repo has its own job; the Aether job checks out `msg`/`wind-predictor` as siblings and generates a CI-only `go.work`. No committed `go.work`. |
| 10 | `session.action` without `text` | Allowed for `lock`/`extend`/`close`; `text` is optional when a session action is present. |
| 11 | Session continuation endpoint/path | An intercepted reply is matched against the session satellite's configured command patterns (no trigger) and POSTed to the matched command's `endpoint + path`. A satellite declares a reply pattern such as `<choice>`; no new config field is needed. |
| 12 | `msg` inbound forwarding | Opt-in per profile via `forwarding: {enabled, url}`; `msg` re-reads the active profile's config per inbound message; the token comes only from `AETHER_INGRESS_TOKEN`; the legacy `--forward` / `MSG_FORWARD_URL` enable path is removed (A01). |
| 13 | Queue full | `429` with `Retry-After`; `/message` batches enqueue all-or-nothing; `429` is the only retryable `4xx`. |
| 14 | Retryable failures | Network/transport errors, `429`, and `5xx`; `400`, `401`, `403`, `404`, `409` are terminal. Honor `Retry-After` when present. |
| 15 | Where does the ACK happen | `msg` ACKs signal-cli after its SQLite write; `/receive` returns `202` after enqueueing; `router.Dispatch` runs on a bounded worker. |
| 16 | Inbound idempotency | Dedupe `(sender, message_id)` for 15 minutes; a duplicate returns `202` and is not dispatched; an absent `message_id` skips dedupe. |
| 17 | Delivery guarantees | Inbound at-least-once with dedupe; outbound at-least-once within bounded attempts, then dead-letter. Durable outbox deferred (A07). |
| 18 | Observability | Hand-rolled Prometheus counters/gauges/summaries in both repos; families in plan §3.19; `message_id` correlation in logs and `route_log`. |
| 19 | `message_id` propagation | Copied into the satellite envelope and `route_log`; never used as a metric label. |

## Follow-ups (non-blocking, tunable later)

- Rate-limit defaults may need tuning once real `msg` throughput is measured.
- `satellite_ids` currently maps one wind-predictor user per Aether phone; multiple
  profiles per phone can be added later without a breaking change.
- The config whitelist can grow by adding tests; no schema migration is required.
- CI's sibling checkout uses a PAT; replace with published modules when the three
  repos stabilise.
- Timeout, attempt, backoff, and dedupe defaults are starting points; tune them
  after real `msg` throughput and provider latency are measured.
- A07 (durable `msg` outbox + replay) stays reserved; schedule it only if
  restart-loss becomes unacceptable.
