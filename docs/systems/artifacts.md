# Artifacts and authority

**Status:** approved. Implements the [system design](../design.md).

## Purpose

Artifacts and authority decides what a task delivers, captures it, proves when it has been delivered, and gates irreversible actions on authority the user granted. Finishing work never implies permission to land, discard or release it.

**Owns:** deliverable types, sealing and storing artifacts, passing artifacts to dependent tasks, delivery and its proof, landing code, discard, when workspaces are released, and grants.

**Does not own:** task state ([coordination](coordination.md)), creating and removing workspaces ([session execution](execution.md)), or how results reach owners ([notification and result flow](notifications.md)).

## Deliverables

A task's deliverable is fixed when it is created.

| Deliverable | The result carries | Delivered when |
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

A sealed artifact never changes. If the task is reopened by a message and reports again, the new result seals a new artifact, and only the latest counts as current.

A driver task's result automatically lists the current artifacts of the request's children, alongside their states, so the owner sees what was produced without the driver restating it.

## Passing artifacts on

When a task starts, the artifacts of its [dependencies](coordination.md#dependencies) are pinned into its inputs:

- **Report** files are placed in a read-only inputs directory beside the workspace, outside the repository. The brief lists their paths, never their contents, so a session reads only what it needs.
- **Code** needs no copy. A merged dependency is already in the default branch the workspace starts from. A dependency that is published but not merged becomes the workspace's base instead, as described in [stacking](#stacking). The brief names the commit.
- **Answer** text is included in the brief, since it is already bounded.

Pinned inputs never change. If a dependency changes afterwards, the dependent task's owner is told, as described in [stacking](#stacking).

`artifact show <id>` prints an artifact's metadata, and `artifact read <id>` streams its bytes. Both work over SSH like any command.

## Delivery

`task deliver <task>` is the one verb for delivering a done task:

- **Report or answer:** the owner accepts the current result. The task closes as `delivered`. No grant is needed, because nothing irreversible happens: the snapshot already exists.
- **Code:** Shephrd lands the sealed commit using the repository's landing mode, then proves it. This needs the `land` grant in every mode, because every mode publishes code.

A driver task can be delivered only once all its children are closed.

### Milestones

Every deliverable passes two milestones. [Dependencies](coordination.md#dependencies) wait for one of them.

| Deliverable | Published | Merged, which is delivered |
|---|---|---|
| Report or answer | The owner accepted it | Same moment |
| Code, `direct` mode | Merged into the default branch | Same moment |
| Code, `pull_request` mode | Branch pushed and pull request open | The pull request merged into the default branch |

The task closes as `delivered` at the merged milestone. Between the two, a code task stays `done` and shows that it is awaiting merge, with its pull request.

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

| Mode | `task deliver` does | Merged when |
|---|---|---|
| `direct` | Merges the sealed commit into the default branch in a temporary worktree, fast-forward-only or with a merge commit, and pushes it. Without a remote, it updates the local default branch. | Immediately |
| `pull_request`, `merge = "shephrd"` | Pushes the branch and opens a pull request through the forge provider, then merges it once the forge reports it mergeable, for example after checks pass | Shephrd merges it |
| `pull_request`, `merge = "external"` | Pushes the branch and opens a pull request | Someone merges it on the forge |

- Landing never touches the registered checkout.
- A conflict, rejected push or failed merge leaves the task `done`. The owner can send the worker a message to fix it, which reopens the task.
- Review changes work the same way: a message reopens the task, the worker commits, its new result seals a new commit, and `task deliver` pushes it to the same pull request.
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

Code is delivered only when core proves it is on the default branch:

- **Ancestry:** the sealed commit is reachable from the default branch, on the remote when there is one. Core checks this itself.
- **Recorded merge:** a pull request merged on a forge that records it, such as GitHub or GitLab, read through that forge's plugin. Core checks that the pull request's head is the sealed commit and its merge commit is on the default branch, and records the proof as coming from that forge. This is how squash and rebase merges are proven, since they break ancestry.

`task verify <task>` re-checks proof for code merged outside Shephrd. Verification never changes repository history.

## Discard and release

- **Discard:** `task discard <task>` closes a task as `discarded` and removes its workspaces even if they hold uncommitted changes. Committed work survives on its branches. This needs the `discard` grant. Cancelling a task that never started needs no grant.
- **Release:** when a task closes, session execution removes each of its attempts' workspaces that are clean, after re-verifying identity and that no process is live. A workspace with uncommitted changes stays held, and its owner gets an inbox item or a continuation turn naming it. Removing it then needs `task discard --attempt <n>`.
- Branches are never deleted, and every artifact, proof and event survives.

## Grants

Irreversible actions need a grant: `land` or `discard`. Owning a task is not enough.

**Standing grants are declared in configuration**, which is the user's explicit authority:

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
- **By default, nothing passes down.** `shephrd init` writes a configuration granting the main driver `land` and `discard`, and sub-drivers get nothing until configuration or a runtime grant gives it to them.

## Failure behavior

| Situation | Behavior |
|---|---|
| Code result with uncommitted changes, no commits or the wrong branch | Result refused; the session fixes it and reports again. |
| Report file missing, outside the workspace or over the size limit | Result refused with the reason. |
| `task deliver` on code without a `land` grant | Refused with `not_granted`, naming the action. |
| `task deliver` on code in a repository without a landing mode | Refused with `landing_not_configured`. |
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
| `task.delivered` | Task, artifact, proof kind and its source |
| `grant.added`, `grant.revoked` | Grant, grantor, grantee, actions, scope, reason |
| `grant.used` | Grant, action, task |

### Intercept points

| Point | Gates |
|---|---|
| `task.deliver` | Delivering any task, including landing code. A typical gate requires passing CI before landing. |
| `task.discard` | Discarding a task or attempt |
| `grant.add` | Delegating a grant at runtime |

The `report.result` gate in [notification and result flow](notifications.md#intercept-points) runs before sealing.

### Providers

| Type | Request | Response | Built-in |
|---|---|---|---|
| `forge` | Operation `publish` (push and open a pull request against a target branch), `merge`, `observe` (whether and how a pull request merged) or `retarget`, with repository, branch, sealed commit and task summary | Pull request reference, merge commit, or `failed` with a reason | None; `direct` mode needs no provider |

### Limits

Plugins never mark a task delivered, create or widen grants except through `grant` under their own identity, or change sealed artifacts. Core establishes every proof itself, and a provider's answer is evidence for it.

## Settled decisions

1. **Three deliverables:** `code`, `report` and `answer`.
2. **One `task deliver` verb.** It accepts reports and answers without a grant, and lands code with the `land` grant in every landing mode.
3. **Shephrd lands code itself**, using each repository's landing mode: `direct`, or `pull_request` merged by Shephrd or externally. There is no default mode. `task verify` proves merges done outside Shephrd.
4. **Authority:** standing grants are declared in configuration, runtime grants delegate down the tree, and by default only the main driver can land and discard.
5. **Reports are sealed when the result is reported**, and passed on as files beside the dependent's workspace rather than inside its brief.
6. **Code is proven by ancestry or by a merged pull request recorded on a forge**, which also covers squash and rebase merges.
7. **Closing a task releases clean workspaces automatically**; dirty ones stay held until discarded.
8. **Published and merged milestones.** In `pull_request` repositories, dependent tasks stack on published work by default, and `--until merged` waits for the merge instead.
