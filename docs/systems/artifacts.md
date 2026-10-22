# Artifacts and authority

**Status:** approved. Implements the [system design](../design.md).

## Purpose

Artifacts and authority decides what a task produces, captures it, proves when it has been accepted, and gates irreversible actions on authority the user granted. Finishing work never implies permission to land, discard or release it.

**Owns:** deliverable types, sealing and storing artifacts, passing artifacts to dependent tasks, acceptance and its proof, landing code, discard, when workspaces are released, and grants.

**Does not own:** task state ([coordination](coordination.md)), creating and removing workspaces ([session execution](execution.md)), or how results reach owners ([notification and result flow](notifications.md)).

## Deliverables

A task's deliverable is fixed when it is created.

| Deliverable | The result carries | Accepted when |
|---|---|---|
| `code` | The attempt branch, sealed at a commit | The sealed commit is proven on the repository's default branch |
| `report` | One or more files from the workspace, sealed as snapshots | The owner accepts it |
| `answer` | The result text itself | The owner accepts it |

Workers deliver `code` or `report`. Driver tasks deliver `answer` by default, or `report`.

**Results carry summaries; large content moves by reference.** A result's text is a bounded summary. Files and code travel as artifacts, which an owner reads only if it chooses to.

## Sealing

When a session reports a result, the artifact is checked and sealed in the same step, after the `report.result` gate:

- **Code:** the workspace must be clean, with everything committed, `HEAD` on the attempt branch, and at least one commit beyond the base. Shephrd records the exact commit. A code result with uncommitted changes or no commits is refused, and the session can fix that and report again.
- **Report:** each named file must be a regular file inside the workspace. Shephrd copies it to the home host's content-addressed artifact store, records its digest and size, and makes the copy read-only. Snapshots have a configurable size limit.
- **Answer:** the result text is the artifact.

A sealed artifact never changes. If the task is reopened by a message, including one sent during the prior run, its continuation must report an ordinary validated result even when no more work is needed. The new result seals a new artifact (which may name the same code commit), and only the latest counts as current. A note alone does not complete the worker turn or preserve the previous artifact as current.

A driver task's result automatically lists the current artifacts of the request's children, alongside their states, so the owner sees what was produced without the driver restating it.

## Passing artifacts on

When a task starts, the artifacts of its [dependencies](coordination.md#dependencies) are pinned into its inputs:

- **Report** files are placed in a read-only inputs directory beside the workspace, outside the repository. The brief lists their paths, never their contents, so a session reads only what it needs.
- **Code** needs no copy. A merged dependency is already in the default branch the workspace starts from. A dependency that is published but not merged becomes the workspace's base instead, as described in [stacking](#stacking). The brief names the commit.
- **Answer** text is included in the brief, since it is already bounded.

Pinned inputs never change. If a dependency changes afterwards, the dependent task's owner is told, as described in [stacking](#stacking).

`artifact show <id>` prints an artifact's metadata, and `artifact read <id>` streams its bytes. Both work over SSH like any command.

## Acceptance

`task accept <task>` is the canonical verb for accepting a done task; `task deliver <task>` remains a CLI compatibility alias with identical gates and effects:

- **Report or answer:** the direct owner reviews and accepts the current result. The task closes as `accepted`. No grant is needed, because nothing irreversible happens: the snapshot already exists.
- **Code:** `task accept` lands the sealed commit using the repository's landing mode and proves it. This needs the `land` grant in every mode, because every mode publishes code. A standing `land` grant is prior user authorization; a delegated grant records approval in its reason. Core checks the grant, not a separate approval field. Code already merged outside Shephrd can instead close as `accepted` through `task verify` after core proves it is on the default branch; verification does not publish code or require a `land` grant. In pull-request mode, publishing alone does not accept the task.

A sub-driver reviews and accepts its own report/answer children and verified merged code children; the main driver accepts only the sub-driver parent. Neither reporting a parent result nor accepting it accepts, lands or discards a child. A driver task can close only after every direct child is closed, including done unmerged code. Active, waiting or questioned children must be resolved by their owner or escalated upstream, never closed automatically. Obsolete unmerged code stays open until proven merged or individually approved for discard; there is no superseded auto-close.

### Milestones

Every deliverable passes two milestones. [Dependencies](coordination.md#dependencies) wait for one of them.

| Deliverable | Published | Merged, with accepted closure on proof |
|---|---|---|
| Report or answer | The owner accepted it | Same moment |
| Code, `direct` mode | Merged into the default branch | Same moment |
| Code, `pull_request` mode | Branch pushed and pull request open | The pull request merged into the default branch |

The task closes as `accepted` at the merged milestone. Between the two, a code task stays `done` and shows that it is awaiting merge, with its pull request. Historical `delivered` task closure reasons remain valid and readable; no stored rows or events are rewritten. The milestone `merged` remains distinct from task acceptance and webhook transport `delivered`.

### Landing modes

Each repository's configuration chooses how its code lands. There is no default: a repository without a landing mode refuses to deliver code with `landing_not_configured`, so code is never pushed somewhere by accident.

```toml
[repos.my-tool.landing]
mode = "direct"
method = "fast-forward"

[repos.side-project.landing]
mode = "pull_request"
forge = "github"
merge = "shephrd"

[repos.work-api.landing]
mode = "pull_request"
forge = "github"
merge = "external"
```

| Mode | `task accept` does | Merged when |
|---|---|---|
| `direct` | Merges the sealed commit into the default branch in a temporary worktree, fast-forward-only or with a merge commit, and pushes it. Without a remote, it updates the local default branch. | Immediately |
| `pull_request`, `merge = "shephrd"` | Pushes the branch from the repository's host and opens a pull request through the forge provider, then merges it once the forge reports it mergeable, for example after checks pass. While checks are pending the task stays published; running `task accept` again merges it. | Shephrd merges it |
| `pull_request`, `merge = "external"` | Pushes the branch and opens a pull request | Someone merges it on the forge |

- Landing never touches the registered checkout.
- A conflict, rejected push or failed merge leaves the task `done`. The owner can send the worker a message to fix it, which reopens the task.
- Review changes work the same way: a message reopens the task, the worker commits, its new result seals a new commit, and `task accept` pushes it to the same pull request.
- In `pull_request` mode, `task accept` may supply a PR title, body and `--draft` when publishing. When omitted, new PRs retain the task-reference/title and sealed-result-text defaults and open ready for review. The forge checks a known or discovered open PR before creation. An existing PR is reused without changing its title, body or draft state; explicitly supplied values (including `--draft=false`) must match its current metadata or publication is refused. Matching values allow safe retry after a creation whose recording failed. Multiple matching PRs or an unreadable or closed recorded PR are refused rather than guessed. Retargeting an existing PR remains independent of publication metadata. Draft PRs are not merged by Shephrd while draft.
- Shephrd learns that a pull request merged from the forge plugin, typically through a forge webhook that runs `task verify`, so nothing polls. The owner can run `task verify` at any time too.

### Stacking

In a `pull_request` repository, a task that depends on published but unmerged code **stacks** on it:

- Its workspace starts from the dependency's sealed commit on the dependency's branch, not the default branch.
- Its pull request targets the dependency's branch, so it is reviewed on top of the dependency's.
- When the dependency's pull request merges, the forge provider retargets the stacked pull request to the default branch.

A dependency that waits with `--until merged` never stacks: the task starts from the default branch once the dependency has merged.

Stacked work can go stale. A **`dependency.changed`** event goes to the owner of every task built on a dependency when that dependency:

- seals a new commit, for example after review changes
- is discarded
- merges in a way that rewrites history, such as a squash merge

The owner decides what to do, typically telling the worker to rebase. Shephrd never rebases on its own.

### Proof

Code is accepted only when core proves it is on the default branch:

- **Ancestry:** the sealed commit is reachable from the default branch, on the remote when there is one. Core checks this itself.
- **Recorded merge:** only a durable pull-request publication for the *current* sealed artifact in this repository can identify the candidate PR. Core requires a readable, merged forge record and a merge carrier reachable from the remote default branch. Forge state and reachability establish lineage, not landed content. A live PR head may have moved or been deleted after merge; it is not a historical merged-head attestation. If the forge supplies a reliable historical merged head, it must match the sealed commit. Without one, the artifact-bound publication and exact content proof below are both required. Missing, unreadable, closed-unmerged or mismatched evidence cannot close the task; `task verify` never adopts an unrecorded PR.
- **Rewritten history content:** for a squash or provable single-commit rebase, core takes the immutable sealed artifact's task-only `Base -> Commit` delta, applies it to the carrier's first parent (the premerge tree) in an isolated index, and requires the resulting **full Git tree ID** to equal the carrier's tree ID. Conflicts, missing objects, changed file modes, extra or omitted landed changes fail proof. An unrelated commit already in the premerge parent is allowed; later unrelated commits on the default branch do not affect proof. For stacked work the sealed Base is the parent's sealed commit: only the child's delta is replayed, never the parent's changes. A carrier not reachable from default is not proof. Multi-commit rebase needs a forge-attested, unambiguous premerge boundary and complete landed commit range; the last rewritten commit's first parent alone is insufficient. Without that range evidence, ambiguous multi-commit rebase fails closed rather than guessing a boundary. Proof records the sealed base and commit, PR, carrier, premerge parent, compared tree IDs, kind and source.

`task verify <task>` re-checks proof for code merged outside Shephrd. Verification never publishes code, changes repository history or requires a `land` grant.

## Discard and release

- **Discard:** `task discard <task>` closes a task as `discarded` and removes its workspaces even if they hold uncommitted changes. Committed work survives on its branches. This needs the `discard` grant and explicit per-action user approval, even with a standing `discard` grant. A delegated grant records that approval in its reason for the specific action; it does not blanket-approve later discards in its subtree. Cancelling a task that never started needs no grant.
- **Release:** when a task closes, session execution removes each of its attempts' workspaces that are clean, after re-verifying identity and that no process is live. Report files already sealed from the workspace do not count as changes, since their snapshots are kept. A workspace with other uncommitted changes stays held, and its owner gets an inbox item or a continuation turn naming it. Removing it then needs `task discard --attempt <n>`.
- Branches are never deleted, and every artifact, proof and event survives.

## Grants

Irreversible actions need a grant: `land` or `discard`. Owning a task is not enough.

**Standing grants are declared in configuration**. A standing `land` grant is prior user authorization; a standing `discard` grant supplies capability but never replaces explicit per-action approval:

```toml
[[grants]]
to = "driver:main"
actions = ["land", "discard"]

[[grants]]
to = "subdriver"
repo = "api"
actions = ["land"]
```

The first lets the main driver land and discard in its own trees. The second lets every sub-driver for repository `api` land its own workers' code.

**Grants can be delegated down the tree at runtime.** A caller holding a grant can pass it to one of its child tasks, never wider than its own:

```sh
shephrd grant land --to t_42 --reason "User approved landing for the auth refactor"
shephrd grant revoke g_7
```

- A grant to a task covers that task's subtree.
- Every grant records who granted it, when, why and its scope. Each use of a grant is recorded with the action it allowed.
- Revoking a grant stops future uses only.
- **By default, nothing passes down.** `shephrd init` writes a configuration granting the main driver `land` and `discard`, and sub-drivers get nothing until configuration or a runtime grant gives it to them. Core uses the grant as runtime authorization; the land prior authorization and discard approval rules are policy, not separate core checks.

## Failure behavior

| Situation | Behavior |
|---|---|
| Code result with uncommitted changes, no commits or the wrong branch | Result refused; the session fixes it and reports again. |
| Report file missing, outside the workspace or over the size limit | Result refused with the reason. |
| `task accept` on code without a `land` grant | Refused with `not_granted`, naming the action. |
| `task accept` on code in a repository without a landing mode | Refused with `landing_not_configured`. |
| Landing conflict or rejected push | Landing fails; the task stays `done`. |
| Landing provider fails or times out | Nothing is recorded as landed; the task stays `done`. |
| Proof cannot be established | The task stays `done`; `task verify` can be run again. |
| Workspace dirty at close | It stays held and its owner is told; removal needs `task discard --attempt`. |

## Extensibility

Follows [plugins and extensibility](extensibility.md).

### Events

| Event | `data` |
|---|---|
| `artifact.sealed` | Task, attempt, run, deliverable, commit or file digests and sizes |
| `land.attempted` | Task, sealed commit, landing mode, outcome: `published`, `merged` or `failed` |
| `task.published` | Task, artifact, branch and pull request when there is one |
| `dependency.changed` | Dependent task, dependency, what changed: new commit, discarded, or merged with rewritten history |
| `task.delivered` | Task, artifact, proof kind and its source. Retained as the stable event name for acceptance, including new `accepted` closures; old events and IDs remain unchanged. |
| `grant.added`, `grant.revoked` | Grant, grantor, grantee, actions, scope, reason |
| `grant.used` | Grant, action, task |
| `pull_request.retargeted` | Stacked task, pull request, new target branch |
| `workspace.retained` | Task, attempt, path, why it was kept |

### Intercept points

| Point | Gates |
|---|---|
| `task.deliver` | Accepting any task, including landing code. Retained as the stable gate name for existing plugins, for both CLI verbs. A typical gate requires passing CI before landing. |
| `task.discard` | Discarding a task or attempt |
| `grant.add` | Delegating a grant at runtime |

The `report.result` gate in [notification and result flow](notifications.md#intercept-points) runs before sealing.

### Providers

| Type | Request | Response | Built-in |
|---|---|---|---|
| `forge` | Operation `publish` (open, or find, the pull request for a branch Shephrd already pushed, against a target branch), `merge`, `observe` (whether and how a pull request merged) or `retarget`, with repository and its remote, branch, sealed commit and task summary. Publish carries effective title and body, booleans indicating whether each was explicitly supplied, and the requested draft state with an explicitness boolean. | Outcome `ok`, `pending` or `failed` with a reason; pull request reference, state, head and merge commit | None; `direct` mode needs no provider. The first-party `github` plugin uses the `gh` CLI |

### Limits

Plugins never mark a task accepted, create or widen grants except through `grant` under their own identity, or change sealed artifacts. Core establishes every proof itself, and a provider's answer is evidence for it.

## Settled decisions

1. **Three deliverables:** `code`, `report` and `answer`.
2. **Canonical `task accept` verb, with `task deliver` compatibility.** It accepts reports and answers without a grant, and lands code with the `land` grant in every landing mode. Existing artifact IDs, proofs, events and gate names remain stable across upgrades and rollbacks; new closures use `accepted`, old `delivered` closures remain readable without migration.
3. **Shephrd lands code itself**, using each repository's landing mode: `direct`, or `pull_request` merged by Shephrd or externally. There is no default mode. `task verify` proves merges done outside Shephrd.
4. **Authority:** standing grants are declared in configuration, runtime grants delegate down the tree, and by default only the main driver can land and discard.
5. **Reports are sealed when the result is reported**, and passed on as files beside the dependent's workspace rather than inside its brief.
6. **Code is proven by ancestry or by a merged pull request recorded on a forge with exact task-only content proof** for rewritten history. Unattested multi-commit rebase ranges are not proven.
7. **Closing a task releases clean workspaces automatically**; dirty ones stay held until discarded.
8. **Published and merged milestones.** In `pull_request` repositories, dependent tasks stack on published work by default, and `--until merged` waits for the merge instead.
