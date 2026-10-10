# Implementation plan

**Status:** approved. Builds the [system design](design.md) and its [system specs](design.md#left-to-component-specs).

## Context

The minimal redesign is approved: the [system design](design.md) plus seven system specs in `docs/systems/`. The current code (about 40k LOC across `store`, `control`, `cli`, `runner`, wake and bridge packages, plus a Pi extension) implements the old design and has no place in the new one. The decision is to delete it and build the new core from empty, lifting a few self-contained mechanisms from git history. This plan orders that build so a working local vertical slice exists early and every later system lands on it.

## Approach

- **Specs are the contract.** Each milestone implements named spec sections, including that system's Extensibility section, and its tests cite them.
- **Parity by construction from day one.** Every public command goes through one `Dispatch(caller, Request) Response` path, with `Request{v, argv, key, run_token, call_token, stdin}`. The local CLI builds a `Request` and dispatches in-process. Later, SSH `serve` decodes the same `Request` and calls the same function.
- **Plugins are wired as systems are built.** The plugin mechanism lands before coordination, so each system adds its own events, intercept points and providers in its own milestone instead of retrofitting them.
- **End-to-end tests drive the real binary.** A built `shephrd`, real git repositories and a fake harness are used throughout. The fake harness is a small Go program that runs scripted `shephrd report` calls, which is far simpler than the old JSONL replay fakes, since there is no output parsing.

## Package layout

| Package | Contents |
|---|---|
| `cmd/shephrd` | `main` only |
| `internal/cli` | cobra tree, `Request` and `Dispatch`, JSON success and error envelopes, exit codes, caller identity |
| `internal/config` | TOML schema v1, paths, strict decoding |
| `internal/store` | SQLite open, schema v1, forward-only migrations, transactions, idempotency records, event log |
| `internal/coord` | tasks, tree, ownership and authorization, lifecycle, requests, dependencies, attempts, runs |
| `internal/exec` | host operations, workspaces, briefs, the `_run` supervisor, liveness, stopping, reconciliation, built-in harnesses, headless presentation |
| `internal/proc` | process identity (PID plus start time), process-group stop and reap confirmation |
| `internal/notify` | `report`, routing, inbox, wake decisions, built-in webhook delivery |
| `internal/artifact` | deliverables, sealing, content-addressed store, inputs, delivery, landing, proof, grants, release |
| `internal/plugin` | manifests, packages, sync, the per-call protocol, hooks, provider registry, plugin commands, dispatcher cursors |
| `internal/daemon` | the daemon loop and lock: event dispatch, wakes with the settle window, push delivery, periodic reconciliation |
| `internal/transport` | SSH client forwarding with retries, `serve`, `agent` |
| `internal/guide` | embedded skill text, `replace` and `extend`, sections |
| `internal/testkit` | shared fixtures: git repository, built binary, fake harness, fake `ssh`, configuration writer |
| `plugins/` | first-party plugins (GitHub forge, Herdr, cmux, Pi delivery, macOS notifications), each a `plugin.toml` package |

## Lifted from history

Recovered from `main` with `git show`, then trimmed to the new types:

- **`internal/worktree/native.go`**: worktree allocate, inspect, verify, dirty, remove and setup hook. It goes into `exec`.
- **`internal/process/process_posix.go`**: `Detach` and group `Stop`. It goes into `proc`. Start-time identity and reap confirmation are new.
- **`internal/extension/host.go`** and **`protocol.go`**: process launch, the clean environment, bounded frames and stderr capture. It goes into `plugin`. The sha256 pinning is replaced by package pinning.
- **`internal/adapter/adapter.go`**: per-harness argv and resume flags for Claude Code, Codex and Pi. Envelope parsing and repair turns are dropped.
- **`internal/driverdelivery/webhook/webhook.go`**: HMAC signing, outcome classification and a safe HTTP client.
- **`internal/config/config.go`**: the loader skeleton and XDG paths.
- **`internal/forge/github`**: the `gh` GraphQL observe logic, which becomes the GitHub forge plugin.
- **`internal/terminal/herdr.go`** and **`cmux.go`**: clients that become presentation plugins.
- **`internal/store/baseline.go`**: the SQLite open settings (WAL, busy timeout, immediate transactions).

## Store schema v1 (outline)

- `repos`
- `tasks`: owner driver or parent, request, role, repo, deliverable, target, state, reason, attempt, revision, depth
- `dependencies`: `until` is `published` or `merged`
- `attempts`: base, stacked-on, branch, workspace path and state, host, harness, model, native session
- `runs`: generation, token hash, PID, start time, endpoint, liveness, reported, nudged
- `events`: global `seq`, name, task, attempt, run, caller, body, ref, data
- `idempotency`: caller, key, digest, result
- `inbox_items`: driver, event, task, ack, delivery state and backoff
- `plugin_cursors`, `plugin_data`
- `artifacts`, with blobs on disk keyed by digest
- `grants`, `grant_uses`, `landings`

Exact columns are settled in M1 and M2 against the specs.

## Milestones

Each milestone ends green on `make test`, `go vet ./...` and `git diff --check`, and is committed and pushed as its own PR-sized step.

### M1. Clear the ground and foundation

**Delete:**
- every package under `internal/`
- every `cmd/` binary except `cmd/shephrd`
- `.pi/`, `tests/pi/`, `scripts/build-clean.sh`
- the old `.agents/skills/shephrd/` skill and the `.shephrd/modules/` playbooks
- the root `design.md`, which `docs/design.md` replaces

Keep `docs/archive`.

**Rewrite:**
- `Makefile`: `build`, `install`, `test`, `test-docs`, `vet`. Drop `test-extension` and the live targets for now.
- the CI workflow
- the `AGENTS.md` verification list
- a minimal `README.md` and `OVERVIEW.md` pointing to `docs/design.md`

**Build:**
- the `TestDocumentation` link and Mermaid check, moved to a root `docs_test.go`
- `config` v1
- `store`: open, schema v1 `repos`, `events` and `idempotency`, the migration runner
- `cli` conventions: `Request` and `Dispatch`, JSON output, the error envelope with `next`, exit codes 0, 1 and 3, `warnings`, `--key` generation, `-` for stdin, field size bounds
- local caller identity: configuration driver, `--as`, run token, call token, operator
- the commands `init`, `version`, `repo add`, `repo list`
- `testkit` with the git fixture and the built binary

**Tests:**
- envelopes and exit codes
- an idempotent repeat and `key_conflict`
- `--as` refused when a token is present
- `repo add` refused for non-operators

### M2. Plugin mechanism

Specs: [extensibility](systems/extensibility.md) and the commands spec's plugin errors.

**Build:**
- manifests and protocol version checks
- packages: git pinned to a commit, path, explicit and convention discovery
- configuration-only declaration
- `plugin sync`, operator only
- per-use checkout verification
- the per-call protocol with clean environment, timeouts and size limits
- intercept hook runner: configured order, first block wins, fail closed with `plugin_blocked` and `plugin_failed`
- provider registry with built-in defaults
- plugin subcommands with call tokens, refusing to shadow core commands
- the `plugin:<name>` caller
- `plugin list`, `plugin status`, `plugin skill`
- an `events` read with `--after`

**Tests:**
- a script plugin for each surface
- a crashing or timing-out hook refuses
- an undeclared plugin never runs
- a dirty git checkout makes the plugin unavailable

### M3. Coordination

Spec: [coordination](systems/coordination.md).

**Build:**
- tasks and the tree
- ownership and authorization (read the subtree, mutate direct children, ancestors read only)
- the six-state lifecycle with reasons
- driver requests: every message is a request, children carry `request`
- sibling-only acyclic dependencies with `--until merged`. Readiness stays evidence. Milestone values come from artifacts in M6.
- attempts and runs with generation fencing and `stale` events
- revision and `--if-revision`
- the depth limit
- plugin data namespaces
- the built-in config-rule router

**Commands:** `task create`, `show`, `list`, `send`, `cancel`, `adopt`, `note`, `data set`.

**Extensibility:**
- intercept points: `task.create`, `start`, `send`, `adopt`, `cancel`
- all events in the spec's table

**Tests:**
- `not_owner` on a grandchild
- adoption of roots only
- a cycle refused
- depth refused
- a stale run's input recorded but refused
- closing refused with open children

### M4. Session execution, local and headless

Spec: [execution](systems/execution.md).

**Build:**
- `proc`: PID plus start time on Linux (`/proc`) and macOS (`sysctl` through `golang.org/x/sys/unix`), group stop, reap confirmation
- workspaces from the lifted worktree code: intent first, the `shephrd/<task>/<attempt>` branch, a fetched base with a divergence refusal, setup command, and refusing submodules and LFS
- briefs with size bounds
- the `_run` supervisor: clean environment, own process group, session log, exit record
- built-in harness providers for Claude Code, Codex and Pi: new and resume modes, read-only mode, assigning or discovering the native session ID
- headless presentation
- continuation resumes the native session; rotation from the checkpoint note
- read-only sub-driver workspaces
- inactivity timeout and the `turn_long` warning
- reconciliation
- `internal/guide` with first-draft text for all three roles, so briefs embed it

**Commands:** `task start`, `resume`, `retry`, `stop`, `log`, `workspace reconcile`, `host list` (local host), `skill main`, `skill subdriver`, `skill worker`.

**Tests:**
- the worker environment carries no driver identity or parent tokens
- `unknown` liveness refuses retry
- an interrupted allocation reconciles to `none`, `held` or `unknown`
- the inactivity timeout stops a run and nudges once

### M5. Notification, daemon and the first full loop

Spec: [notifications](systems/notifications.md).

**Build:**
- `report progress`, `question`, `result`, `blocker`, `note`, with `--request` for drivers and turn-ending rules per role
- one nudge, then `no_report`
- routing: driver task owners get turns, drivers get inbox items
- `inbox`, `inbox ack`, `inbox wait`
- auto-acknowledge on close
- the `report.result` gate
- the `daemon`, with a lock:
  - plugin event dispatch with per-plugin cursors, backoff and pause
  - wake turns with the 5 second settle window
  - worker continuation on `task send`
  - push delivery through the built-in signed webhook
  - `plugins.changed`
  - periodic reconciliation
- the `daemon_not_running` warning
- commands nudge the daemon over a local socket after commit, so wakes and pushes are immediate. The daemon also scans on an interval as a fallback.
- `inbox wait` and `events --follow` watch the store on the home host. Callers never poll.

**Acceptance (first vertical slice):**
1. The main driver creates a sub-driver.
2. The sub-driver plans two worker tasks with a dependency.
3. The workers report results.
4. The sub-driver is woken once (coalesced) and reports a per-request result.
5. The main driver's webhook receives the item and acknowledges it.
6. The same flow survives a daemon restart mid-way.

### M6. Artifacts and authority, direct landing

Spec: [artifacts](systems/artifacts.md).

**Build:**
- sealing at result time: code (clean, committed, on the attempt branch, beyond base), report (files copied into the content-addressed store), answer
- refusals the session can fix
- pinned inputs: a read-only inputs directory, answers in the brief
- `artifact show`, `artifact read`
- `task deliver` for reports and answers
- grants: configuration plus runtime `grant` and `grant revoke`, scoped to a subtree, never wider than the grantor's, uses recorded; `init` grants the main driver
- `direct` landing (fast-forward or merge commit, in a temporary worktree, push or local update)
- `landing_not_configured`
- ancestry proof and `task verify`
- `task discard` with `--attempt`
- release of clean workspaces on close; dirty ones held, with the owner told
- published and merged milestones feeding dependency readiness

**Tests:**
- a dirty code result is refused, then fixed and accepted
- a missing grant gives `not_granted`
- the registered checkout is never touched
- a dirty workspace survives close
- a pinned input is unchanged after the dependency is retried

### M7. SSH parity and worker hosts

Spec: [commands](systems/commands.md).

**Build:**
- `transport`: client forwarding when `home` is set, sending `{"v":1,"argv","key","run_token"}` over `ssh` with stdin piped
- bounded retries with the same key, then exit 3
- `version_mismatch`
- `serve`, with `--as driver:<name>`, `--as plugin:<name>` and `--host <name>`; run tokens bound to their host
- `request.refused` events
- `agent` on worker hosts: host operations only
- execution routed through the agent, with version and provider checks
- `unknown` on an unreachable host
- per-host execution provider declarations, including in `host list`

**Testing:** a fake `ssh` on `PATH` maps targets to forced commands, simulating `authorized_keys` through the real code path. An opt-in live test uses a real sshd.

**Tests:**
- a dropped connection after commit returns the stored result on retry
- `--as` over SSH is refused
- a run token from the wrong host is refused
- a sleeping worker host leaves runs `unknown` and refuses retry
- the same JSON comes back locally and remotely for the M5 acceptance flow

### M8. Pull request landing and the GitHub forge plugin

Spec: [artifacts](systems/artifacts.md), the [landing modes](systems/artifacts.md#landing-modes) and [stacking](systems/artifacts.md#stacking) sections.

**Build:**
- `pull_request` mode with `merge = "shephrd"` or `"external"`
- the forge provider contract: `publish`, `merge`, `observe`, `retarget`
- `plugins/github` (reusing the `gh` observe logic; adding push, PR create, merge, retarget)
- recorded-merge proof for squash and rebase merges
- stacking: base on the dependency's sealed commit, PR targets the dependency's branch, retarget on merge
- `dependency.changed` for a new seal, a discard or a rewriting merge

**Tests:** a fake `gh` covers stacked PRs, squash-merge proof and a review change pushed to the same PR. An opt-in live GitHub test runs against a scratch repository.

### M9. First-party plugins, guidance and cutover

**Build:**
- presentation plugins `plugins/herdr` and `plugins/cmux`, from the lifted clients
- a Pi delivery plugin that starts a turn in a Pi session
- a macOS notification event subscriber
- final skill text for all three roles, with `--section` references and `[skills.*]` `replace` and `extend`
- installing the main driver skill
- a full README and getting-started guide for one machine and for three machines
- reinstated opt-in live targets for Herdr, cmux and GitHub

**Acceptance:** the [three-machine example](systems/commands.md#example-three-machines) runs for real. The main driver is on `hermes`, the home host is `workhorse`, and `laptop` is a worker host.

## Failure modes carried over from the old end-to-end suite

Each becomes a named end-to-end test in the milestone shown:

| Failure mode | Milestone |
|---|---|
| Driver restart mid-plan | M5 |
| Ownership transfer on adoption | M3 |
| An ambiguous state fails closed | M3, M4 |
| A stale run's report is refused | M3, M5 |
| A report from a superseded attempt is refused | M3, M5 |
| A worker environment strips driver context | M4 |
| A busy worker's message is not lost | M5 |
| A stop before a turn finishes is recovered | M4 |
| Repeated push failure is retried and stays pending | M5 |
| Squash merge proven by the forge | M8 |
| A head advance after a result forces a new seal | M6, M8 |
| SSH: a forced command cannot impersonate | M7 |
| SSH: owner-fenced recovery | M7 |

## Verification

- Per milestone: the focused package tests, then `make test`, `make test-docs`, `go vet ./...` and `git diff --check`.
- From M5 on, the acceptance flow runs as an end-to-end test against the built binary with the fake harness.
- Opt-in live tests from M7 onwards: real sshd, real Claude Code, Codex and Pi, and real GitHub.
