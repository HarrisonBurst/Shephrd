# AGENTS.md

<!-- shephrd-authority: worker-guidance -->

## Scope

This file governs contributors and implementation, investigation, test, documentation, and review workers operating in the Shephrd repository. It is repository context for changing the project, not orchestration policy for a Shephrd driver.

## Repository contract

- Preserve explicit task, attempt, workspace, notification, landing, verification, and release boundaries. Readiness and verification are evidence, not authorization for a different lifecycle action.
- Keep annotations, worker messages, plan readiness, artifact acceptance, landing proof, and release authorization distinct in code and tests.
- Treat uncertain ownership, process state, workspace identity, artifact lineage, and release state as unsafe. Fail closed and retain recoverable state.
- Reuse existing model types, store projections, command patterns, fixtures, and test helpers before adding parallel abstractions.
- Keep changes focused on the requested outcome. Do not edit unrelated generated artifacts or broaden public contracts without matching tests.

## Verification

Run the focused tests for the changed package first. Before delivery, run:

```sh
make test
make test-extension
go vet ./...
git diff --check
```
