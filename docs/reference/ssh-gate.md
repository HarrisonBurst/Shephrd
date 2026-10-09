# Remote SSH gate

## Purpose

`shephrd gate --driver-id <owner>` lets one remote, non-Pi main driver run a fixed set of Shephrd commands over SSH as that owner. It is an SSH forced command, not a shell, a daemon or a general remote CLI. It grants no authority beyond the allowlisted commands, which keep every existing owner, claim and lifecycle check.

## authorized_keys

Give the remote driver its own key and pin the owner in the forced command:

```text
command="/abs/path/shephrd gate --driver-id driver:hermes",no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-user-rc ssh-ed25519 AAAA... hermes
```

The owner comes only from the gate's own argv, never from the request. Startup refuses an empty owner (`driver_context_required`) and, like [`wake watch`](../components/notifications-watchers.md#driver-agnostic-watcher), any `driver:pi:*` or sub-driver owner (`gate_owner_refused`). It also refuses to run with `PI_SESSION_ID`, `SHEPHRD_WORKER` or a sub-driver session identity in its environment (`gate_environment_refused`), because those would replace the forced owner. Configuration and database selection come only from the server-side environment (`SHEPHRD_CONFIG`, `SHEPHRD_STATE_DIR`, `SHEPHRD_DATA_DIR`, XDG directories); Shephrd has no global flag that selects them.

## Request protocol

`SSH_ORIGINAL_COMMAND` holds exactly one JSON array of strings: the Shephrd argv without the leading `shephrd`. A leading `"shephrd"` element is accepted and dropped, so the `driver.delivery` command vectors run unchanged after the consumer fills `<text>` and `<key>`:

```sh
ssh -i hermes_key host "$(jq -cn --arg text "$answer" '["shephrd","subdriver","reply","request_1",$text,"--reply-to","42","--key","answer-1","--driver-id","driver:hermes","--json"]')"
```

Nothing is shell-parsed or shell-evaluated on the gate side. Each element reaches Shephrd as one literal argument, so metacharacters, quotes and newlines in reply text arrive byte for byte. The remote side must still build the SSH command from JSON rather than interpolating text into its own shell. The gate refuses non-JSON input, trailing data, non-string elements including `null` in any position, NUL bytes, an empty argv and a request larger than 256 KiB with `gate_request_invalid`.

The gate parses the request against the same Cobra command tree, then runs the canonical argv in-process: the subcommand path, every set flag as `--name=value`, then `--` and the positional arguments. Unknown flags, missing or extra positional arguments, aliases such as `coordinator`, and flags before the subcommand path are refused before anything runs.

Stdin is read only when the request contains `--request-file -`. The gate then copies at most 256 KiB of stdin into a private 0600 temporary file, substitutes its path and removes it afterwards; larger input is refused. Otherwise stdin is never read and the command receives empty input. Use stdin for a long original request, since the request itself is bounded and passes through the environment.

## Allowlist

Exact subcommand paths:

| Path | Why it is allowed |
|---|---|
| `repo list`, `repo context` | Read-only repository discovery for a driver choosing a scope. |
| `subdriver handoff` | Forward original intake; always queued (see below). |
| `subdriver reply` | The `driver.delivery` reply vector for sub-driver returns; the store requires the request's owner. |
| `subdriver inspect`, `subdriver request`, `subdriver event`, `subdriver ls` | Read vectors and owner inspection. |
| `subdriver diagnose` | Read-only recovery evidence for a sub-driver with a request returning to the gate owner (see [owner recovery](#owner-recovery)). |
| `subdriver recover` | Owner-fenced recovery of a held sub-driver every linked request of which returns to the gate owner. |
| `subdriver resume` | Owner-fenced manual resume for an explicitly headless runtime while no `wake watch` serves the owner. |
| `task inspect`, `task obligations` | The task-notification read vector and owner-scoped obligations. |
| `wake ack` | The acknowledgement vector; the store requires the claim owner. |

`--help` or `-h` on an allowlisted path returns JSON help without running the command, after the same refused-flag and owner checks as any other request. Everything else is refused with `gate_command_refused`, including:

- `repo add` and `repo scan`, because setup hooks run arbitrary shell and discovery runs extensions;
- `wake drain`, `wake pump` and `wake renew`, because they would compete with the owner's watcher as a second consumer;
- `worker send`, see below;
- `wake watch`, `task adopt`, `subdriver adopt-request` and `subdriver context`, which are operator actions;
- task creation, worker spawn, retry, stop and relaunch, sub-driver dispatch and return, landing, verification, attestation, discard, release, archive, workspace and plan mutations, protocol, completion, legacy and private commands.

## Forced and refused flags

- `--json` is always set.
- `--driver-id <owner>` is set on every allowlisted command that accepts it (`subdriver handoff`, `reply`, `ls`, `diagnose`, `recover`, `resume`, `task obligations`, `wake ack`). A request in which any `--driver-id` occurrence differs, including an empty one or an earlier occurrence followed by a matching one, is refused with `gate_owner_mismatch`. The check records every value the command's own flag parser assigns, so positional text after `--` stays literal. Commands without the flag, such as `task inspect`, do not receive it.
- `--driver-generation`, `--legacy-coordinator-json` and `--all-drivers` are refused with `gate_flag_refused`, because they change claim identity, output identity or owner scope.
- `subdriver handoff` always gets `--queue`. The request persists without starting a model session from the SSH process, whose environment has none of the operator's runtime context. The owner's [`wake watch`](../components/notifications-watchers.md#driver-agnostic-watcher) then activates the queued sub-driver in its own pump-only passes with the watcher's runtime context, which terminal runtimes such as Herdr need for panes. Without a watcher, the queued owner waits for an operator `wake pump` or `subdriver resume`.

## Owner recovery

A held sub-driver normally needs a local operator. The gate lets the owner's remote driver diagnose and release one when every fence below holds, and otherwise leaves it held. The commands are the [local recovery commands](../components/subdrivers.md#restart-and-recovery) with the forced `--driver-id`, which selects their owner-fenced form.

```sh
ssh -i hermes_key host '["subdriver","inspect","coord_1"]'
ssh -i hermes_key host '["subdriver","diagnose","coord_1"]'
ssh -i hermes_key host '["subdriver","recover","coord_1","--generation","3","--model","exact-model-id"]'
```

`subdriver diagnose` changes nothing. It reports the generation, state, retained harness, model and runtime; each recorded runner and harness PID as `absent`, `live_or_uncertain` or `unrecorded`; the recorded terminal endpoint as `absent`, `present`, `uncertain` or `unrecorded`, probed read-only through the configured terminal extension; whether launch identity is complete; the request owners with counts; whether work is pending; and `recoverable` with any `blockers`. It never reads session or launcher logs and never prints claim or session tokens. With the forced owner it refuses (`subdriver_owner_refused`) a sub-driver with no request returning to that owner. Use `inspect`, `request` and `event` for the requests and returns themselves.

`subdriver recover <id> --generation <n>` with the forced owner adds these fences to every existing check:

- The exact inspected generation must still be current.
- The sub-driver must be `held` (`subdriver_not_held` otherwise); local recovery of a `starting` or `running` owner stays a local operator action.
- Every linked request, including completed ones, must return to the gate owner. Mixed owners, a request adopted by another driver and a sub-driver without requests are refused with `subdriver_owner_refused`.
- The held state and the owner check are re-read in the same immediate store transaction that releases the hold, so an adoption, handoff or other recovery in between is refused rather than overwritten.
- The existing server-side fences are unchanged: a live or reused recorded runner or harness PID, and a present or uncertain recorded endpoint, refuse recovery even with `--launch-absent`. Recovery never closes an endpoint.

Incomplete launch identity (no recorded runner or harness PID, or a terminal runtime without a recorded endpoint) stays held unless the request carries `--launch-absent "<evidence and reason>"`: a nonblank operator assertion of at most 512 bytes that unrecorded launch effects are absent, naming the independent evidence, such as launcher logs and the exact `shephrd:coordinator:<id>:<generation>` source, checked on the server. The recovery event records the assertion and the gate owner. A remote driver that cannot obtain such evidence must leave the owner held and ask a local operator; `diagnose` output alone is not that evidence.

`--model <exact-id>` replaces the retained model for the retained harness in the same transaction as the held-to-idle transition, so the next reservation cannot use the old model. The identifier must be one token of at most 256 bytes, without whitespace, control characters or a leading `-`; `--model=` selects the harness-native default. Recovery refuses a model when no harness is retained. Without `--model` the retained selection is kept. Reservation also compares the harness and model it read, so an activation that read the owner before a model-changing recovery is refused, and a later pass reads the new selection, instead of reserving the old model.

Recovery never starts a model. It queues a recovery input for each unfinished request, and the owner's [`wake watch`](../components/notifications-watchers.md#driver-agnostic-watcher) activates the pending turn on its next pass in its own Herdr or headless runtime context.

### Manual resume without a watcher

`subdriver resume <id> --generation <n>` with the forced owner is for an owner whose watcher is stopped. It holds the owner's watcher lock in shared mode for the whole command, so it fails closed with `wake_watch_active` while a watcher runs, and a watcher cannot start mid-resume. It additionally requires:

- `--generation`, compared with the reservation;
- every linked request returning to the gate owner, checked atomically with the reservation;
- a retained runtime of `headless` or none, and a server runtime (`SHEPHRD_WORKER_RUNTIME`, then `worker_runtime`, then the headless default) of exactly `headless`. A retained Herdr or cmux runtime, or a configured `auto`, `herdr` or `cmux`, is refused with `subdriver_resume_refused`, so an SSH process never runs a Herdr turn and never falls back from Herdr to headless;
- no `--foreground`.

The existing idle-state, process-absence and endpoint checks still apply, `--model` keeps its generation-fenced meaning, and an empty queue starts nothing. The resumed runner is the ordinary detached headless runner. Terminal-runtime owners wait for their watcher or a local operator.

## Why `worker send` is not allowlisted

The brief allows `worker send` only if the gate can confirm that the task belongs to a request owned by the gate owner. The only durable request-to-task link is a sub-driver's worker dispatch (`coordinator_workers`), and those tasks are owned and supervised by the `coordinator:<id>` sub-driver. [Authority](authority.md) and [sub-driver](../components/subdrivers.md) rules forbid main from supervising or adopting them, so a request-lineage check could only admit sends main may not make. Tasks a main driver owns directly have no request link at all, so the check would refuse them too, and comparing the task's owner instead would replace request lineage with a different contract. `worker send` itself has no owner check. The gate therefore refuses it with `gate_command_refused`. Answer a sub-driver's question with `subdriver reply`; follow-ups to a remote driver's own direct workers need a local operator.

## Output and audit

An allowed command's JSON stdout, JSON stderr error and exit status are exactly what the same `shephrd ... --json` invocation produces. Gate refusals write one JSON error with an `error` and `error_kind` to stderr and exit 1 without running anything.

Each invocation that loads configuration appends one JSON line to `<data_dir>/gate/audit.jsonl` (directory 0700, file 0600) with the time, owner, subcommand path, `allowed` or `refused`, exit status and any error kind. The path is taken from the command tree, never copied from the request. Audit lines never contain claim tokens, request bodies, reply text or other arguments.

## Limits

- The gate runs one command per SSH connection and keeps no state between requests.
- Read commands keep their CLI scope: `task inspect`, `subdriver inspect`, `subdriver request` and `subdriver event` read by identifier as they do locally and are not filtered to the gate owner.
- Remote recovery evidence is limited to `diagnose`: process liveness by PID and endpoint presence by the configured extension. The gate cannot show logs or scan for unrecorded processes, so incomplete identity without server-side evidence stays held.
- `repo context` resolves an explicit server path and may create an absent default configuration, as it does locally.

## Tests

`internal/cli/gate_test.go` covers the allowlist and forced flags for each path, refusals without execution, malformed, `null`-element, NUL and oversized requests, help after refused-flag and owner checks, repeated `--driver-id` occurrences, startup owner and environment refusals, hostile reply text, the private stdin file and its removal, stdin being ignored otherwise and audit redaction. `internal/cli/gate_e2e_test.go` runs the built CLI with `SSH_ORIGINAL_COMMAND` against a temporary store: a queued handoff from stdin, a `driver.delivery` request read compared with direct CLI output, a hostile reply and `wake ack`; and `null` elements, help with a foreign owner or refused flag, a differing `--driver-id` hidden by a later matching one and `worker send`, each refused without side effects, plus literal `--driver-id` text after `--`. `internal/cli/gate_recovery_e2e_test.go` runs the built CLI and a real `wake watch` with a deterministic headless Pi fixture: a gate handoff completes one request, a second turn loses its runner and is held, recovery is refused while the completed request is adopted by another driver, for a stale generation, for an invalid model and for resume while the watcher runs; `diagnose` reports absent processes without tokens or log text; recovery with `--model` lets the watcher run the next turn with the selected model; after the watcher stops, Herdr, `auto`, foreground, ungenerationed and stale resumes start nothing and an owner-fenced headless resume completes the request. A second fixture refuses a live recorded runner and an uncertain recorded Herdr endpoint even with `--launch-absent`, refuses incomplete identity without a nonblank assertion, records an accepted assertion with the owner, and refuses resume of a retained Herdr owner. Store and control tests cover completed and mixed owners, non-held and repeated recovery, retained-harness model validation, bounded assertions, present, uncertain and absent endpoints, and reservation after a model-changing recovery. These are isolated fixtures, not an SSH server or live activation.
