# Aether Implementation Checklist

**Source of truth:** `plan.md`. If a task here conflicts with the plan, the plan
wins. If the plan is ambiguous, **stop and record the question** in
`docs/open-questions.md`; do not invent a contract.

**Status:** `[ ]` not started · `[?]` awaiting approval · `[~]` in progress ·
`[x]` verified.

---

## Start here (zero-context agent)

You are implementing `plan.md` one task at a time. Do this before anything else:

1. Read `plan.md` end to end. It is the contract; this file is only the order of
   work. Do not skim §3.
2. Read `docs/open-questions.md`.
3. Find the lowest-numbered `[ ]` task. Check that every task in its
   `Depends on` line is `[x]`. If not, stop and report.
4. Create the task's branch, record the exact command that resumes your CLI
   session, then request approval to start (see **Branch per task**,
   **Session & resume**, and **Approval gates**).
5. Work **only** that task. When it is complete, stop and wait for
   `START T(nn+1)`.

Repository: `github.com/charlesrobsampson/aether`, checked out at the repo root
(the directory containing `plan.md`). Default branch is `master`.

Do **not** run with `--auto`. The two approval gates in this file are the point of
the process; auto-approving permissions defeats them.

---

## 0. Non-Negotiable Rules

1. **One task per agent, in numeric order.** Do not start a task until every task
   in its `Depends on` line is `[x]`. Do not begin the next task until the current
   one is `[x]` **and** the human explicitly says `START T(nn+1)`. Never continue
   on your own.
2. **Every task needs two human approvals** — one to start, one to complete.
   See **Approval gates**. Silence is not approval.
3. **One branch per task.** Work on the branch named in the task, never on
   `master`, and merge with `--ff-only` after completion approval. See
   **Branch per task**.
4. **Test first.** Add the named failing test, run it, paste the failure, and
   confirm it fails for the stated reason. Then implement.
5. **Verify before ticking.** A task is done only when the named test prints
   `--- PASS: <TestName>` **and** the full gate is clean.
6. **One task, one concern.** Do not refactor unrelated packages or "fix" tests
   from earlier tasks. Stop and report instead.
7. **Hermetic tests.** No `$HOME`, no network, no `time.Sleep`. Use
   `t.TempDir()`, `httptest`, and `testutil.FakeClock`.
8. **No new dependencies** beyond those named in a task. In particular: no
   testify, no `prometheus/client_golang`, no JSON-Patch library.
9. **Type ownership.** The task that first defines a type owns its name and
   fields. Later tasks must use those exact identifiers; if a field is missing,
   add it in the owning package and update `plan.md` in the same commit. Never
   introduce a second type with the same JSON shape.
10. **Give the human something to run.** Every completion request carries a
   `USER VERIFY` block meeting the bar in **User verification block**. If you
   cannot verify a claim from a shell, say so explicitly.
11. **Tick only after approval.** Change `[ ]` to `[x]` and commit
   `Tnn: <summary> [<cli>:<id>]` only after the completion approval. If the repo
   is not a git repo, T01 initialises it on `master`.
12. **Commit and push deliberately.** Stage explicit paths (never `git add -A`),
   commit only after Gate 2 on the task branch, and push only when a remote
   exists. Never force-push or amend a pushed commit. See
   **Staging, commit, and push**.

### Approval gates

Both gates are mandatory for every task, including T01.

**Gate 1 — before you write code.** Output exactly this block and then stop:

```
REQUEST APPROVAL TO START Tnn — <title>
Branch:   task/tnn-slug (created from master)
Session/Resume: <the exact command you recorded>
Files I will touch: <list>
Failing test I will add: <package>::<TestName>
Plan sections: <§ references>
```

Wait for the human to reply `APPROVED START Tnn`. If they reply with anything
else, treat it as feedback and re-request. Do not create, edit, or delete files
before this approval. The only exceptions are creating the task branch and
filling the task's `**Session:**` / `**Resume:**` lines.

**Gate 2 — after the gate passes.** Output exactly this block and then stop:

```
REQUEST APPROVAL TO COMPLETE Tnn — <title>
Test:     --- PASS: <TestName>        (paste the line)
Full gate: go test ./... / gofmt -l / go vet / go build   (paste the result)

USER VERIFY — copy, paste, and run these yourself:
  1. <command run from the repo root>
     expect: <exact line, exit code, HTTP status, or file>
  2. <command>
     expect: <...>
  3. <command>
     expect: <...>

Artifacts: <files or packages this task added or changed>
Commit:   Tnn: <summary> [<cli>:<id>]   (on the task branch)
Merge:    git switch master && git merge --ff-only task/tnn-slug
Push:     git push -u origin task/tnn-slug && git push origin master
Next:     T(nn+1) — <title>   (do not start it)
```

Wait for `APPROVED COMPLETE Tnn`. Only then tick the box and commit. If the human
asks for changes, make them and request completion again.

### User verification block

The human must be able to confirm the task without trusting the agent. Behind
every `REQUEST APPROVAL TO COMPLETE` there must be a **USER VERIFY** block that
meets this bar:

- **Copy-paste runnable.** Absolute or repo-root-relative paths; if a command must
  run elsewhere (e.g. `../msg`), say so on the command line.
- **Expected output stated.** Never "should pass" — give the exact `--- PASS`
  line, exit code, HTTP status plus body fragment, or file that must exist.
- **Minimum three checks**, covering:
  1. the named test,
  2. an artifact the human can look at (`git show --stat`, `ls`, `cat`,
     `go doc`),
  3. a negative or boundary case the test actually exercises (e.g. a `400`,
     the unchanged file, the counter that did **not** increment).
- **Long-running work starts and stops cleanly.** If verification needs a server,
  give the start command, the check, and the stop command.
- **No agent-only tools.** Do not require a tool the human may not have; assume
  `go`, `git`, `curl`, and a shell.
- **Honest scope.** Say what the block does **not** prove (e.g. "does not exercise
  a real Signal account").

### Branch per task

Every task gets its own branch. The exact name is pre-filled in the task's
`**Branch:**` line — do not invent a different one.

**When you claim a task:**

```bash
branch=task/t02-hermetic-test-harness   # copy from the task's Branch line
git switch master
git pull --ff-only                      # skip when the repo has no remote
git switch -c "$branch"
```

- Never work on `master`, and never reuse a branch from an earlier task.
- If the branch already exists, someone else claimed the task: stop and report.
- T01 is the bootstrap exception: its commit is the root of `master`.
- Resuming on another machine: if the branch is not local but exists on the
  remote, plain `git switch <branch>` will track it. If it exists nowhere, stop
  and report — do not recreate it and risk duplicate work.

**At completion, after `APPROVED COMPLETE Tnn`:** tick, commit on the task
branch, then merge:

```bash
git switch master
git merge --ff-only <branch>
```

- `--ff-only` must succeed. If it fails, `master` moved while you worked — stop
  and report rather than resolving it alone.
- Do not delete the task branch; the human decides when to prune.
- Stay on `master` after merging so the next task branches from the right place.

### Staging, commit, and push

**Stage explicit paths — never `git add -A` or `git add .`.** This repo has
untracked files that do not belong to any task (`chat.sh`, `opencode.sh`, editor
leftovers). `git add -A` has already swept them into a commit once.

```bash
git status --short
git add <every path you changed for this task>   # list them; no wildcards
git diff --cached                                # read every line before committing
```

**Commit once, after `APPROVED COMPLETE Tnn`, on the task branch.** Message
`Tnn: <summary> [<cli>:<id>]`. The tick and the code go in the same commit.

**Push only if a remote exists**, and only after the commit:

```bash
if git remote get-url origin >/dev/null 2>&1; then
  git push -u origin <branch>
  git switch master
  git merge --ff-only <branch>
  git push origin master
else
  echo "no remote: local commit only"
fi
```

- **Never force-push** and never rewrite pushed history (`--amend`, `rebase`,
  `reset` on a pushed branch). If the push is rejected, stop and report.
- **A failed push is not a failed task.** Commit locally, say the push failed and
  why, and continue waiting for the next instruction.
- **Never commit secrets.** Example configs hold placeholders only; real
  `jwt_secret` values, tokens, and `*.db` files stay out of git (see
  `.gitignore`).
- **Working tree after the task:** only expected untracked files remain. If
  `git status` shows something you cannot explain, report it instead of
  committing it.

### Session & resume (works with any CLI)

This checklist is harness-agnostic. You may be running under `opencode`, `agy`,
Claude Code, Codex, or anything else. Do not assume `opencode`.

**At task start, before Gate 1**, determine the exact command that resumes *your*
session and write it into the task:

```bash
# Usage: fill in the task line, then run. Prints the command to paste.
task="T07 — JWT issue/verify"

if [ -n "$OPENCODE_SESSION_ID" ]; then
  printf 'opencode run -s %s "Resume %s. Read plan.md and checklist.md, then continue."\n' \
    "$OPENCODE_SESSION_ID" "$task"
elif [ -n "$AGY_CONVERSATION_ID" ]; then
  printf 'agy --conversation=%s\n' "$AGY_CONVERSATION_ID"
elif [ -d "$HOME/.gemini/antigravity-cli" ]; then
  id=$(grep -F "\"workspace\":\"$PWD\"" "$HOME/.gemini/antigravity-cli/history.jsonl" 2>/dev/null \
       | tail -1 | sed -n 's/.*"conversationId":"\([^"]*\)".*/\1/p')
  printf 'agy --conversation=%s   # verify this is your conversation\n' "${id:-<unknown>}"
else
  echo "Unknown CLI. Find how it resumes (usually '--continue'), and record that."
fi
```

Then edit **only these two lines** in the selected task:

```
**Session:** <cli>:<session-or-conversation-id> (<session title if known>)
**Resume:** <the exact command printed above>
```

Known resume forms (verify with `<cli> --help`; versions change):

| CLI | Resume |
|---|---|
| opencode | `opencode run -s <id> "Resume Tnn ..."` |
| agy | `agy --conversation=<uuid>` (interactive) or `agy --conversation=<uuid> -p "Resume Tnn ..."` |
| Claude Code | `claude --resume <id>` |
| Codex | `codex resume <id>` |

Rules for this section:

- Paste the **expanded** command with the real ID, not `$OPENCODE_SESSION_ID` or
  `<session-id>`; the line must work months later in a fresh shell.
- Get the ID right the first time. If your CLI exposes no ID, record
  `<cli> --continue` plus the working directory, and say so explicitly.
- Do **not** overwrite a `Session:` line that already names another agent. That
  means the task is claimed. Stop and report the conflict; do not work on it.
- Do not change `Session:`/`Resume:` after Gate 1 except to correct a wrong ID.
- The tick commit is `Tnn: <summary> [<cli>:<id>]`.

**To resume**, paste the task's `Resume:` command into a shell in this directory.
It must land on the task branch first, so begin it with `git switch <branch> &&`.
Example (T07, branch `task/t07-jwt`):

```bash
git switch task/t07-jwt && \
opencode run -s ses_e75ab3d3cffe4bV13UJE4hk8VI \
  "Resume T07 (JWT issue/verify). Read plan.md and checklist.md, then continue."
```

For a different CLI, substitute the second line; keep the `git switch` prefix.

---

### One task per agent, and stop there

- **One task per agent session.** Do not open a second task in the same session,
  and do not hand a half-finished task to a second agent.
- **Never start the next task on your own.** After Gate 2, the tick, and the
  commit, stop and print exactly:

  ```
  Tnn complete. Awaiting explicit instruction to start T(nn+1).
  ```

- Begin another task **only** when the human sends a new message containing
  `START T(nn+1)`. "Looks good", "continue", and silence are not instructions to
  start the next task.
- If a `Session:` line on the next task already names a different agent, that task
  is claimed. Report it and wait.

### Full gate

Run after every task, from the repo root:

```bash
go test ./...
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
```

All four must exit 0. `go test -run` exits 0 when **no** test matches, so always
use `-v` and look for `--- PASS: <TestName>` before believing a task is green.

### Repository map

| Repo | Path | Module |
|---|---|---|
| Aether | `.` | `github.com/charlesrobsampson/aether` |
| msg | `../msg` | `github.com/charlesrobsampson/msg` |
| wind-predictor | `../wind-predictor` | `wind-predictor` |

### Task index

| ID | Task | Package | Depends on |
|---|---|---|---|
| T01 | Workspace reset & full scaffold | `internal/router` | — |
| T02 | Hermetic test harness | `internal/testutil` | T01 |
| T03 | Config & shared model types | `internal/config`, `internal/model` | T01 |
| T04 | Config loader | `internal/config` | T03 |
| T05 | Config hot reload | `internal/config` | T04 |
| T06 | Auth roles | `internal/auth` | T04 |
| T07 | JWT issue/verify | `internal/auth` | T06 |
| T08 | SQLite store & migrations | `internal/store` | T01 |
| T09 | Session store | `internal/session` | T02 |
| T10 | Metrics registry | `internal/metrics` | T01 |
| T11 | Messenger interface & fake | `internal/transport` | T01 |
| T12 | Outbound queue | `internal/queue` | T04, T10, T11 |
| T13 | Scheduler | `internal/scheduler` | T02 |
| T14 | `/receive` handler | `internal/api` | T03, T04, T06 |
| T15 | `/message` single recipient | `internal/api` | T04, T07, T12, T14 |
| T16 | Bulk & group sends | `internal/api` | T15 |
| T17 | Mux, `/healthz`, `/metrics` | `internal/api` | T04, T08, T10, T14 |
| T18 | `/register-satellite` | `internal/api` | T04, T07, T08 |
| T19 | SDK `MatchCommand` | `pkg/sdk` | T01 |
| T20 | SDK `ConvertArgs` + `ToUTC` | `pkg/sdk` | T02, T19 |
| T21 | Router dispatch | `internal/router` | T04, T08, T09, T10, T19, T20 |
| T22 | Session routing | `internal/router` | T09, T15, T16, T21 |
| T23 | SDK webhook & response | `pkg/sdk` | T01 |
| T24 | SDK `SessionManager` | `pkg/sdk` | T23 |
| T25 | SDK RFC6902 patch | `pkg/sdk` | T01 |
| T26 | Config-manager satellite | `internal/satellite/configmgr` | T04, T06, T07, T08, T21, T23, T25 |
| T27 | Admin-manager approvals | `internal/satellite/adminmgr` | T04, T06, T08, T12, T25 |
| T28 | Test/reminder satellite | `internal/satellite/testsat` | T12, T13, T20, T23, T24 |
| T29 | `msg` Messenger facade | `../msg/client` | T01 |
| T30 | `msg` inbound forward | `../msg/cmd/msg` | T29 |
| T31 | Aether `msg` adapter | `internal/transport` | T11, T29 |
| T32 | wind-predictor notifier | `../wind-predictor/pkg/alerts` | T23 |
| T33 | wind-predictor config endpoint | `../wind-predictor/pkg/api` | T25, T32 |
| T34 | wind-predictor text prediction | `../wind-predictor/pkg/api` | T32 |
| T35 | App wiring & `cmd/aether` | `internal/app` | T05, T06, T07, T08, T09, T10, T12, T13, T14, T15, T16, T17, T18, T21, T22, T26, T27, T28 |
| T36 | Full-flow integration | `internal/integration` | T35 |
| T37 | Docker & compose | `internal/deploy` | T35 |
| T38 | Docs & CI | `internal/docs` | T36, T37 |

---

## Phase 1 — Foundation

### [x] T01 — Workspace reset & full scaffold
**Depends on:** —
**Branch:** `master` (bootstrap commit; this task is the repo root)
**Write first:** `internal/router/router_test.go::TestModuleInit` — `New()` returns a non-nil `*Router`; `TestScaffoldLayout` — resolves the repo root with `runtime.Caller`, asserts `go.mod` declares `github.com/charlesrobsampson/aether`, the skeleton directories exist, and every `examples/config/**/*.json` parses.
**Implement:**
- **Toss the current module:** delete `go.mod`/`go.sum`, run `go mod init github.com/charlesrobsampson/aether && go mod tidy`. Do not preserve the old indirect deps; each later task adds what it needs.
- `git init -b master` + initial commit if the repo is not a git repo (default branch is `master`).
- Create the skeleton: `cmd/aether`, `internal/{app,config,model,auth,router,session,store,queue,transport,metrics,api,scheduler,testutil,integration,deploy,docs,satellite/{configmgr,adminmgr,testsat}}`, `pkg/sdk`, `examples/config/{users,satellites}`, `scripts`, `.github/workflows` (use `.gitkeep` in empty dirs).
- `cmd/aether/main.go` (`func main() {}` for now); `internal/router/router.go` (`type Router struct{}`, `func New() *Router`).
- `examples/config/`: `system.json` (§3.14 with `schedules`), `admin.json`, `maintainer.json`, `users/+15555550102.json` (including `satellite_ids`), `satellites/reminder.json`, `satellites/wind-predictor.json` (§3.6, trigger `fly`, path `/api/v1/predict/text`). Do not create `routes.json`; T18 generates it.
- `Taskfile.yml` (`build`, `test`, `fmt`, `vet`, `check`), `.gitignore` (`bin/`, `data/`, `*.db*`, `.env`, `go.work*`), and a one-paragraph `README.md` pointing at `plan.md`/`checklist.md`.
**Verify:**
```bash
go test -v ./internal/router -run '^(TestModuleInit|TestScaffoldLayout)$'
go test ./... && test -z "$(gofmt -l .)" && go vet ./...
go build ./...
```
**Done when:** both tests PASS; build clean; `git log --oneline` shows the `T01:` scaffold commit on `master`.
**Session:** `opencode:ses_e75ab3d3cffe4bV13UJE4hk8VI` (checklist.md readability for agents)
**Resume:** `opencode run -s ses_e75ab3d3cffe4bV13UJE4hk8VI "Resume T01 (Workspace reset & full scaffold). Read plan.md and checklist.md, then continue."`

### [x] T02 — Hermetic test harness
**Depends on:** T01
**Branch:** `task/t02-hermetic-test-harness`
**Write first:** `internal/testutil/testutil_test.go::TestTempConfig` — copies `examples/config` into `t.TempDir()`, sets `AETHER_CONFIG_DIR`, and confirms it is restored and `$HOME` is untouched; `TestFakeClock` — `Advance` fires registered timers exactly once and in order.
**Implement:** `internal/testutil` with `TempConfig(t) (root string)` (recursive copy of the example tree), `FakeClock` (`Now`, `Advance`, `AfterFunc`), and `WriteJSON(t, path, v)`.
**Verify:**
```bash
go test -v ./internal/testutil -run '^(TestTempConfig|TestFakeClock)$'
```
**Done when:** both PASS; `grep -R 'os.Getenv("HOME")' internal/testutil` returns nothing.
**Session:** `opencode:ses_e74cdb6d0ffe4oC5vwfKa9Nys8`
**Resume:** `opencode run -s ses_e74cdb6d0ffe4oC5vwfKa9Nys8 "Resume T02 (Hermetic test harness). Read plan.md and checklist.md, then continue."`

### [x] T03 — Config & shared model types
**Depends on:** T01
**Branch:** `task/t03-config-model-types`
**Session:** `agy:cbb49e56-b9ef-466d-8952-935ce8828e37`
**Resume:** `git switch task/t03-config-model-types && agy --conversation=cbb49e56-b9ef-466d-8952-935ce8828e37`
**Write first:** `internal/config/types_test.go::TestStructs` — table of JSON blobs unmarshalling into every type with exact field assertions; `internal/model/model_test.go::TestModel` — `Inbound`/`Outbound` marshal to the §3.2/§3.3 shapes.
**Implement:** `internal/config/types.go` and `internal/model/model.go` with these exact identifiers. Later tasks must not rename or duplicate them (Rule 9).

```go
type Config struct {
    Root       string
    System     SystemConfig
    Admin      AdminConfig
    Maintainer MaintainerConfig
    Users      map[string]UserConfig      // key: phone, e.g. "+15555550102"
    Satellites map[string]SatelliteConfig // key: satellite name
    Routes     RoutesConfig
}
type SystemConfig struct {
    Server          ServerConfig              `json:"server"`
    Persona         PersonaConfig             `json:"persona"`
    Auth            AuthConfig                `json:"auth"`
    Limits          LimitsConfig              `json:"limits"`
    DefaultProvider string                    `json:"default_provider"`
    Providers       map[string]ProviderConfig `json:"providers"`
    Schedules       []ScheduleConfig          `json:"schedules"`
}
type ServerConfig   struct { Port int    `json:"port"`; DBPath string `json:"db_path"` }
type PersonaConfig  struct { Name string `json:"name"`; Prefixes []string `json:"prefixes"` }
type AuthConfig     struct { JWTSecret string `json:"jwt_secret"`; IngressToken string `json:"ingress_token,omitempty"` }
type LimitsConfig   struct { RatePerSecond float64 `json:"rate_per_second"`; Burst int `json:"burst"`; QueueDepth int `json:"queue_depth"` }
type ProviderConfig struct { Type string `json:"type"`; Account string `json:"account,omitempty"`; BaseURL string `json:"base_url,omitempty"`; DBPath string `json:"db_path,omitempty"` }
type ScheduleConfig struct { Name string `json:"name"`; Cron string `json:"cron"`; Satellite string `json:"satellite"`; Command string `json:"command"`; Args map[string]any `json:"args,omitempty"`; Recipients []string `json:"recipients,omitempty"` }
type AdminConfig struct { Admins []string `json:"admins"`; ApprovedNumbers []string `json:"approved_numbers"`; DeniedNumbers []string `json:"denied_numbers"`; ApprovedGroups []string `json:"approved_groups"` }
type MaintainerConfig struct { Maintainers []string `json:"maintainers"`; LastAssigned int `json:"last_assigned"`; NotifyApprovals bool `json:"notify_approvals"` }
type UserConfig struct { Prefixes []string `json:"prefixes"`; Timezone string `json:"timezone"`; Muted bool `json:"muted"`; SatelliteIDs map[string]string `json:"satellite_ids"`; Aliases Aliases `json:"aliases"` }
type Aliases struct { Satellites map[string]string `json:"satellites"`; Commands map[string]string `json:"commands"` }
type SatelliteConfig struct { Name string `json:"name"`; Trigger string `json:"trigger"`; Endpoint string `json:"endpoint"`; Commands []CommandConfig `json:"commands"` }
type CommandConfig struct { Name string `json:"name"`; Path string `json:"path"`; Patterns []string `json:"patterns"`; Args []ArgDef `json:"args,omitempty"` }
type ArgDef struct { Name string `json:"name"`; Type string `json:"type"`; Enum []string `json:"enum,omitempty"` }
type RoutesConfig struct { Routes []RouteEntry `json:"routes"` }
type RouteEntry struct { Trigger string `json:"trigger"`; Satellite string `json:"satellite"`; Endpoint string `json:"endpoint"`; Commands []CommandConfig `json:"commands"` }

// internal/model — JSON tags exactly as plan §3.2 and §3.3
type Inbound struct { Sender string `json:"sender"`; Text string `json:"text"`; Provider string `json:"provider,omitempty"`; GroupID string `json:"group_id,omitempty"`; MessageID string `json:"message_id,omitempty"` }
type Outbound struct { Recipients []string `json:"recipients"`; GroupChatID string `json:"group_chat_id,omitempty"`; Text string `json:"text,omitempty"`; Source string `json:"source,omitempty"`; Satellite string `json:"satellite,omitempty"`; ThreadID string `json:"thread_id,omitempty"`; Session *SessionAction `json:"session,omitempty"` }
type SessionAction struct { Action string `json:"action"`; TTLSeconds int `json:"ttl_seconds,omitempty"` }
```

`ArgDef.Type` is one of `string`, `int`, `enum`, `datetime`. `Config.Root` is set by `LoadConfig`, never serialised.
**Verify:**
```bash
go test -v ./internal/config -run '^TestStructs$'
go test -v ./internal/model -run '^TestModel$'
```
**Done when:** both PASS; no `yaml` tags anywhere.

### [x] T04 — Config loader
**Depends on:** T03
**Branch:** `task/t04-config-loader`
**Write first:** `internal/config/load_test.go::TestLoadConfig` — table: missing `system.json` errors; valid tree loads; malformed JSON errors; `ResolveRoot` honours `AETHER_CONFIG_DIR`.
**Implement:** `LoadConfig(root string) (*Config, error)` reading `system.json`, `admin.json`, `maintainer.json`, `users/*.json`, `satellites/*.json`, `routes.json`; optional files produce zero values; `ResolveRoot()` reads `AETHER_CONFIG_DIR` then defaults to `~/.config/aether`. Never call `LoadConfig("")`.
**Verify:**
```bash
go test -v ./internal/config -run '^TestLoadConfig$'
```
**Done when:** PASS; every test uses `t.TempDir()`.
**Session:** `opencode:ses_e6de59891ffeapIAeJAglm48IE`
**Resume:** `git switch task/t04-config-loader && opencode run -s ses_e6de59891ffeapIAeJAglm48IE "Resume T04 (Config loader). Read plan.md and checklist.md, then continue."`

### [x] T05 — Config hot reload
**Depends on:** T04
**Branch:** `task/t05-config-hot-reload`
**Write first:** `internal/config/watch_test.go::TestWatchReload` — a fake event source triggers exactly one reload and swaps the config snapshot atomically; a malformed edit keeps the previous snapshot.
**Implement:** `go get github.com/fsnotify/fsnotify`; `Watcher` interface (`Events() <-chan struct{}`), `NewFSWatcher(root)` using fsnotify, and `Watch(ctx, load, onReload)`. Debounce bursts.
**Verify:**
```bash
go test -v ./internal/config -run '^TestWatchReload$'
```
**Done when:** PASS; the fsnotify implementation is exercised through the interface, not by sleeping.
**Session:** `opencode:ses_e6dc95334ffeM2eM5I2RH3o3kC` (T05 config hot reload)
**Resume:** `git switch task/t05-config-hot-reload && opencode run -s ses_e6dc95334ffeM2eM5I2RH3o3kC "Resume T05 (Config hot reload). Read plan.md and checklist.md, then continue."`

### [x] T06 — Auth roles
**Depends on:** T04
**Branch:** `task/t06-auth-roles`
**Write first:** `internal/auth/role_test.go::TestCheckAccess` — table covering every transition `Denied/User/Admin/Maintainer`, plus "denied wins over approved".
**Implement:** `internal/auth/role.go`: `type Role int` with `Denied, User, Admin, Maintainer`; `RoleOf(cfg *config.Config, phone string) Role`; `CheckAccess(...) bool` as `RoleOf >= needed`.
**Verify:**
```bash
go test -v ./internal/auth -run '^TestCheckAccess$'
```
**Done when:** PASS; `admin.json` is the only source of approved/denied numbers.
**Session:** `opencode:ses_e6dc20db8ffepO3XzsJspeXpaG` (T06 auth roles)
**Resume:** `git switch task/t06-auth-roles && opencode run -s ses_e6dc20db8ffepO3XzsJspeXpaG "Resume T06 (Auth roles). Read plan.md and checklist.md, then continue."`

### [x] T07 — JWT issue/verify
**Depends on:** T06
**Branch:** `task/t07-jwt`
**Write first:** `internal/auth/jwt_test.go::TestJWT` — valid token, expired, wrong secret, wrong role, missing/malformed `Authorization` header.
**Implement:** `internal/auth/jwt.go`: `Claims{Sub, Role, Exp}`, `Issue(secret, sub, role, ttl)`, `Verify(secret, token)`, and `Middleware(next http.Handler, needed Role)` returning `401`/`403`. HS256 only.
**Verify:**
```bash
go test -v ./internal/auth -run '^TestJWT$'
```
**Done when:** PASS; no `alg:none` path.
**Session:** `opencode:ses_e6dba0bf8ffersHt00DErGuXwJ` (T07 JWT issue/verify)
**Resume:** `git switch task/t07-jwt && opencode run -s ses_e6dba0bf8ffersHt00DErGuXwJ "Resume T07 (JWT issue/verify). Read plan.md and checklist.md, then continue."`

### [x] T08 — SQLite store & migrations
**Depends on:** T01
**Branch:** `task/t08-sqlite-store`
**Write first:** `internal/store/store_test.go::TestMigrations` — `:memory:` creates all four tables with expected columns and is idempotent; `TestWAL` — a file DB reports `journal_mode=wal` and `busy_timeout=5000`.
**Implement:** `go get modernc.org/sqlite`; `internal/store/store.go` using that driver; `Open(dsn)`, `Close()`, `Ping(ctx)`, migrations for `route_log`, `satellites`, `patches`, `approvals` (plan §3.8). Single writer; `foreign_keys=ON`.
**Verify:**
```bash
go test -v ./internal/store -run '^(TestMigrations|TestWAL)$'
```
**Done when:** both PASS; no CGO.
**Session:** `opencode:ses_e6db063d6ffedbQw5QqxVZ7J7w` (T08 SQLite store & migrations)
**Resume:** `git switch task/t08-sqlite-store && opencode run -s ses_e6db063d6ffedbQw5QqxVZ7J7w "Resume T08 (SQLite store & migrations). Read plan.md and checklist.md, then continue."`

### [x] T09 — Session store
**Depends on:** T02
**Branch:** `task/t09-session-store`
**Write first:** `internal/session/session_test.go::TestSessionLifecycle` — start/get/extend/close with `FakeClock`; `TestThreadTargeting` — `#thread-id` selects a thread and default is most recent.
**Implement:** `internal/session`: key `(phone, satellite, thread)`; `Start`, `Get`, `Extend`, `Close`, `MostRecent(phone)`, `List(phone)`; default TTL 5 min; injectable `Clock`; safe for concurrent use.
**Verify:**
```bash
go test -v ./internal/session -run '^(TestSessionLifecycle|TestThreadTargeting)$'
go test -race ./internal/session
```
**Done when:** both PASS under `-race`; no goroutine leaks.
**Session:** `opencode:ses_e6dab3dbfffe2qZAZtpwyOeDg7` (T09 session store)
**Resume:** `git switch task/t09-session-store && opencode run -s ses_e6dab3dbfffe2qZAZtpwyOeDg7 "Resume T09 (Session store). Read plan.md and checklist.md, then continue."`

### [x] T10 — Metrics registry
**Depends on:** T01
**Branch:** `task/t10-metrics`
**Write first:** `internal/metrics/metrics_test.go::TestCounters` — increments are visible and named exactly `aether_bulk_message_requests_total`, `aether_outbound_jobs_total`, `aether_inbound_messages_total`, `aether_dropped_total`; `TestMetricsHandler` — `/metrics` text exposition parses and contains `# TYPE`.
**Implement:** `internal/metrics` with atomic counters and a `Handler() http.Handler` writing Prometheus text format by hand. No external dependency.
**Verify:**
```bash
go test -v ./internal/metrics -run '^(TestCounters|TestMetricsHandler)$'
```
**Done when:** both PASS; no `prometheus/client_golang` in `go.mod`.
**Session:** `opencode:ses_e6da47678ffe2vnf8zshDgUCyL` (T10 metrics registry)
**Resume:** `git switch task/t10-metrics && opencode run -s ses_e6da47678ffe2vnf8zshDgUCyL "Resume T10 (Metrics registry). Read plan.md and checklist.md, then continue."`

---

## Phase 2 — Transport & scheduling

### [x] T11 — Messenger interface & fake
**Depends on:** T01
**Branch:** `task/t11-messenger`
**Write first:** `internal/transport/messenger_test.go::TestFakeMessenger` — records calls and returns an injected error.
**Implement:** `internal/transport/messenger.go`: `Messenger` interface (`Send(ctx, recipient, text) error`, `Name() string`) and `Fake` with `Calls()`/`FailWith(err)`.
**Verify:**
```bash
go test -v ./internal/transport -run '^TestFakeMessenger$'
```
**Done when:** PASS.
**Session:** `opencode:ses_e6d9702a2ffeEXNOK07YyDQEhl` (T11 messenger interface & fake)
**Resume:** `git switch task/t11-messenger && opencode run -s ses_e6d9702a2ffeEXNOK07YyDQEhl "Resume T11 (Messenger interface & fake). Read plan.md and checklist.md, then continue."`

### [x] T12 — Outbound queue
**Depends on:** T04, T10, T11
**Branch:** `task/t12-outbound-queue`
**Write first:** `internal/queue/queue_test.go::TestQueueDrain` — N jobs reach the fake messenger; `TestRateLimit` — a fake clock enforces the token bucket without sleeping; `TestMutedDropped` — muted recipients increment `aether_dropped_total` and are never sent; `TestShutdown` — `Shutdown(ctx)` drains then rejects new jobs.
**Implement:** `internal/queue`: buffered channel (`limits.queue_depth`), `Enqueue`, `Shutdown`, per-provider token bucket, mute predicate callback, metrics wiring. The bucket is hand-rolled (no `x/time/rate`): state is `{tokens float64, last time.Time}` behind a mutex, `allow(now)` refills by `now.Sub(last) * rate` capped at `burst`, and the worker never sleeps on the wall clock — it waits on `clock.AfterFunc`. Exceeding the rate delays, it does not drop. Enqueue after `Shutdown` returns `ErrClosed`.
**Verify:**
```bash
go test -v ./internal/queue -run '^Test(QueueDrain|RateLimit|MutedDropped|Shutdown)$'
go test -race ./internal/queue
```
**Done when:** all PASS under `-race`.
**Session:** `opencode:ses_e6d91e262ffeQoOfNXaIp2rjL5` (T12 outbound queue)
**Resume:** `git switch task/t12-outbound-queue && opencode run -s ses_e6d91e262ffeQoOfNXaIp2rjL5 "Resume T12 (Outbound queue). Read plan.md and checklist.md, then continue."`

### [ ] T13 — Scheduler
**Depends on:** T02
**Branch:** `task/t13-scheduler`
**Write first:** `internal/scheduler/scheduler_test.go::TestScheduler` — `After(30s)` fires exactly once at +30s on `FakeClock`; an absolute instant fires once at that instant; `Cancel` prevents firing; `TestCronNext` — `Next("0 7 * * 1-5", fixedInstant)` returns the expected minute and a 6-field spec is rejected.
**Implement:** `go get github.com/robfig/cron/v3`; `internal/scheduler` uses cron **only** to compute the next activation (`cron.ParseStandard(spec).Next(now)`); firing is driven by the injectable `Clock.AfterFunc`, so tests never wait on a real timer. Expose `After`, `At`, `Every(spec, fn)`, `Cancel`. Reject 6-field specs.
**Verify:**
```bash
go test -v ./internal/scheduler -run '^(TestScheduler|TestCronNext)$'
```
**Done when:** both PASS; no `time.Sleep` and no reliance on robfig's internal goroutine.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

---

## Phase 3 — HTTP API

### [ ] T14 — `/receive` handler
**Depends on:** T03, T04, T06
**Branch:** `task/t14-receive-handler`
**Write first:** `internal/api/receive_test.go::TestReceiveHandler` — table: malformed JSON `400`, missing `sender`/`text` `400`, bad `X-Aether-Token` `401` when configured, valid `202`, denied sender `202` with no dispatch, unknown sender `202` and exactly one call to the injected approval callback.
**Implement:** `internal/api/receive.go` decoding the §3.2 payload into `model.Inbound`; optional `X-Aether-Token` check; denied senders dropped; unknown senders call `OnUnknown(phone)`; call an injected `InboundHandler`; never leak parse errors to the caller.
**Verify:**
```bash
go test -v ./internal/api -run '^TestReceiveHandler$'
```
**Done when:** PASS.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T15 — `/message` single recipient
**Depends on:** T04, T07, T12, T14
**Branch:** `task/t15-message-single`
**Write first:** `internal/api/message_test.go::TestMessageSingle` — table: valid one-recipient payload enqueues one job and returns `202`; malformed `400`; missing/invalid JWT `401`; insufficient role `403`; denied recipient `202` with zero jobs.
**Implement:** `internal/api/message.go` with JWT middleware, decoding into `model.Outbound`, validation of `recipients`, `text`, mutual exclusion with `group_chat_id`, and queue enqueue.
**Verify:**
```bash
go test -v ./internal/api -run '^TestMessageSingle$'
```
**Done when:** PASS; a JWT is minted in the test via `auth.Issue`.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T16 — Bulk & group sends
**Depends on:** T15
**Branch:** `task/t16-message-bulk-group`
**Write first:** `internal/api/message_test.go::TestMessageBulk` — three recipients create three jobs and increment the bulk counter exactly once; `TestMessageGroup` — approved group creates one job, unapproved group creates none, `recipients` + `group_chat_id` returns `400`.
**Implement:** extend `messageHandler` to fan out; single metric increment per request; group lookup against `admin.json`.
**Verify:**
```bash
go test -v ./internal/api -run '^TestMessage(Bulk|Group)$'
```
**Done when:** both PASS.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T17 — Mux, `/healthz`, `/metrics`
**Depends on:** T04, T08, T10, T14
**Branch:** `task/t17-server-health-metrics`
**Write first:** `internal/api/server_test.go::TestHealthMetrics` — `/healthz` is `200` when config is loaded and the store pings, `503` otherwise; `/metrics` serves the registry; no duplicate route registration.
**Implement:** `internal/api/server.go` with `NewServer(deps)` registering every route once; `/healthz` checks `store.Ping` and a config-loaded flag; `/metrics` from `internal/metrics`.
**Verify:**
```bash
go test -v ./internal/api -run '^TestHealthMetrics$'
```
**Done when:** PASS; no second `http.ServeMux` anywhere.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T18 — `/register-satellite`
**Depends on:** T04, T07, T08
**Branch:** `task/t18-register-satellite`
**Write first:** `internal/api/register_test.go::TestRegisterSatellite` — table: success writes `satellites/<name>.json` atomically, mirrors to SQLite, regenerates `routes.json`, returns `201`; duplicate trigger `409`; built-in name/trigger (`config`, `admin`, `remind`, `test`) `409`; `name: "../evil"` `400`; non-absolute endpoint `400`; missing JWT `401`; role `user` `403`.
**Implement:** `internal/api/register.go` validating plan §3.15, writing temp file + rename, upserting the store, and rebuilding `routes.json` from all satellite files.
**Verify:**
```bash
go test -v ./internal/api -run '^TestRegisterSatellite$'
```
**Done when:** PASS; `routes.json` is rebuilt, never appended.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

---

## Phase 4 — Parsing & routing

### [ ] T19 — SDK `MatchCommand`
**Depends on:** T01
**Branch:** `task/t19-sdk-command`
**Write first:** `pkg/sdk/command_test.go::TestMatchCommand` — table: relative reminder, absolute reminder, multi-word `<msg>`, overlapping patterns (declaration order wins), no match, trigger included in pattern.
**Implement:** `pkg/sdk/command.go`: `CommandSpec`, `ArgDef`, `MatchCommand` compiling anchored regexes with lazy named groups and `\s+` between tokens.
**Verify:**
```bash
go test -v ./pkg/sdk -run '^TestMatchCommand$'
```
**Done when:** PASS; regexes are compiled once per spec, not per message.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T20 — SDK `ConvertArgs` + `ToUTC`
**Depends on:** T02, T19
**Branch:** `task/t20-sdk-convert-tz`
**Write first:** `pkg/sdk/convert_test.go::TestConvertArgs` — int, enum rejection, datetime conversion, missing arg; `tz_test.go::TestToUTC` — `America/Chicago` and `Europe/London`, DST boundaries, all five accepted layouts, unknown zone.
**Implement:** `pkg/sdk/convert.go` and `pkg/sdk/tz.go`; `import _ "time/tzdata"`; return `map[string]any`.
**Verify:**
```bash
go test -v ./pkg/sdk -run '^(TestConvertArgs|TestToUTC)$'
```
**Done when:** both PASS; a container without zoneinfo still resolves zones (tzdata import present).
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T21 — Router dispatch
**Depends on:** T04, T08, T09, T10, T19, T20
**Branch:** `task/t21-router-dispatch`
**Write first:** `internal/router/dispatch_test.go::TestDispatch` — table: prefix stripping, satellite alias, command alias, unknown trigger dropped, typed payload posted to `endpoint+path`, route logged.
**Implement:** `internal/router/dispatch.go`: accept `model.Inbound`, resolve user config, strip prefixes, resolve aliases, look up trigger in `routes.json`, `MatchCommand`, `ConvertArgs`, POST JSON, write `route_log`. The outbound POST uses an injectable `*http.Client` with a 5 s timeout; a non-2xx response or transport error is logged and recorded as `status=failed` with the error text — it is never returned to the sender, and no reply is generated.
**Verify:**
```bash
go test -v ./internal/router -run '^TestDispatch$'
```
**Done when:** PASS; the test satellite is `httptest`, not a real service.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T22 — Session routing
**Depends on:** T09, T15, T16, T21
**Branch:** `task/t22-session-routing`
**Write first:** `internal/router/session_test.go::TestSessionRouting` — active session intercepts `2` before trigger parsing; `#jellyfin 2` targets that thread; expired session falls through to trigger parsing; `session.action=close` removes it.
**Implement:** session lookup in the dispatch path, `#thread-id` extraction, and `ApplySessionAction(model.SessionAction)` (called by the `/message` handler) for `lock`/`extend`/`close`.
**Verify:**
```bash
go test -v ./internal/router -run '^TestSessionRouting$'
```
**Done when:** PASS; sessions are never persisted.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

---

## Phase 5 — SDK & satellites

### [ ] T23 — SDK webhook & response
**Depends on:** T01
**Branch:** `task/t23-sdk-webhook`
**Write first:** `pkg/sdk/webhook_test.go::TestSDKWebhook` — table: valid JSON, wrong method, wrong content type, malformed body; `SendResponse` writes JSON and the requested status; `TestSDKClient` — `NewClient` POSTs a `Response` to `/message` with `Authorization: Bearer` and returns an error on non-2xx.
**Implement:** `pkg/sdk/webhook.go`: `Transmission`, `Response` (`Recipients`, `Text`, `Satellite`, `ThreadID`, `Session`), `ParseWebhook`, `SendResponse`; `pkg/sdk/client.go`: `NewClient(baseURL, token)`, `Send(ctx, Response) error`. `sdk.Response` and `internal/model.Outbound` must carry identical JSON tags; `internal/api` converts between them in one helper, and a test asserts the two marshal identically. `pkg/sdk` never imports `internal/`.
**Verify:**
```bash
go test -v ./pkg/sdk -run '^(TestSDKWebhook|TestSDKClient)$'
```
**Done when:** both PASS; `sdk.Response` and `model.Outbound` marshal identically.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T24 — SDK `SessionManager`
**Depends on:** T23
**Branch:** `task/t24-sdk-sessionmanager`
**Write first:** `pkg/sdk/session_test.go::TestSDKSessionManager` — `Lock`, `Extend`, `Close` emit the correct `session.action` and `thread_id` envelope.
**Implement:** `pkg/sdk/session.go` returning `Response` values; no HTTP calls.
**Verify:**
```bash
go test -v ./pkg/sdk -run '^TestSDKSessionManager$'
```
**Done when:** PASS.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T25 — SDK RFC6902 patch
**Depends on:** T01
**Branch:** `task/t25-sdk-patch`
**Write first:** `pkg/sdk/patch_test.go::TestApplyPatch` — table: `add`, `replace`, `remove`, `test` success/failure, invalid pointer, missing path.
**Implement:** `pkg/sdk/patch.go`: `Operation`, `Apply(doc []byte, ops []Operation) ([]byte, error)` with JSON Pointer (`/a/b/0`) support.
**Verify:**
```bash
go test -v ./pkg/sdk -run '^TestApplyPatch$'
```
**Done when:** PASS; no external patch library.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T26 — Config-manager satellite
**Depends on:** T04, T06, T07, T08, T21, T23, T25
**Branch:** `task/t26-configmgr`
**Write first:** `internal/satellite/configmgr/configmgr_test.go::TestApplyPatch` — a patch changes `timezone` on disk and writes a `patches` row; `TestConfigSetCommand` — self edit allowed, other user denied for `User`, allowed for `Admin`, disallowed key rejected, `satellite_ids` is not user-editable; `TestForwardSatelliteConfig` — a `<satellite>.<key>` target resolves the sender via `satellite_ids`, POSTs `{user, patch}` to `/api/v1/config` with a Bearer token, and does not touch the user file; an unmapped satellite returns `400`.
**Implement:** `internal/satellite/configmgr` with `HandlePatch(sdk.Transmission) error`, `!config set <key> <value>`, `!config set @<phone> <key> <value>`, and `<satellite>.<key>` forwarding; user keys are `timezone`, `prefixes`, `muted`, `aliases.satellites.*`, `aliases.commands.*` (extensible); atomic write; audit.
**Verify:**
```bash
go test -v ./internal/satellite/configmgr -run '^(TestApplyPatch|TestConfigSetCommand|TestForwardSatelliteConfig)$'
```
**Done when:** all PASS; tests use `t.TempDir()` and an `httptest` satellite.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T27 — Admin-manager approvals
**Depends on:** T04, T06, T08, T12, T25
**Branch:** `task/t27-adminmgr`
**Write first:** `internal/satellite/adminmgr/adminmgr_test.go::TestApprovalFlow` — new number assigns the expected maintainer, `Approve` moves it to `approved_numbers`, idempotent repeat; `TestApprovalRotation` — round-robin across three maintainers; `TestApprovalExpiry` — a 7-day-old pending row is `expired` and a new message re-requests; `TestAdminMaintenance` — `!admin add/remove <phone>` updates `admins` and is rejected for a `User`.
**Implement:** `internal/satellite/adminmgr` with `RequestApproval`, `Approve`, `Deny`, `Pending`, `AddAdmin`, `RemoveAdmin`; SQLite `approvals`; `maintainer.json` rotation; notify via queue.
**Verify:**
```bash
go test -v ./internal/satellite/adminmgr -run '^Test(ApprovalFlow|ApprovalRotation|ApprovalExpiry|AdminMaintenance)$'
```
**Done when:** all PASS; `last_assigned` only increments after a successful write.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T28 — Test/reminder satellite
**Depends on:** T12, T13, T20, T23, T24
**Branch:** `task/t28-testsat`
**Write first:** `internal/satellite/testsat/testsat_test.go::TestReminderFlow` — `in 30 seconds` fires exactly one `/message` after `FakeClock.Advance(30s)`; absolute reminder fires once; `/test` echoes `test received`.
**Implement:** `internal/satellite/testsat` with `/test` and `/remind`, using `pkg/sdk` and `internal/scheduler`.
**Verify:**
```bash
go test -v ./internal/satellite/testsat -run '^TestReminderFlow$'
```
**Done when:** PASS; no `time.Sleep`.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

---

## Phase 6 — Integrations

> Repo switch: T29–T30 edit `../msg`; T32–T34 edit `../wind-predictor`.
> Both repos get a local `replace` for `github.com/charlesrobsampson/aether` while developing.
>
> **Before a cross-repo task**, run `git -C ../msg status --short` (or
> `../wind-predictor`). If the tree is not clean, stop and report — do not commit
> someone else's work. Record the baseline commit (`git -C ../msg rev-parse
> --short HEAD`) in your start request. Each repo gets its own commit; never mix
> two repos in one commit, and never `git add` from a parent directory.

### [ ] T29 — `msg` Messenger facade
**Depends on:** T01
**Branch:** `task/t29-msg-messenger`
**Write first:** `../msg/client/messenger_test.go::TestMessengerFacade` — `NewMessenger` rejects unknown providers; a signal send hits the fake signal-cli endpoint with the right account/recipient/text; `false, nil` becomes an error; a `t.TempDir()` DB path is used (no `$HOME`).
**Implement:** `../msg/client/messenger.go` with `Messenger`, `MessengerConfig{Provider, Account, BaseURL, DBPath}`, `NewMessenger`; add `OpenSignalDBAt(path)` and `NewSignalProviderWithDB(...)` so `DBPath` is honoured; wrap `SignalProvider`/`OpenMessageProvider`. **Do not change CLI behavior in `cmd/msg`.**
**Verify:**
```bash
(cd ../msg && go test -v ./client -run '^TestMessengerFacade$' && go test ./...)
```
**Done when:** PASS; `git -C ../msg diff -- cmd/msg` is empty.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T30 — `msg` inbound forward
**Depends on:** T29
**Branch:** `task/t30-msg-forward`
**Write first:** `../msg/cmd/msg/forward_test.go::TestForwardInbound` — a normalized inbound message POSTs `{sender,text,provider,group_id,message_id}` with `X-Aether-Token`; on failure it retries a bounded number of times with backoff, then logs and returns without failing the receiver; no spool file is created.
**Implement:** `../msg/cmd/msg/forward.go` with a `Forwarder` and bounded retry/backoff; wire `--forward URL` / `MSG_FORWARD_URL` and `--forward-token` / `AETHER_INGRESS_TOKEN` into `signal-receiver` only.
**Verify:**
```bash
(cd ../msg && go test -v ./cmd/msg -run '^TestForwardInbound$')
```
**Done when:** PASS; forwarding disabled leaves existing behavior byte-identical.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T31 — Aether `msg` adapter
**Depends on:** T11, T29
**Branch:** `task/t31-msg-adapter`
**Write first:** `internal/transport/msg_test.go::TestMsgAdapter` — a fake `msg` sender receives `Send(ctx, recipient, text)` unchanged and errors propagate.
**Implement:** `internal/transport/msg.go` adapting `msg/client.Messenger` to `transport.Messenger`; add the `replace`/`go.work` entry; map `system.json` provider config to `MessengerConfig`; wire it into `cmd/aether` when `providers.<name>.type == "msg"`.
**Verify:**
```bash
go test -v ./internal/transport -run '^TestMsgAdapter$'
```
**Done when:** PASS; no network in the test.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T32 — wind-predictor notifier
**Depends on:** T23
**Branch:** `task/t32-wind-notifier`
**Write first:** `../wind-predictor/pkg/alerts/aether_test.go::TestAetherNotifier` — `AlertPayload` becomes text with title/body; the POST goes to `/message` with a Bearer token and `satellite: "wind-predictor"`; the recipient comes from `UserConfig.aether_recipient`; an empty recipient is a skipped delivery, not an error; non-2xx becomes an error.
**Implement:** `../wind-predictor/pkg/alerts/aether.go` implementing `Notifier` via `sdk.NewClient(...).Send(...)`; add `AetherRecipient string \`json:"aether_recipient,omitempty"\`` to `UserConfig`; add the `aether` replace; read the token from `AETHER_TOKEN`, falling back to a 0600 `aether.token` in the wind-predictor config dir.
**Verify:**
```bash
(cd ../wind-predictor && go test -v ./pkg/alerts -run '^TestAetherNotifier$')
```
**Done when:** PASS; existing `channels_test.go` still passes.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T33 — wind-predictor config endpoint
**Depends on:** T25, T32
**Branch:** `task/t33-wind-config`
**Write first:** `../wind-predictor/pkg/api/config_test.go::TestConfigEndpoint` — table: a patch to `/subscriptions/0/personal_constraints/no_fly_days` and `/alert_rules/0/min_desirability` returns `204` and rewrites only `users/<user>.json`; invalid `ValidateUser` result `400`; `/alert_channels/...` and `/stations/...` `422`; unknown user `404`; wrong method `405`; a failed validation leaves the file untouched.
**Implement:** `../wind-predictor/pkg/api/config.go` handling `POST /api/v1/config` with `{"user":"alice","patch":[...]}`; allow only `/display_name`, `/subscriptions/*/personal_constraints/{window_start_local,window_end_local,no_fly_days}`, `/alert_timing/*`, and `/alert_rules/*/{min_desirability,min_confidence,priority,sound}`; apply via `sdk.ApplyPatch` to a copy, run `config.ValidateUser`, write atomically through an injected `UserStore` rooted at a temp dir. Bind to loopback.
**Verify:**
```bash
(cd ../wind-predictor && go test -v ./pkg/api -run '^TestConfigEndpoint$')
```
**Done when:** PASS; a failed validation leaves the original file untouched.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T34 — wind-predictor text prediction
**Depends on:** T32
**Branch:** `task/t34-wind-text-predict`
**Write first:** `../wind-predictor/pkg/api/predict_text_test.go::TestTextPredict` — `POST /api/v1/predict/text` with the Aether envelope `{"command":"fly","args":{"station":"FPS"},"sender":"+1..."}` returns `{"text": "..."}` with short lines; unknown station `404`; no model `503`; wrong method `405`.
**Implement:** `../wind-predictor/pkg/api/predict_text.go` parsing the envelope with `sdk.ParseWebhook`, reusing the existing predictor, formatting for messaging (no tables, mph/°F); register `POST /api/v1/predict/text`. Add `examples/config/satellites/wind-predictor.json` with `trigger: "fly"`, pattern `fly <station>`, path `/api/v1/predict/text`.
**Verify:**
```bash
(cd ../wind-predictor && go test -v ./pkg/api -run '^TestTextPredict$')
```
**Done when:** PASS; JSON predict routes unchanged.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

---

## Phase 7 — Binary, E2E, deploy

### [ ] T35 — App wiring & `cmd/aether`
**Depends on:** T05, T06, T07, T08, T09, T10, T12, T13, T14, T15, T16, T17, T18, T21, T22, T26, T27, T28
**Branch:** `task/t35-app-wiring`
**Write first:** `internal/app/app_test.go::TestAppWiring` — `Run` on `:0` serves `/healthz`, reloads config, accepts an injected fake `Messenger`, and shuts down cleanly; `TestTokenCommand` — `app.RunToken([]string{"--sub","+1","--role","admin"})` prints a verifiable JWT; `TestBuiltinRoutes` — `!config`, `!admin`, `!remind`, and `!test` reach the internal handlers without any HTTP satellite; `TestSchedulesWired` — a `system.json` schedule fires on `FakeClock` and dispatches to the configured satellite.
**Implement:** `internal/app` with `Run(ctx, Options{ConfigDir, Addr, Messenger, Clock})` and `RunToken(args)`; `cmd/aether/main.go` parsing `serve` (default) and `token`, plus `--config-dir`; register built-in commands before file satellites; load `schedules` into `internal/scheduler`; apply `AETHER_JWT_SECRET` over `auth.jwt_secret`; graceful shutdown drains the queue; unknown senders call `adminmgr.RequestApproval` exactly once per pending phone. The real `msg` adapter is injected in T31; T35 uses a fake.
**Verify:**
```bash
go test -v ./internal/app -run '^(TestAppWiring|TestTokenCommand|TestBuiltinRoutes|TestSchedulesWired)$'
go build ./cmd/aether
```
**Done when:** all PASS; binary builds.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T36 — Full-flow integration
**Depends on:** T35
**Branch:** `task/t36-integration`
**Write first:** `internal/integration/full_flow_test.go::TestFullFlow` — one flow: mint JWT → register a satellite → `/receive` `!watch sci-fi` → mock satellite gets typed payload → satellite responds `/message` with `session.lock` → user sends `2` → satellite closes → fake messenger has both outbound messages → bulk counter unchanged and outbound metric incremented.
**Implement:** the wiring needed to make the flow pass; no production shortcuts that bypass auth or the queue.
**Verify:**
```bash
go test -v ./internal/integration -run '^TestFullFlow$'
go test -race ./...
```
**Done when:** PASS under `-race`; no external services.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T37 — Docker & compose
**Depends on:** T35
**Branch:** `task/t37-deploy`
**Write first:** `internal/deploy/deploy_test.go::TestComposeConfig` — parses `docker-compose.yml`, asserts the `aether` service, healthcheck, non-root user, and the `msg` sidecar; `Dockerfile` is multi-stage and ends on Alpine (not scratch).
**Implement:** `go get gopkg.in/yaml.v3`; `Dockerfile`, `docker-compose.yml`, `scripts/smoke.sh` (starts the binary with a temp config and curls `/healthz`).
**Verify:**
```bash
go test -v ./internal/deploy -run '^TestComposeConfig$'
./scripts/smoke.sh
```
**Done when:** test PASS; smoke script exits 0.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

### [ ] T38 — Docs & CI
**Depends on:** T36, T37
**Branch:** `task/t38-docs-ci`
**Write first:** `internal/docs/docs_test.go::TestDocs` — `README.md` exists with a quick start and links this checklist; `docs/adding-a-satellite.md` and `docs/architecture.md` exist; every relative Markdown link resolves; `.github/workflows/ci.yml` parses with `gopkg.in/yaml.v3` and defines jobs for `aether`, `msg`, and `wind-predictor`; no `go.work` is committed.
**Implement:** `go get gopkg.in/yaml.v3`; `README.md`, `docs/adding-a-satellite.md`, `docs/architecture.md`, `.github/workflows/ci.yml` (each repo's own job; the Aether job checks out `msg`/`wind-predictor` as siblings and generates a CI-only `go.work`), and `scripts/check-docs.sh` as a thin wrapper around the test.
**Verify:**
```bash
go test -v ./internal/docs -run '^TestDocs$'
./scripts/check-docs.sh
```
**Done when:** `--- PASS: TestDocs`; the script exits 0; CI YAML parses.
**Session:** `<fill at task start>`
**Resume:** `<fill at task start: exact command for your CLI>`

---

## Completion

When every box is `[x]`, report:

1. The exact commit range and the test command output summary.
2. Any `docs/open-questions.md` entries that remain unresolved.
3. Confirmation that no task was ticked without a `--- PASS` line.
