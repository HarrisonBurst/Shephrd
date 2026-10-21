# Session execution

**Status:** approved. Implements the [system design](../design.md).

## Purpose

Session execution turns coordination's decisions into running sessions. It prepares an isolated workspace for each attempt, starts sub-driver and worker sessions in it, on the home host or a worker host, observes whether they are alive, and stops them. It never decides lifecycle outcomes. It reports what it did and observed to [coordination](coordination.md).

**Owns:** host operations, workspaces, briefs, launching and supervising sessions, liveness, stopping, session logs and reconciliation of interrupted effects.

**Does not own:** task state and run identity ([coordination](coordination.md)), how sessions report and when continuation turns start ([notification and result flow](notifications.md)), what counts as accepted and when a workspace may be removed ([artifacts and authority](artifacts.md)), or transport conventions ([command surface](commands.md)).

## Hosts

Every operation on a machine goes through that machine's **host agent**, the same `shephrd` binary:

- On the home host, execution calls it in-process.
- On a worker host, the home host connects over SSH with its own key, whose forced command on the worker host is `shephrd agent`. The agent accepts only host operations from the home host. Each operation is named in the SSH command with the protocol version and release, and its JSON request travels on stdin, so nothing secret appears in a process list.

A worker host's agent reports its release version and the harness and presentation providers installed on it. Execution checks these before routing anything there. Each host configures its own absolute, stable installed `cli_path`. Before advertising a host or launching a run, the agent checks that this path exists, is executable and reports the agent's release. It advertises that literal path, not its resolved store target or the home host's path. A host that cannot be reached makes every run on it `unknown`, never `exited`.

```mermaid
sequenceDiagram
    participant C as Coordination (home)
    participant E as Execution (home)
    participant A as Host agent (worker host)
    participant S as Supervisor and harness
    C->>E: start run: task, attempt, run, token
    E->>E: record intent
    E->>A: prepare workspace
    A-->>E: workspace identity
    E->>A: write brief, start supervisor
    A->>S: launch harness in presentation
    A-->>E: process and endpoint identity
    E->>C: run started
    S-->>C: shephrd report ... (via forwarding)
    S-->>E: exit status, through the agent
```

## Workspaces

Every attempt of a task with a repository gets one git worktree of that repository, on the repository's host. That includes sub-driver tasks, which then read and investigate in isolation. A driver task without a repository gets an empty scratch directory.

- **Branch:** `shephrd/<task>/<attempt>`. Branches are never deleted.
- **Base:** the registered default branch, fetched from its remote when the attempt is created and recorded as an exact commit. Without a remote, the local default branch is used. If the local and fetched branches have diverged, creation is refused rather than guessing. Coordination guarantees dependencies are finished first, so the base already contains merged ones. A task that [stacks](artifacts.md#stacking) on published but unmerged code starts from that dependency's sealed commit instead.
- **Intent first:** execution records the intended path and branch before creating anything, then verifies the path, branch, common git directory and base commit before marking the workspace `held`.
- **Setup:** the repository's registered setup command runs once in a new workspace. It is trusted code chosen by the operator at registration.
- **The registered checkout is never modified.** Execution only fetches into it, and never copies its uncommitted or ignored files.
- **Not supported yet:** repositories with active submodules or Git LFS are refused at workspace creation.

```mermaid
stateDiagram-v2
    [*] --> allocating: intent recorded
    allocating --> held: identity verified
    allocating --> none: nothing was created
    allocating --> unknown: partial or mismatched
    held --> releasing: removal authorized
    held --> unknown: identity no longer provable
    releasing --> released: exact removal done
    releasing --> held: safe retryable failure
    releasing --> unknown: ambiguous result
```

Resume reuses the attempt's held workspace exactly, with no re-fetch, reset or rerun of setup. Retry creates a new attempt and a new workspace from a fresh base, and never copies anything from the old one. Removal happens only when [artifacts and authority](artifacts.md#discard-and-release) authorizes it, after execution verifies exact identity, no live process and, unless discarding, a clean tree. An `unknown` workspace is never removed automatically.

## Home-host main coordinator

The optional main coordinator is not a task, attempt or workspace. `main start` creates its private directory under the home host's data directory and a durable UUID session identity, once. The daemon selects at most one pending surface input or `driver:main` inbox item in ID order, records the turn intent, and launches a private supervisor with a PID and process start identity. It uses the built-in headless Claude Code command with its durable `--session-id` until a successful result establishes the session, then `--resume` for subsequent turns, in the same private directory and home account. The supervisor records its own identity before starting Claude, writes the stream-json log and an exit record, and does not expose the private coordinator token to other harnesses. The daemon never starts a second turn until the first has exited, produced a valid result and its output is committed. A configured inbox return surface/thread is snapshotted with the selected turn, not looked up again on completion or restart. A successful inbox result is stored once in that scoped mailbox before the turn advances, independently of inbox acknowledgement; output capacity failure holds the turn with its result for recovery rather than retrying the session. An unknown process identity, missing exit record, nonzero exit or missing result holds the turn and leaves input intact; neither daemon restart nor a second operator command retries it. Recovery requires operator evidence and resolution of the correlated reply before advancing; for a held inbox turn with a completed routed result it first persists any missing output, then records a separate operator acknowledgement after independent inspection. There is no unsafe automatic replay. The sole coordinator session may act as `driver:main` only during its active turn, and acknowledges inbox items itself after handling. The daemon does not acknowledge them. The coordinator is independent of task-run supervision and cannot grant itself authority.

## Sessions

A run is one turn of one session. It starts, works until it reports a result, question or blocker, and exits. Nothing waits idle in a process.

### Starting a run

1. Coordination reserves the run generation and token.
2. Execution writes the **brief** into the workspace: role, title, objective, acceptance, the paths of [pinned inputs](artifacts.md#passing-artifacts-on), the latest notes, and for a driver task an overview of every open request with its children and the events since its last turn, plus a pointer to the repository's context file, and how to report. Each brief is bounded in size, and longer material is referenced by commands to read it rather than included.
3. The host agent starts `shephrd _run`, the **supervisor**, with its pinned `os.Executable()` path, including in a terminal presentation. The running supervisor remains that original executable for process-group ownership and exit recording. The brief and `run.json` instead carry this host's literal stable installed `cli_path`. The supervisor invokes `_started` and `_turn` through that path on each new exec; the harness receives a clean environment: `PATH` with the installed path's directory first, `HOME`, the OS account's `USER`, `LOGNAME` and `TMPDIR` (harnesses find their logins through them), the host's `SHEPHRD_CONFIG` and `SHEPHRD_RUN_TOKEN`. Driver identity, terminal credentials and any parent Shephrd tokens are never passed through. An already running supervisor keeps its process identity across activation, while subsequent commands resolve the installed path afresh.
4. Process identity, meaning process ID with start time, and any presentation endpoint are recorded before the harness receives the brief. If the supervisor exits before recording its identity, an exit record or the bounded startup observation establishes only an exited or unknown run; execution never infers that an unrecorded process or endpoint is absent.

### Presented runs

With a terminal presentation, such as Herdr or cmux, people watch the harness work. The harness runs its own interactive interface in the terminal instead of its headless mode. Its first message points it at the brief. The supervisor checks the run for an ended turn every two seconds. Once the run has reported its result, question or blocker, or is no longer the task's current run, the supervisor waits a short grace for the harness to finish writing, then ends it. People can still type into the harness while the turn runs.

A terminal is labelled with the task's human-readable title. An empty title falls back to the stable task ID; duplicate titles remain valid and may produce identical labels, while the task ID stays in Shephrd's task and run metadata for exact identification. Its shell executes the supervisor, rather than remaining behind it; after a reported turn the supervisor exits and the terminal closes with its own process. A continuation opens a fresh terminal while resuming the task's native harness session in the same attempt and workspace; terminal reuse is not session resume. After the harness exits, the supervisor retries its turn-status query before showing a no-report hint; it always writes the exit record. For an unreported presented run, the daemon waits the 30-second turn grace for inspection before superseding it; a headless run is eligible immediately. The daemon processes the record for workers and drivers alike. When the harness exits without reporting or fails to start, the supervisor prints a task-log hint and holds the terminal until Enter, run supersession or task stop; supersession interrupts that wait. The daemon can supersede an unreported run after the grace, then give the run its one nudge. Execution never closes a presentation endpoint by ID: a probe cannot establish ownership atomically with a later close.

### Continuation

When a waiting or done task receives a message, or a driver task's children report, [notification and result flow](notifications.md) asks execution for a new run of the same attempt. Every new run resumes the task's native harness session:

- **A worker** resumes its own session, keeping its working memory of the change it is making.
- **A sub-driver** is one continuous session for its repository. Every turn resumes it, so it keeps context across all its requests and workers. A turn handles everything new since the last one, across all requests at once, and its brief always includes the overview of open requests and children, so it stays oriented after the harness compacts its context.

**Every driver turn ends with a checkpoint note:** its plan, decisions, what it is waiting on and what comes next. The note is how a session hands over when it has to be replaced.

**Rotation.** A new session is started, seeded from the brief and the latest checkpoint note, when:

- the harness reports its context is nearly full
- the native session cannot be resumed, for example because it was lost or the harness cannot resume
- the task was retried, since a new attempt has a new workspace and may have a new harness

Rotation is recorded as an event. A harness that cannot resume at all starts a new session every turn the same way.

### Reporting and output

Sessions report only by calling `shephrd report`, defined by [notification and result flow](notifications.md#reporting). Execution never parses harness output. The supervisor writes the session's output to a log on its host, readable with `task log`, but the log is never evidence of a result.

A run that exits without reporting a result, question or blocker gets **one nudge**: a continuation run whose brief says it finished without reporting and must call `shephrd report`. If that run also exits without reporting, the task is `held` with reason `no_report`.

### Inactivity

There is no fixed deadline. A run that reports nothing, not even progress, for its role's inactivity timeout, 30 minutes by default and configurable per role, is stopped as below and gets one nudge the same way. If the nudged run is also inactive, the task is `held` with reason `inactive`. A long run that keeps reporting progress is never stopped.

### Read-only sub-drivers

Sub-drivers plan and supervise; [workers implement](skill.md#sub-driver). Execution enforces that split:

- A driver run asks its harness provider for **read-only mode**, which disables file-editing tools where the harness supports it.
- The files in a driver task's workspace are made **read-only** before each run. Briefs and logs live outside the workspace, so nothing a driver needs to write is inside it.
- A sub-driver can still read, search and run `shephrd` commands.

This is a guard rail, not a security boundary: it turns an accidental implementation into an error, not a judgment call.

**Long turns.** A driver turn that has run longer than its turn budget, 10 minutes by default and configurable, gets one nudge: the next `shephrd` command it runs returns a `turn_long` warning telling it to delegate the remaining work and report. The turn is never stopped for length.

## Liveness and stopping

| Observation | Meaning |
|---|---|
| `live` | The recorded process exists with the recorded start time. |
| `exited` | The supervisor reported the exit, or the host agent proved the process is gone. |
| `unknown` | The host is unreachable, or the probe could not prove either. |

The supervisor writes the harness exit record before it exits; it cannot report its own process as exited while it still runs. The daemon or explicit reconciliation probes the recorded supervisor identity and process group through the host agent and applies the exit record only after both are proven gone. On Linux, a group containing only zombies counts as gone: those processes cannot execute, even if their parent has not reaped them and a signal-0 group probe still succeeds. A live member, unreadable process state, or an uninspectable process table does not prove the group gone. If startup never records a supervisor identity, a durable exit record proves that supervisor's exit; otherwise the bounded startup observation changes the run to `unknown` and holds the task for recovery without inferring an absent process or endpoint. An uncertain identity or live group stays unknown, not exited; a host outage retains recoverable state. If the supervisor identity is gone while a group member remains, the group is not treated as owned and execution does not signal it; the run stays unknown and the task is held with an owner notification. Stopping records `exited` only after the group is confirmed gone. A stop requested before `_started` refuses `_started`, so it cannot launch a harness after the stop. Only proven `exited` lets coordination resume, retry or release.

Stopping sends a termination signal to the run's whole process group only while the recorded supervisor identity still proves ownership, waits a bounded grace period, then kills it. The run counts as stopped only once the host agent confirms no executable group member remains; on Linux, unreaped zombies alone do not block this confirmation. If ownership or the absence of live group members cannot be confirmed, the run is `unknown`; the task is held only from `running` or `waiting`, while a `done` or `closed` task remains in that state with an owner notification, and nothing that needs the run gone proceeds. A PID-less unknown presentation run whose shell never execed the supervisor can be recovered only with explicit `task stop --abandon`, after the host confirms the recorded endpoint is absent and the run directory still exists without an exit record. Abandonment records the run exited but leaves the task and workspace held; it is unavailable without a presentation endpoint and is not proof from an ordinary endpoint observation. A normal stop never removes the workspace.

## Reconciliation

Interrupted effects, such as a crash between recording intent and creating a worktree, are classified by reconciliation:

- An `allocating` workspace with nothing on disk becomes `none`. One whose exact worktree exists becomes `held`. Anything partial or mismatched becomes `unknown`. Before applying a classification, reconciliation rechecks that the attempt is still `allocating`, so it cannot overwrite a concurrently completed workspace identity or base commit.
- A run whose process is proven gone becomes `exited`.
- An authorized removal that was interrupted is finished once its identity is re-verified.

Reconciliation never starts a run, chooses resume or retry, or invents authority. The daemon runs it at startup and periodically, and `workspace reconcile` runs it on demand.

## Harness and model

The task's target names the harness and an opaque model string. Execution passes the model to the harness unchanged and never translates it. A model is never carried across a harness change. Resume keeps the attempt's harness and model. Retry uses the task's current target, which the owner may change.

## Commands

| Command | Effect |
|---|---|
| `task start`, `task resume`, `task retry` | Start a run on a new attempt, the same attempt or a new attempt, after coordination's checks |
| `task stop` | Stop the current run, optionally for a whole subtree; `--abandon` explicitly recovers an unstarted presentation run |
| `task log` | Read the current or a named run's session log |
| `workspace reconcile` | Classify interrupted effects on every host |
| `host list` | Configured hosts with reachability, version and installed providers |

`shephrd agent` requires the same release on home and worker hosts; `shephrd _run`, `_started` and `_turn` are private commands, not public commands. An in-flight pinned supervisor may call a newer installed CLI's `_started` and `_turn` using its unchanged run token, PID/start-time and ended-turn wire semantics. This bounded in-flight contract does not permit an old CLI to read a newer store schema or permit cross-release host agents; incompatible private changes require draining runs or an explicit transition. The supervisor records its own process identity with `_started` before it starts the harness, even if its launcher dies. An unsuccessful `_turn` lookup never proves completion.

## Failure behavior

| Situation | Behavior |
|---|---|
| Host unreachable | Its runs are `unknown`; resume, retry and removal on that host are refused. Starting new work there is refused. |
| Host on a different release | Refused with `version_mismatch` before anything is created. |
| Installed CLI path missing, non-executable or wrong release at launch | Refused without falling back to PATH or the pinned executable; existing run liveness remains recoverable. A missing path during an active run fails new commands and private lookups, not proof of an exit. |
| Base cannot be resolved or has diverged | Workspace creation refused; the task stays as it was. |
| Setup command fails | Workspace stays `held`; the task is `held` with reason `setup_failed`. |
| Harness fails to start | Task `held` with reason `start_failed`; the workspace stays `held`. |
| Supervisor shell never execs | The PID-less run becomes `unknown` after the startup grace; explicit `task stop --abandon` is the only recovery, and it leaves the task and workspace held. |
| Run exits without reporting | One nudge, then task `held` with reason `no_report`. |
| Run reports nothing for its inactivity timeout | Stopped, one nudge, then task `held` with reason `inactive`. |
| Process group cannot be confirmed free of live members | Run `unknown`; nothing that needs it gone proceeds. |
| Workspace identity mismatched | Workspace `unknown`; never removed automatically. |

## Extensibility

Follows [plugins and extensibility](extensibility.md). Execution providers run on the host where the session runs, so each host declares the providers installed on it in its own configuration, and the host agent reports them.

### Events

| Event | `data` |
|---|---|
| `workspace.state` | Attempt, host, path, branch, base commit, from, to |
| `run.started` | Run, host, harness, model, presentation endpoint |
| `run.liveness` | Run, from, to |
| `run.exited` | Run, exit status, whether it reported |
| `run.nudged` | Run, reason: `no_report` or `inactive` |
| `session.rotated` | Task, old and new native session, reason |
| `host.reachability` | Host, reachable or unreachable |

### Intercept points

None of its own. Coordination's `task.start` gate runs before every run starts, including continuation turns.

### Providers

| Type | Request | Response | Built-in |
|---|---|---|---|
| `harness` | Mode (new or resume), read-only flag, interactive flag, brief path and first message, workspace, model, title, native session ID when resuming | Command, extra environment, and the native session ID it assigned, if it supports resume | Claude Code, Codex, Pi |
| `presentation` | Operation: open, probe, focus. For open: title, command and workspace | Endpoint identity; probe answers `present`, `absent` or `uncertain` | Headless |

A harness provider only builds the command. The supervisor runs it, so the provider never owns process identity. A presentation provider such as Herdr or cmux runs the supervisor in a terminal whose shell is replaced by the supervisor. Execution never asks a provider to close an endpoint: neither an earlier probe nor a second check before an ID-based close can protect against a concurrent change in ownership. Only an exact `absent` answer counts as the endpoint being gone; absence alone does not prove the supervisor's process group has exited. A host names its presentation with `presentation` in its own configuration. With a terminal presentation, the run token reaches the supervisor through a private file in the run directory that the supervisor removes, never through a command line or a terminal's environment, and the supervisor records its own process identity once it starts.

### Limits

Providers never decide liveness, change workspace identity, see run tokens other than through the environment they are given, or translate models. Execution re-verifies every process and workspace identity itself.

## Settled decisions

1. **Every task with a repository gets its own worktree**, sub-drivers included. Sub-driver runs are read-only, with a turn budget nudge.
2. **Every run resumes its task's native session.** A sub-driver is one continuous session per repository, ending each turn with a checkpoint note and rotating to a new session from that note when it must.
3. **No output parsing.** Sessions report only through `shephrd report`, with one nudge before `no_report`.
4. **Built-in providers:** Claude Code, Codex and Pi harnesses and headless presentation ship in core. Herdr and cmux are plugins.
5. **Execution providers are declared per host**, in that host's configuration.
6. **Inactivity timeout instead of deadlines**, for both roles, with one nudge before `inactive`.
