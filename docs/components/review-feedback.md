# Optional review feedback

## Purpose and authority

Shephrd does not require independent review, re-review, or PR creation. Users and repositories may request a review workflow, but the base supplies only task, report, question, annotation, and follow-up mechanics. There is no review task type, automatic approval gate, or model-family rule.

## Available mechanics

When review is requested, an ordinary `report` task can inspect an exact branch and commit. Put the requested scope and any read-only restriction in its objective and acceptance criteria. A report deliverable controls the artifact contract; it does not itself prohibit edits.

If same-attempt feedback is desired, the implementation worker can checkpoint and emit a question while waiting for input. `worker send` continues that waiting task on its existing branch and session. An annotation records context but does not send findings or authorize changes. Review evidence applies to the inspected commit, not automatically to later commits.

```sh
shephrd task create --repo <repo> --feature <key> --deliverable report \
  --acceptance "<requested review criteria>" "<review subject and scope>" --json
shephrd worker spawn <report-task-id> --json
shephrd task inspect <report-task-id> --json
shephrd task verify-delivery <report-task-id> --json
shephrd worker send <waiting-task-id> "<authorized follow-up>" --json
```

These are available operations, not a required sequence for every change. A task may emit `done` when its own contract is satisfied without waiting for review approval.

## Lifecycle boundaries

A `done` task cannot receive `worker send` or same-worktree relaunch. Clean retry is a separate explicit lifecycle choice, not an automatic response to findings. Never take over another attempt's branch or transfer dirty state implicitly.

Report verification and release do not approve findings or authorize implementation, landing, or more tasks. Code landing remains explicit, and cleanup retains the ordinary proof and process-liveness requirements.

See [worker protocol](worker-protocol.md), [ownership and annotations](ownership-annotations.md), [recovery](recovery.md), and [delivery and release](delivery-release.md) for implemented mechanics. The [workflow module convention](../reference/workflow-modules.md) describes explicit selection of ordinary Markdown guidance, including an optional thorough-review playbook. No module loader or automatic review cycle is implemented.
