# Quick change

An optional playbook for small, well-understood changes. It applies only when selected by the task request or applicable repository guidance, and can be adapted by explicit task instructions.

## Approach

1. Confirm the exact outcome, bounded scope, stop condition, and expected answer size. The driver normally assigns one worker, including for a small change; explicitly requested direct work remains direct.
2. The implementer inspects the relevant files and makes the requested change. Do not add a separate planning, review, or PR stage merely because code changes.
3. Run focused validation plus any checks required by the repository or task.
4. Inspect the final diff for unintended changes. Report the result, checks, and unresolved concerns, then stop.

## When to reconsider

Secrets, destructive operations, authorization changes, or unexpectedly broad effects may need a different approach even when the diff is small. Explain the concrete risk and resolve any material change in scope before proceeding. Do not automatically spawn reviewers or switch modules.

## Boundaries

This playbook reduces workflow overhead, not required evidence or safety. It does not waive repository constraints, change worker artifact contracts, or authorize merging, release, discard, deployment, or successor work.
