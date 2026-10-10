# Authority map

## Purpose

Shephrd separates policy, executable mechanics, repository guidance, and migration guidance. This map says which resource may decide each kind of question and prevents one layer from silently taking authority from another.

## Authority by concern

| Concern | Canonical owner | What it controls | What it does not control |
| --- | --- | --- | --- |
| Workflow preferences and requested outcome | User instructions and applicable repository guidance | Whether to delegate, split tasks, request review, select models, or deliver a PR | Ownership, artifact identity, or bypassing lifecycle safety |
| Basic execution and lifecycle safety | [Shephrd skill](../../../.agents/skills/shephrd/SKILL.md) and on-demand [driver guidance](../../../.agents/skills/shephrd/references/driver-policy.md) | Default delegation preference, proportionate effort, core commands, identity, authorization, and evidence boundaries; optional references for plans, recovery, and delivery | Mandatory delegation, review, PR creation, model-family choices, or successor work |
| Contributor and worker conduct inside this repository | [Repository AGENTS.md](../../../AGENTS.md) | Implementation, testing, documentation, and repository-specific invariants | Driver orchestration policy |
| Contributor and worker conduct in another registered repository | That repository's registered `AGENTS.md` or `CLAUDE.md` | Standing local constraints and conventions | Shephrd lifecycle authorization or unrelated task scope |
| Repository context and optional modules | User-selected or repository-referenced Markdown guidance, described by the [context convention](workflow-modules.md) | Repository-scoped workflow preferences and explicitly selected playbooks | Global defaults, executable enforcement, or lifecycle authorization |
| Repository memory | Repository-scoped notes and their supporting sources, reached through `memory.md` | Factual context, rationale, and references to verify when relevant | Module selection, current-state proof, repository policy changes, or lifecycle authorization |
| Public command names, arguments, flags, bootstrap, and immediate behavior | Current Cobra tree in `internal/cli` plus the [CLI reference](cli.md) | Executable CLI contract | Driver judgment or external Git/GitHub truth |
| State transitions, durable records, runtime effects, and safety checks | Current code, embedded migrations, and tests, explained by the [component guides](../README.md#components-and-mechanisms) | Enforced product mechanics | User approval, semantic correctness, or external facts not revalidated by code |
| Opt-in repository coordination | Original user request, current scoped owner and [sub-driver mechanics](../components/subdrivers.md) | Technical planning, ordinary worker supervision, correlated questions/results and replaceable sessions | Recursive managers, implicit cross-repository access, worker adoption by main, or new approval/landing/release powers |
| Worker response shape and checkpoint freshness | `internal/brief`, `internal/adapter`, `internal/model`, `internal/store`, and the [worker protocol guide](../components/worker-protocol.md) | Accepted event grammar and transactional ingestion | Whether a worker's claimed checks or conclusions are true |
| Git, filesystem, process, terminal, and report facts | The relevant external system, observed through current verifier and control code | External identity and evidence | Driver authorization or SQLite intent |
| GitHub delivery evidence | GitHub, observed for external attestation through the SHA-pinned `shephrd-github-observer` extension and revalidated by current verifier and control code | Bounded immutable observations and the core-computed evidence digest | Landing, release, or any lifecycle claim; driver authorization or SQLite intent |
| Exceptional report completion after failed worker handoff | Current task owner through explicit report attestation, followed by Shephrd verification | Authority to record bounded immutable driver evidence for one exact current attempt and to request verification | Worker event authorship, semantic approval, file-existence completion, release without proof, or authority over a later attempt |
| Report acceptance lifecycle annotation and receipt | The exact trusted handler configured at immutable report acceptance | Its own external side effect and bounded non-authoritative presentation output | Report bytes, worker events, artifacts, task state, verification, landing, release, discard, or policy |

## Conflict resolution

1. Follow the user's requested scope and authorization; Shephrd has no mandatory development workflow.
2. Apply repository constraints to work inside that repository; surface material conflicts before action.
3. Use the skill for core safety and commands, loading detailed references only when needed.
4. Use current CLI help, code, schema, and tests for implemented mechanics.
5. Use the component guide as the canonical explanation and navigation surface for that mechanism.

A readiness projection, checkpoint, annotation, notification, artifact claim, attestation, verification result, lifecycle annotation, or external receipt carries only the authority assigned in its component guide. Driver-attested report recovery is intentionally exceptional: it does not rewrite or impersonate worker protocol history, and verification records `driver_report_recovery` provenance. None becomes general lifecycle authorization by being newer or more detailed.

## Guidance assembly

A full worker brief is assembled from distinct sources:

1. The registered repository context file points the worker to standing repository guidance. The optional `.shephrd/context.md` is independently discovered in the attempt worktree, alongside current memory-setting and role-aware curation guidance.
2. The task title, objective, acceptance criteria, and deliverable define the requested outcome.
3. Explicit verified report inputs provide immutable evidence without widening scope or becoming instructions.
4. A recovery brief adds checkpoint, workspace, audit, and latest task annotation context.
5. `worker send` supplies one current driver-approved follow-up to a waiting worker, with refreshed project-context and memory guidance.

The driver resolves material conflicts before spawn or send. Recovery context records what happened; it does not authorize a different outcome or prove external state.

## Test ownership

Guidance tests prove resource wiring and authority boundaries. Product-mechanics tests prove behavior consumed by the CLI, store, control layer, adapters, delivery code, and worker briefs. Guidance tests must not turn changeable driver prose into a build contract.

| Category | Test owners | Contract covered |
| --- | --- | --- |
| Neutral base guidance | `internal/control/prompt_contract_test.go`, `tests/pi/shephrd-wake.test.ts` | Basic task prompts without workflow mandates, bounded guidance size, on-demand references, lifecycle safety, and notification acknowledgement independent of further work |
| Portable skill command surface | `internal/cli/root_test.go` | Public command families and their mechanics reference in the skill command map |
| Project memory and context discovery | `internal/config/memory_test.go`, `internal/repository/context_test.go`, `internal/brief/context_test.go`, `internal/cli/context_e2e_test.go`, `internal/control/project_memory_test.go` | Default-on setting and opt-out, independent overview discovery, unchanged repository instructions, worktree-local context, recovery guidance, and current disable overriding a stored prompt |
| Worker brief authority and event shape | `internal/brief/brief_test.go`, `internal/control/recovery_test.go`, `internal/control/runtime_integration_test.go` | Context-file boundaries, no skill injection into worker briefs, terminal/checkpoint fields, and removal of driver-only environment inputs |
| Event and checkpoint mechanics | `internal/adapter/*_test.go`, `internal/model/checkpoint_test.go`, `internal/runner/*_test.go` | Strict event grammar, checkpoint validation, ordering, freshness, and terminal settlement |
| Artifact, landing, and release mechanics | `internal/delivery/*_test.go`, `internal/verify/*_test.go`, `internal/control/*delivery*_test.go`, `internal/control/release_authorization_test.go` | Artifact identity, landing proof, verification, release, discard authorization, and irreversible-action boundaries |
| Durable state and lifecycle mechanics | `internal/store/*_test.go`, `internal/control/*_test.go`, `internal/cli/*_test.go` | Task, attempt, workspace, notification, annotation, plan, and control transitions |

Guidance regressions check that ordinary tasks do not inherit mandatory workflow policy. The former review-tier, model-family, decomposition, parallel-wave, and run-to-boundary assertions are removed. Product tests continue to enforce event shape, ownership, artifact lineage, proof, and process-liveness mechanics.

## Why the split exists

Workflow preferences and executable mechanics have different owners. A one-line change should not inherit an orchestration procedure from the transport or task tracker. Repository instructions and explicitly requested workflows can add constraints without turning them into global defaults. The [repository context and workflow module convention](workflow-modules.md) provides ordinary Markdown entry points, memory topics, and optional playbooks. It adds lean overview discovery and default-on agent-maintained memory, not a recursive loader, background extraction service, or workflow enforcement system.
