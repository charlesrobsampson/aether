# Aether Task Process

This file defines **how** work is executed. It applies to both task queues:

- [`checklist.md`](checklist.md) — the initial build, tasks `T01`…`T38`.
- [`amendments.md`](amendments.md) — post-completion contract changes, tasks
  `A01`…, each of which cites the plan section it amends.

`plan.md` is the source of truth. If a task conflicts with the plan, the plan
wins. If the plan is ambiguous, **stop and record the question** in
`docs/open-questions.md`; do not invent a contract.

**Status:** `[ ]` not started · `[?]` awaiting approval · `[~]` in progress ·
`[x]` verified · `[!]` superseded (amendments only; do not execute).

---

## Start here (zero-context agent)

You are implementing `plan.md` one task at a time. Do this before anything else:

1. Read `plan.md` end to end. It is the contract; the task files are only the
   order of work. Do not skim §3.
2. Read `docs/open-questions.md` and this file.
3. Open the **active task file** and find the lowest-numbered `[ ]` task. Check
   that every task in its `Depends on` line is `[x]`. If not, stop and report.
4. Create the task's branch, record the exact command that resumes your CLI
   session, then request approval to start (see **Branch per task**,
   **Session & resume**, and **Approval gates**).
5. Work **only** that task. When it is complete, stop and wait for
   `START <next ID>`.

### Which task file is active?

- `checklist.md` is the main queue. Work it in numeric order.
- `amendments.md` contains post-completion changes. Do **not** start an amendment
  automatically after a checklist task. Begin one only when the human says
  `START A(nn)`.
- If both files contain unstarted tasks, the human decides which queue runs.
  Unless told otherwise, finish the checklist first.

Repository: `github.com/charlesrobsampson/aether`, checked out at the repo root
(the directory containing `plan.md`). Default branch is `master`.

Do **not** run with `--auto`. The two approval gates in this file are the point of
the process; auto-approving permissions defeats them.

---

## 0. Non-Negotiable Rules

1. **One task per agent, in numeric order.** Do not start a task until every task
   in its `Depends on` line is `[x]`. Do not begin the next task until the current
   one is `[x]` **and** the human explicitly says `START <next ID>`. Never
   continue on your own.
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
    `<ID>: <summary> [<cli>:<id>]` only after the completion approval. If the
    repo is not a git repo, T01 initialises it on `master`.
12. **Commit and push deliberately.** Stage explicit paths (never `git add -A`),
    commit only after Gate 2 on the task branch, and push only when a remote
    exists. Never force-push or amend a pushed commit. See
    **Staging, commit, and push**.
13. **Amendments update the plan.** An amendment task cites the plan section it
    changes and updates `plan.md` in the same change. If a later amendment
    replaces it, mark the old one `[!]` with a `Superseded by:` line; never
    delete the record.

### Approval gates

Both gates are mandatory for every task, including T01.

`<ID>` is the task identifier (`T07`, `A01`, …). `<title>` is the task title from
the task file.

**Gate 1 — before you write code.** Output exactly this block and then stop:

```
REQUEST APPROVAL TO START <ID> — <title>
Branch:   <branch from the task's Branch line> (created from master)
Session/Resume: <the exact command you recorded>
Files I will touch: <list>
Failing test I will add: <package>::<TestName>
Plan sections: <§ references, plus Amends/Why for amendments>
```

Wait for the human to reply `APPROVED START <ID>`. If they reply with anything
else, treat it as feedback and re-request. Do not create, edit, or delete files
before this approval. The only exceptions are creating the task branch and
filling the task's `**Session:**` / `**Resume:**` lines.

**Gate 2 — after the gate passes.** Output exactly this block and then stop:

```
REQUEST APPROVAL TO COMPLETE <ID> — <title>
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
Commit:   <ID>: <summary> [<cli>:<id>]   (on the task branch)
Merge:    git switch master && git merge --ff-only <branch>
Push:     git push -u origin <branch> && git push origin master
Next:     <next ID> — <title>   (do not start it)
```

Wait for `APPROVED COMPLETE <ID>`. Only then tick the box and commit. If the human
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
`**Branch:**` line — do not invent a different one. Checklist branches usually
start `task/`; amendment branches usually start `amendment/`.

**When you claim a task:**

```bash
branch=<copy from the task's Branch line>
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

**At completion, after `APPROVED COMPLETE <ID>`:** tick, commit on the task
branch, then merge:

```bash
git switch master
git merge --ff-only <branch>
```

- `--ff-only` must succeed. If it fails, `master` moved while you worked — stop
  and report rather than resolving it alone.
- Do not delete the task branch; the human decides when to prune.
- Stay on `master` after merging so the next task branches from the right place.

### Cross-repo tasks

Some tasks edit a sibling repository (`../msg`, `../wind-predictor`). Those tasks
declare a `**Repo:**` line. The rules are:

- The code commit lives in the task's repo, on the named branch there.
- The Aether checkout gets a separate **tick commit** on the same branch name;
  it ticks the task and records the sibling commit hash in the commit body or the
  task's completion note. Never mix two repositories in one commit.
- Run the full gate from the code repo's root, e.g.
  `(cd ../msg && go test ./... && test -z "$(gofmt -l .)" && go vet ./... && go build ./...)`.
- Gate 2 must list both commit hashes and both branch pushes.
- Before starting, check `git -C ../msg status --short` (or the sibling path).
  If it is not clean, stop and report; do not commit someone else's work.

### Staging, commit, and push

**Stage explicit paths — never `git add -A` or `git add .`.** This repo has
untracked files that do not belong to any task (`chat.sh`, `opencode.sh`, editor
leftovers). `git add -A` has already swept them into a commit once.

```bash
git status --short
git add <every path you changed for this task>   # list them; no wildcards
git diff --cached                                # read every line before committing
```

**Commit once, after `APPROVED COMPLETE <ID>`, on the task branch.** Message
`<ID>: <summary> [<cli>:<id>]`. For single-repo tasks the tick and the code go in
the same commit. For cross-repo tasks, follow **Cross-repo tasks**.

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

This process is harness-agnostic. You may be running under `opencode`, `agy`,
Claude Code, Codex, or anything else. Do not assume `opencode`.

**At task start, before Gate 1**, determine the exact command that resumes *your*
session and write it into the task:

```bash
# Usage: fill in the task line, then run. Prints the command to paste.
task="T07 — JWT issue/verify"

if [ -n "$OPENCODE_SESSION_ID" ]; then
  printf 'opencode run -s %s "Resume %s. Read plan.md, process.md, and the task file, then continue."\n' \
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
| opencode | `opencode run -s <id> "Resume <ID> ..."` |
| agy | `agy --conversation=<uuid>` (interactive) or `agy --conversation=<uuid> -p "Resume <ID> ..."` |
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
- The tick commit is `<ID>: <summary> [<cli>:<id>]`.

**To resume**, paste the task's `Resume:` command into a shell in this directory.
It must land on the task branch first, so begin it with `git switch <branch> &&`.
Example (T07, branch `task/t07-jwt`):

```bash
git switch task/t07-jwt && \
opencode run -s ses_e75ab3d3cffe4bV13UJE4hk8VI \
  "Resume T07 (JWT issue/verify). Read plan.md, process.md, and checklist.md, then continue."
```

For a different CLI, substitute the second line; keep the `git switch` prefix.

---

### One task per agent, and stop there

- **One task per agent session.** Do not open a second task in the same session,
  and do not hand a half-finished task to a second agent.
- **Never start the next task on your own.** After Gate 2, the tick, and the
  commit, stop and print exactly:

  ```
  <ID> complete. Awaiting explicit instruction to start <next ID>.
  ```

- Begin another task **only** when the human sends a new message containing
  `START <next ID>`. "Looks good", "continue", and silence are not instructions
  to start the next task.
- If a `Session:` line on the next task already names a different agent, that task
  is claimed. Report it and wait.

### Full gate

Run after every task, from the task repo's root:

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

### Task files and IDs

| File | IDs | Purpose |
|---|---|---|
| `checklist.md` | `T01`…`T38` | Initial, dependency-ordered build. |
| `amendments.md` | `A01`… | Post-completion contract changes; each task has `Amends:` and `Why:`. |

- Checklist tasks run in numeric order. Amendment tasks run in numeric order and
  are started explicitly with `START A(nn)`.
- An amendment is a single concern. If it needs two unrelated changes, split it
  into two amendments.
- An amendment must update `plan.md` and, when the plan has an amendment table,
  add its row there in the same change.
- Never rewrite a completed amendment. If a later one replaces it, mark the old
  one `[!]` and add `**Superseded by:** A(nn)`.
