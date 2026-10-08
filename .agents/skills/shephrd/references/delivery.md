# Delivery and release

Use this reference after an artifact is accepted or when delivery proof needs recovery. Inspect the exact task, attempt, accepted artifact, sealed commit/report, workspace, and process state first:

```sh
shephrd task inspect <task-id> --json
shephrd worker status <task-id> --json
```

An accepted artifact is a task branch, GitHub PR URL, or `report:<absolute-path>`. Neither an open PR nor a branch proves landing. Do not rewrite an accepted artifact or substitute an externally discovered PR. Landing requires an explicit authorized merge outside verification.

## Verification and external evidence

```sh
shephrd task verify-delivery <task-id> --json
shephrd task verify-delivery <task-id> --local --json
```

Use `--local` for local-default-branch proof. Verification records evidence and may release the exact attempt, but never merges. Reports receive an immutable verified snapshot and report landing proof. If proof fails, retain the workspace.

For an accepted branch delivered through a merged external PR, explicitly approve the relationship, record immutable attestation, inspect it, then verify separately:

```sh
shephrd task attest-delivery <task-id> --pr <canonical-pr-url> \
  --commit <full-sealed-commit> --attempt <attempt-id> --json
shephrd task inspect <task-id> --json
shephrd task verify-delivery <task-id> --attempt <attempt-id> --json
```

Attestation does not replace the artifact, push, merge, verify, release, or control workers. New local recovery attestations are not accepted. Existing immutable local recovery records can still be checked with ordinary `verify-delivery --local`.

## Exceptional report handoff recovery

If terminal handoff failed after the canonical report and current final checkpoint were written, use the exact owner-scoped recovery command returned by `task obligations` or failure JSON. Do not infer completion from file existence or synthesize worker output. Confirm the current attempt, run, checkpoint revision/cursor, dead process and endpoint, held workspace, and absence of an accepted current-generation worker terminal:

```sh
shephrd task attest-report-recovery <task-id> \
  --attempt <attempt-id> --run-generation <generation> \
  --checkpoint-revision <revision> --checkpoint-cursor <cursor> \
  --reason "<handoff failure>" --driver-id <owner> --json
shephrd task inspect <task-id> --json
shephrd task verify-delivery <task-id> --attempt <attempt-id> --driver-id <owner> --json
```

Attestation preserves the failed/blocked audit and records driver evidence, not a worker message or artifact. It freezes send and same-worktree relaunch for that attempt. Verification revalidates the exact workspace, checkpoint, canonical regular file identity and bytes. The alternatives are verification or explicit clean retry; retry does not retarget old evidence.

## Cleanup

If proof was recorded but release failed, inspect and retry release:

```sh
shephrd workspace release <task-id> --attempt <attempt-id> --json
```

Release still requires exact workspace identity and safe process/endpoint liveness. Use `--discard` only with explicit authorization to discard that exact attempt's unlanded work. Unknown state stays held; use `workspace reconcile --help` for the recorded recovery case rather than deleting directories manually.

```sh
shephrd task archive <task-id> --json
shephrd task list --include-archived --json
```

Archive requires a terminal task and exited worker. It changes visibility only, not proof, release, discard, or acknowledgement.
