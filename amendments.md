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

---

## Phase A1 — `msg` integration

### [ ] A01 — Per-profile opt-in `msg` forwarding
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
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

---

## Phase A2 — (reserved)

Add future amendments below A01 in numeric order. Do not renumber existing tasks.
