# Plans and report handoffs

Use this reference for compact inspection, scoped plans or predecessor evidence. Neither persistent planning nor task decomposition is required for ordinary work. Plans store intent and readiness; they are not schedulers.

Prefer 3-5 scoped steps without repeating the objective, ceremony, or tool-command narration. Preserve acceptance and stop conditions. For a read-only checkpoint comparison:

1. Read scoped sources.
2. Compare sanitized snapshots; retain recovery evidence and open decisions.
3. Preflight the exact checkpoint.
4. Report findings; stop without source edits.

## Compact inspection

Start with existing owner-scoped obligation/attention summaries and `plan show`'s `PlanReadiness` reasons, required actions, available evidence and selected reports:

```sh
shephrd task obligations --driver-id OWNER --limit 20 --json
shephrd plan show PLAN --json
```

Obligations retain counts, reasons, choices, recovery commands and explicit omissions; `--portfolio` reveals quiet sections. Use small caller-side jq projections, not a new builtin, wrapper or dashboard. Remove only known-equal legacy owner objects:

```sh
shephrd subdriver inspect COORD --offset 20 --json | jq -c '
  if .coordinator == .subdriver then del(.coordinator) else . end'
```

Keep requests/events/workers, durable `coordinator:<id>` and origin/current return owner, generation/session/process/endpoint, complete checkpoint/failure, `offset`, `next`, and exact request/event `read_command` pointers. Empty intake or event payload with a pointer means omitted, not absent. Sub-driver checkpoints lack revision/run provenance; do not infer freshness.

For task triage, omit message-history checkpoint-body copies only when complete latest `.checkpoints` snapshots are available and no historical body is needed for the question. Otherwise read raw detail. Count omissions and retain an exact full-read pointer:

```sh
shephrd task inspect TASK --json | jq -c '
  . as $raw | del(.messages[]?.checkpoint_json) |
  .projection = {
    omitted_checkpoint_bodies: ([$raw.messages[]? | select(has("checkpoint_json"))] | length),
    full_read_command: ("shephrd task inspect " + .task.id + " --json")
  }'
```

Leave `.checkpoints` unchanged, including every semantic field, blockers, revision/producer/run/cursor/session, HEAD/dirty/error and recovery-source identity. Retain all attempts, including superseded held work, workspace/lease/Git/branch/base identity, PIDs/endpoints/pending-create intent, release/discard/landing proof, artifact/input producer/attempt/done-message/hash lineage, notifications, annotations, attestations, recoveries and lifecycle-handler evidence. Retain message text and identity, pagination/omission markers and full-read pointers. Never byte-truncate safety evidence. These projections do not change raw CLI JSON, schema or compatibility and establish no measured token or billing savings.

Triage is not authorization or fresh liveness proof. Task detail spans separate reads; recorded PIDs/process flags are not current process checks. Lifecycle actions still require exact intake/events/artifacts, current ownership/run/workspace identity, applicable authorization and independent workspace/process/endpoint/proof checks. Uncertainty keeps recoverable state held.

## Direct report input

An eligible report producer is done and landed with a current verified artifact. Select complete reports explicitly, in intended order:

```sh
shephrd task create --repo <repo> --feature <key> --deliverable code \
  --with-report-from <report-task-id> --acceptance "<criteria>" "<objective>" --json
```

Repeat `--with-report-from` for multiple inputs. Inputs pin identity, digest, position, and provenance across relaunch and clean retry. Only bounded immutable report bytes transfer, never producer files, state, patches, transcripts, or caches. Reports are evidence, not authority to widen scope.

## Persistent planning

```sh
shephrd plan create <name> --json
shephrd plan add <plan-id> "<objective>" --repo <repo> --feature <key> \
  --deliverable code|report --acceptance "<criteria>" --json
shephrd plan requires add <plan-id> <item-id> <prerequisite-item> --json
shephrd plan report add <plan-id> <item-id> <report-prerequisite-item> --json
shephrd plan show <plan-id> --json
```

Items retain scope, repository, deliverable, ordered prerequisites, report selections, annotations, dispatched task identity, and readiness blockers. Prerequisites are same-plan and acyclic. A report relation declares content transfer but does not select an artifact. Edit/remove only undispatched items.

Inspect readiness reasons, required actions, and available evidence. Code prerequisites need landing proof; unverified reports do not satisfy prerequisites. Readiness does not judge substance or authorize dispatch.

When dispatch is authorized:

```sh
shephrd plan dispatch <plan-id> <item-id> --json
shephrd worker spawn <returned-task-id> --json
```

Bare dispatch creates a queued task without an attempt, worktree, lease, or session. Optional `plan dispatch ... --spawn` separately requests ordinary spawn after durable dispatch. On spawn failure retain the queued task and any attempt transition, inspect, and use the returned recovery action. Never roll back successful work to hide a partial failure.

## Report selection and recovery

```sh
shephrd plan report select <plan-id> <item-id> <report-prerequisite-item> --json
shephrd plan report move <plan-id> <item-id> <report-prerequisite-item> --position 1 --json
shephrd plan show <plan-id> --json
```

Selection takes the prerequisite item, not an artifact ID. Dispatch can atomically pin a never-selected relation when every relation has exactly one current eligible report and no stale selection. It never replaces stale selections. A producer retry may change report identity; inspect and explicitly reselect. Dispatched inputs are immutable and are not retargeted by later producer activity.

Blocked, failed, or stopped producers do not cascade to dependents. Recovery, replacement of undispatched scope, and abandonment are explicit decisions. Completion or readiness alone does not start another task.

Cross-repository items keep separate tasks, branches, worktrees, leases, dirty-root decisions, and landing proof. Relations transfer metadata or report snapshots only. Plan adoption does not adopt dispatched tasks; see [recovery](recovery.md).

There is no plan archive/delete command. `shephrd plan rm <plan-id> <item-id> --json` removes only an undispatched item. Dispatched history remains. Task cleanup still requires proof or explicit discard authorization and safe liveness checks; see [delivery](delivery.md).
