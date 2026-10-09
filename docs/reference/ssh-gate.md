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
| `task inspect`, `task obligations` | The task-notification read vector and owner-scoped obligations. |
| `wake ack` | The acknowledgement vector; the store requires the claim owner. |

`--help` or `-h` on an allowlisted path returns JSON help without running the command, after the same refused-flag and owner checks as any other request. Everything else is refused with `gate_command_refused`, including:

- `repo add` and `repo scan`, because setup hooks run arbitrary shell and discovery runs extensions;
- `wake drain`, `wake pump` and `wake renew`, because they would compete with the owner's watcher as a second consumer;
- `worker send`, see below;
- `wake watch`, `task adopt`, `subdriver adopt-request`, `subdriver recover` and `subdriver resume`, which are operator actions;
- task creation, worker spawn, retry, stop and relaunch, sub-driver dispatch and return, landing, verification, attestation, discard, release, archive, workspace and plan mutations, protocol, completion, legacy and private commands.

## Forced and refused flags

- `--json` is always set.
- `--driver-id <owner>` is set on every allowlisted command that accepts it (`subdriver handoff`, `reply`, `ls`, `task obligations`, `wake ack`). A request in which any `--driver-id` occurrence differs, including an empty one or an earlier occurrence followed by a matching one, is refused with `gate_owner_mismatch`. The check records every value the command's own flag parser assigns, so positional text after `--` stays literal. Commands without the flag, such as `task inspect`, do not receive it.
- `--driver-generation`, `--legacy-coordinator-json` and `--all-drivers` are refused with `gate_flag_refused`, because they change claim identity, output identity or owner scope.
- `subdriver handoff` always gets `--queue`. The request persists without starting a model session from the SSH process, whose environment has none of the operator's runtime context. The owner's [`wake watch`](../components/notifications-watchers.md#driver-agnostic-watcher) then activates the queued sub-driver in its own pump-only passes with the watcher's runtime context, which terminal runtimes such as Herdr need for panes. Without a watcher, the queued owner waits for an operator `wake pump` or `subdriver resume`.

## Why `worker send` is not allowlisted

The brief allows `worker send` only if the gate can confirm that the task belongs to a request owned by the gate owner. The only durable request-to-task link is a sub-driver's worker dispatch (`coordinator_workers`), and those tasks are owned and supervised by the `coordinator:<id>` sub-driver. [Authority](authority.md) and [sub-driver](../components/subdrivers.md) rules forbid main from supervising or adopting them, so a request-lineage check could only admit sends main may not make. Tasks a main driver owns directly have no request link at all, so the check would refuse them too, and comparing the task's owner instead would replace request lineage with a different contract. `worker send` itself has no owner check. The gate therefore refuses it with `gate_command_refused`. Answer a sub-driver's question with `subdriver reply`; follow-ups to a remote driver's own direct workers need a local operator.

## Output and audit

An allowed command's JSON stdout, JSON stderr error and exit status are exactly what the same `shephrd ... --json` invocation produces. Gate refusals write one JSON error with an `error` and `error_kind` to stderr and exit 1 without running anything.

Each invocation that loads configuration appends one JSON line to `<data_dir>/gate/audit.jsonl` (directory 0700, file 0600) with the time, owner, subcommand path, `allowed` or `refused`, exit status and any error kind. The path is taken from the command tree, never copied from the request. Audit lines never contain claim tokens, request bodies, reply text or other arguments.

## Limits

- The gate runs one command per SSH connection and keeps no state between requests.
- Read commands keep their CLI scope: `task inspect`, `subdriver inspect`, `subdriver request` and `subdriver event` read by identifier as they do locally and are not filtered to the gate owner.
- `repo context` resolves an explicit server path and may create an absent default configuration, as it does locally.

## Tests

`internal/cli/gate_test.go` covers the allowlist and forced flags for each path, refusals without execution, malformed, `null`-element, NUL and oversized requests, help after refused-flag and owner checks, repeated `--driver-id` occurrences, startup owner and environment refusals, hostile reply text, the private stdin file and its removal, stdin being ignored otherwise and audit redaction. `internal/cli/gate_e2e_test.go` runs the built CLI with `SSH_ORIGINAL_COMMAND` against a temporary store: a queued handoff from stdin, a `driver.delivery` request read compared with direct CLI output, a hostile reply and `wake ack`; and `null` elements, help with a foreign owner or refused flag, a differing `--driver-id` hidden by a later matching one and `worker send`, each refused without side effects, plus literal `--driver-id` text after `--`. These are isolated fixtures, not an SSH server or live activation.
