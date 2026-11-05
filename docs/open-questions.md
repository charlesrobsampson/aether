# Open Questions

All questions from the first alignment pass are now resolved. `plan.md` remains
the source of truth; this file records the answers and the few follow-ups that
can be tuned later without changing a contract.

## Resolved

| # | Question | Resolution |
|---|---|---|
| 1 | `msg` inbound forwarding when Aether is down | HTTP POST with bounded retry/backoff, then log; **no spool**. `msg`'s own DB remains the replay source. |
| 2 | Ingress token key | `AETHER_INGRESS_TOKEN` / `X-Aether-Token`; `msg --forward-token` sets it. |
| 3 | User `!config set` keys | `timezone`, `prefixes`, `muted`, `aliases.satellites.*`, `aliases.commands.*`. The whitelist is intentionally extensible; adding a key requires a test and a plan update. `satellite_ids` is admin-only. |
| 4 | Approved group identifier | Opaque conversation ID, matched literally. Signal and openmessage formats both work without normalization. |
| 5 | Service token for wind-predictor | `AETHER_TOKEN` env first, then a 0600 `aether.token` file in the wind-predictor config dir. |
| 6 | Wind-predictor config surface | **User files only**: `/display_name`, `/subscriptions/*/personal_constraints/{window_start_local,window_end_local,no_fly_days}`, `/alert_timing/*`, `/alert_rules/*/{min_desirability,min_confidence,priority,sound}`. Credentials and global/station/training keys are rejected. |
| 7 | `routes.json` after a manual satellite edit | Derived cache; regenerate on `LoadConfig`/watch. Never edited by hand. |
| 8 | Default rate limit / queue depth | 1 msg/s per provider, burst 5, queue depth 256; overridable via `limits` in `system.json`. |
| 9 | Multi-repo CI | Each repo has its own job; the Aether job checks out `msg`/`wind-predictor` as siblings and generates a CI-only `go.work`. No committed `go.work`. |
| 10 | `session.action` without `text` | Allowed for `lock`/`extend`/`close`; `text` is optional when a session action is present. |
| 11 | Session continuation endpoint/path | An intercepted reply is matched against the session satellite's configured command patterns (no trigger) and POSTed to the matched command's `endpoint + path`. A satellite declares a reply pattern such as `<choice>`; no new config field is needed. |
| 12 | `msg` inbound forwarding | Opt-in per profile via `forwarding: {enabled, url}`; `msg` re-reads the active profile's config per inbound message; the token comes only from `AETHER_INGRESS_TOKEN`; the legacy `--forward` / `MSG_FORWARD_URL` enable path is removed (A01). |

## Follow-ups (non-blocking, tunable later)

- Rate-limit defaults may need tuning once real `msg` throughput is measured.
- `satellite_ids` currently maps one wind-predictor user per Aether phone; multiple
  profiles per phone can be added later without a breaking change.
- The config whitelist can grow by adding tests; no schema migration is required.
- CI's sibling checkout uses a PAT; replace with published modules when the three
  repos stabilise.
