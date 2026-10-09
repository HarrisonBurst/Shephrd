# CLI conventions and utility commands

## Purpose

This reference owns behavior shared by the command families and the support commands that do not belong to one lifecycle component. Use the current CLI help for complete command paths and flags; component guides own lifecycle semantics.

## Bootstrap and authority

Except for help, shell completion, the private Claude hook client, `protocol validate`, and `repo context`, a command first loads or creates strict configuration, then classifies the state database read-only. A fresh database is created directly on the consolidated baseline; a database at a supported bridge version is upgraded one-shot after its frozen migration identities and schema shapes validate; a database at or beyond the baseline is validated and opened. Below-floor and newer databases are refused before any read-write handle exists, with exact recovery instructions and no database changes. A nominal read command can therefore create a fresh baseline database before its command-specific read. Trusted report lifecycle handlers run only from report verification, never from a read command.

Cobra validates syntax and flags at the public boundary. `internal/control` coordinates side effects, `internal/store` owns durable state, and Git, GitHub, the filesystem, processes, harnesses, and explicitly configured extensions remain external authorities. External GitHub observation for attestation is unavailable unless its pinned observer extension is configured; ordinary exact-head PR verification uses the verifier's direct `gh pr view` path.

`shephrd repo context <path>` loads configuration and resolves project context without opening SQLite or registering the repository. It may initialize an absent user config, but reads no memory contents and creates no repository files. It is available to direct-work drivers before registration; see [project context discovery](../components/repositories.md#project-context-discovery).

## Output and errors

The global `--json` flag requests machine-readable output where supported. Most successful operator commands encode current Go values without a versioned public schema. Explicit schema-bearing surfaces include obligations, help, attestations, wake acknowledgement receipts, worker stop, workspace release, workspace reconcile, and the `report.accepted` handler protocol. Delivery JSON includes persisted lifecycle handler annotations, receipts, failures, and explicit retry commands when present. A wake acknowledgement receipt uses schema 1 and always includes the stable handling ID, including when the CLI generated or recovered it.

When an invocation lexically requests JSON and fails, stderr contains an `error` field and may contain an `error_kind`. Failures with a safe explicit recovery path may also contain stable command fields such as `recovery_command`, `verify_command`, `retry_command`, `inspect_command`, `reconcile_command`, or `release_command`. `wake drain`, `wake pump`, and a second `wake watch` refused by an active watcher report `wake_watch_active` with `driver_id` and, when recorded, `watcher_generation` and `watcher_pid`. `wake unpark` for a notification that is not parked for that owner reports `notification_not_parked`. Wake shorthand identity errors always include `recovery_command` and name the missing or conflicting notification, token, owner, generation, or handling identity. Consumers must not infer a typed kind when none is present. A composed `plan dispatch --spawn` failure reports the already dispatched task's exact ID and normal recovery command in that error rather than hiding durable partial success. Shell completion is the exception: it emits a raw script and rejects `--json` before script output.

## Help

`shephrd help` and `shephrd <command> --help` read the current Cobra tree without opening Shephrd state. JSON help wraps rendered help with schema version and command identity. Family and group commands show their help when invoked without a leaf subcommand.

## Completion

`shephrd completion bash`, `fish`, and `zsh` emit Cobra-generated scripts. Completion has no lifecycle authority and does not load configuration or SQLite.

## Event format preflight

`shephrd protocol validate --role worker|subdriver --file <path|-> --json` validates exact authored assistant output using the ingestion parser, without loading configuration or state. Stdin is the default. It reports format validity and bounded diagnostics, not lifecycle authorization, artifact acceptance or delivery proof. See [worker protocol](../components/worker-protocol.md#read-only-format-preflight) for typed checkpoint examples, limits and repair boundaries.

## Private commands

`shephrd _run` executes one attempt and run generation using a private input file protocol. `shephrd _claude-hook` carries bounded Claude hook frames to the trusted bridge. Both are hidden implementation commands, not stable operator APIs. Their output follows private runtime protocols rather than ordinary operator JSON.

## Legacy export

`shephrd legacy export <output-dir>` publishes the bounded read-only export of a legacy state database: the original database file, the raw table names, column orders, and row counts, the content-addressed digest of the retired legacy task-list tables, and the referenced report snapshot files. The database is opened read-only and is never migrated or rewritten. Below-floor exports (schema 1-14) are bounded by the compiled window that closes exactly thirty calendar days after the baseline release; after it closes, the archived signed legacy exporter is the only reader for those schemas. Bridge-version exports (schema 15-28) stay available for this binary's lifetime so the baseline migration can accept their digest.

## Failure and recovery

A bootstrap failure prevents command-specific behavior. Migration, configuration, ownership, identity, process, or external-evidence uncertainty fails closed at the component boundary. Recovery uses the linked component guide rather than retrying a destructive command blindly.

## Commands

- `shephrd help`
- `shephrd repo context <path>`
- `shephrd protocol validate --role worker|subdriver --file <path|-> --json`
- `shephrd completion bash`
- `shephrd completion fish`
- `shephrd completion zsh`
- `shephrd legacy export <output-dir>`
- `shephrd gate --driver-id <owner>` (SSH forced command; see [remote SSH gate](ssh-gate.md))
- `shephrd _run`
- `shephrd _claude-hook`

Worker spawn and retry also accept one explicit base flag: `--base-branch`, `--base-task`, or `--base-commit`. Without one, normal spawn uses the current default-branch strategy and retry preserves the prior declared strategy.

## Design rationale

Most commands are bounded process invocations rather than requests to a daemon. `wake watch` is the one long-running foreground command; it serves a single owner under an exclusive lock, runs the same bounded store and activation operations as the manual commands, and holds no authority beyond claim delivery. This keeps durable state and side effects explicit. Versioned schemas are reserved for integration surfaces that deliberately promise them; unversioned output remains free to evolve with the prototype. Hidden runtime commands keep worker execution inside the same binary without presenting private protocols as operator contracts.
