# Repository context, memory, and workflow modules

Status: Markdown organization with project-context discovery and default-on agent-maintained memory. Working examples live in [this repository](../../../.shephrd/context.md). There is no recursive module loader, workflow engine, scheduler, background extractor, or new persistence layer.

## Organization

```text
<repository>/
  AGENTS.md
  .shephrd/
    context.md
    memory.md
    memory/
      <topic>.md
    modules/
      quick-change.md
      thorough-review.md
      repository-research.md
```

Shephrd recognizes `.shephrd/context.md` as an optional overview; the remaining files are a Markdown convention read and maintained by agents. Create files lazily when there is useful knowledge to save, not on every repository visit. Existing guidance, skills, and documentation can be linked in place rather than copied.

| Resource | Purpose | Read when |
| --- | --- | --- |
| `AGENTS.md` or `CLAUDE.md` | Existing repository constraints, unchanged by context discovery | Following repository guidance |
| `context.md` | Skinny repository overview, workflow preferences, and a link to `memory.md` | Establishing repository context |
| `memory.md` | Topic list with brief summaries and links | Looking for relevant repository knowledge |
| `memory/<topic>.md` | Detailed notes, lessons, decisions, and supporting references | That topic matters to the task |
| `modules/<name>.md` | Optional procedural playbook | Selected by the request or applicable repository guidance |

`context.md` should stay a few short paragraphs or bullets, not become the memory index itself. `memory.md` is a map, not an encyclopedia or mandatory reading list. Follow the chain **context -> memory index -> relevant topics** without loading the entire directory. Ordinary relative Markdown links are enough; no frontmatter or schema is required.

## Adopt it in a repository

1. Use the portable Shephrd skill for direct or delegated repository work. Before reading memory, the driver runs `shephrd repo context <path> --json` for the target directory, not merely its own working directory.
2. Read the returned overview when present. It describes the repository, any explicit workflow preferences, and where to find `memory.md`. Existing AGENTS.md/CLAUDE.md instructions remain untouched and still apply.
3. Follow the topic index only when relevant. Link existing architecture, command, or decision documentation before creating new notes.
4. Reference only the workflow modules wanted for that repository or task. Without a selection, retain the base delegation preference without adding workflow stages.

The context command resolves the Git repository or worktree root and returns `repository_path`, optional repository-relative `overview_path`, and the configured `memory.enabled` value. It reads no note contents, creates no repository files, and requires neither registration nor the task database. It uses ordinary configuration loading, which creates an absent user config. A directory or an overview link escaping the repository is rejected rather than followed.

Initial worker briefs, retries, relaunches, and follow-ups discover the overview in the actual attempt worktree, not a dirty registered root. They supply current memory guidance and preserve the distinction between worker requirements and driver coordination. Links are not recursively expanded. Missing optional context is normal; a missing source required by the assignment must be reported. Automatic discovery in unrelated sessions without the Shephrd skill is not provided.

The supplied [context](../../../.shephrd/context.md), [quick-change module](../../../.shephrd/modules/quick-change.md), [thorough-review module](../../../.shephrd/modules/thorough-review.md), and [repository-research module](../../../.shephrd/modules/repository-research.md) are usable examples. Copy and adapt only the documents needed, preserving valid links. Referencing a shared module or an existing skill directly is equally valid; local copies are not required.

## Repository workflow preferences

Preferences can be ordinary prose in `context.md`, with links to the chosen documents. They are not fixed profiles, configuration modes, or a new enforcement layer.

For example, dotfiles guidance could say:

> Prefer the quick-change module for ordinary edits. Normally assign one worker a bounded change, diff inspection, and required checks, with a concise handoff and no independent review cycle. Explicitly requested direct work remains direct. Reconsider the approach for secrets or destructive behavior; the task request can choose another workflow.

Glide guidance could instead say:

> Prefer the thorough-review module for implementation work, including independent review and targeted follow-up on fixes. Use the quick-change module for documentation-only edits or when explicitly requested, subject to repository constraints.

These are examples, not changes to either repository. Shephrd itself selects no module by default. Repositories can write their own workflow document instead of using either example.

Keep preferences distinct from mandatory repository constraints. The user's explicit task instructions take precedence over workflow preferences; material conflicts with repository requirements must be surfaced before action. Wording like "prefer" or "normally" leaves room for judgment. Neither that wording nor "must" creates executable enforcement.

## Module selection and scope

Selection comes from a user request or applicable repository guidance, not from the mere presence of a file, a code deliverable, or a remembered past workflow. It can name one task, one plan, or work in one repository. It does not silently extend to unrelated tasks, sibling repositories, successors, or the whole driver session.

For delegated work, record the selected document path, version or content identity, scope, and resolved requirements in existing task text. A repository commit plus document path is sufficient identity; an unversioned document can use a content digest. Give the worker accessible instructions rather than only a path on the driver's machine. Do not add a module identifier field, new task state, or configuration merely to carry this text.

Record task-specific exceptions with the selection so a retry or relaunch does not silently pick up changed preferences. Annotations can preserve recovery context, but do not activate a module or authorize new work. If selected modules materially conflict, resolve that conflict before action instead of relying on load order.

A module should explain its purpose, approach, required inputs or evidence, and stop conditions. Modules describe ordinary existing commands and agent behavior; they do not automate transitions. Use the [review mechanics](../components/review-feedback.md) when an independent review workflow is requested.

## Memory topics

Memory is a repository notebook. Good entries explain a non-obvious hazard, a recurring correction, a decision's rationale, or where to find important information. They can also synthesize system boundaries, data flows, or failure behavior across multiple components. Preserve knowledge that is expensive to reconstruct, even when the underlying facts exist in code; avoid duplicating readily available information. Short topic summaries belong in `memory.md`; details belong in a topic file or existing documentation.

Keep source references with claims where useful: a repository path, commit, issue, user decision, or external document. Record a verification date or revision for facts likely to change, and label uncertainty. These can be ordinary bullets, not required metadata fields. A note observed on one branch or machine is not automatically true on another.

Do not duplicate code, full command catalogs, or canonical architecture documentation. Do not store credentials, raw logs, worker transcripts, temporary paths, or active task state. Tasks, attempts, checkpoints, annotations, and verified artifacts retain their existing roles. A useful lesson may link to evidence without copying all of it into memory.

Memory does not select modules, rewrite repository guidance, or grant lifecycle authority. A remembered preference can explain prior choices, but a current request or applicable guidance must establish its use. Revalidate consequential facts against current sources rather than treating notes as proof.

## Building knowledge through research

Research is an opportunity to build repository knowledge, not just answer the immediate question. Within authorized curation and the task's permitted write scope, distill durable findings into relevant topics without requiring a separate request to remember each finding. Respect any memory opt-out. A focused subsystem investigation should enrich that topic, not trigger an exhaustive repository survey.

The optional [repository-research module](../../../.shephrd/modules/repository-research.md) describes a bounded research pass: reuse existing documentation, map the systems in scope, trace implementation and tests, and retain concise source-backed findings. Record the inspected revision, coverage, and gaps. Distinguish observed behavior, documented intent, and hypotheses; do not treat report acceptance as semantic validation.

Keep the requested answer or report separate from the reusable topic summary. Explicitly read-only work must not edit repository memory. A report-only deliverable carries candidate additions in the report unless repository documentation edits are also in scope. Do not create follow-on tasks or change artifact contracts just to populate memory. The memory guidance applies during ordinary work; this optional playbook adds research structure, not a repository indexer or permission to broaden the task.

## Memory setting

Memory is enabled by default. Users can opt out in the existing Shephrd configuration:

```toml
[memory]
enabled = false
```

A global disable stops automatic recall, reliance on remembered notes, and writing; existing files remain untouched. Repository constraints and explicitly selected workflows still apply. Explicit requests to read or edit a note remain ordinary document operations. Repository guidance or task instructions can further opt out, but cannot override a global disable. Scoped instructions are interpreted by the agent, not parsed into a configuration hierarchy.

Drivers check the configured value through the context command before repository work. Worker prompts refresh it on spawn, retry, relaunch, and follow-up; the runner also reapplies a current global disable before invoking the harness, so an older enabled brief cannot override it. Changes are not pushed into already-running turns. Put task-specific opt-outs in task text and preserve later corrections through explicit follow-ups and recovery context.

This is an agent-behavior setting, not a filesystem sandbox or a switch for a harness's separate native auto-memory feature. Disabling it cannot erase previously read text from a session. Use the harness's own settings for its native memory.

## Writing and maintaining memory

With memory enabled, agents naturally maintain useful knowledge within the task's permitted write scope without per-note approval or a separate remember request. Capture confirmed corrections, decisions, validated discoveries, and scoped research synthesis at natural points during ordinary work, before handoff or artifact finalization. If nothing useful was learned, write nothing. Do not add a reflection worker or an extra review stage merely to populate memory.

On the first useful save, create a skinny overview and index if missing. Add a topic only if the finding does not belong in existing documentation; otherwise index that document without duplicating it. Do not invent workflow preferences. Update the index as topics change and briefly report meaningful edits. Detailed [project memory guidance](../../../.agents/skills/shephrd/references/project-memory.md) covers the driver behavior; workers receive self-contained curation and scope guidance in their prompts. No background extractor, write-approval queue, or maintenance service is involved.

When updating a topic, merge overlapping notes and correct or remove stale entries instead of appending an unlimited diary. Keep the index useful and small; do not silently truncate it. No byte limit is enforced by this convention.

Repository-local committed notes travel through normal Git history. They do not automatically synchronize across branches, worktrees, clones, or machines. Workers read the version available in their attempt, and memory edits in an attempt remain ordinary unlanded repository changes. Dirty root files are never copied implicitly into a worker. Concurrent edits use ordinary Git conflict resolution, not a shared mutable memory file.

Keep personal or sensitive notes outside the shared repository unless deliberately approved for sharing. This convention does not implement a private per-repository store or cross-checkout identity mapping. Confirm the intended repository before reusing externally stored notes; matching names alone do not establish identity.

## Safety and neutral base

The base skill still covers task execution, identity, authorization, evidence, notifications, and safe lifecycle transitions. Worker briefs supply scope, artifact and event contracts, and recovery facts. The Pi watcher delivers and acknowledges notifications without prescribing review or further work.

Context, memory, and modules cannot grant ownership, authorize landing or discard, change accepted artifact identity, or promote readiness and review evidence into authorization. Lifecycle safety remains non-overridable. Reports, notifications, and annotation prose cannot activate modules. Conflicts or uncertainty keep recoverable work held.

Without a selected workflow, drivers still delegate substantive repository work by default, including small tasks. Complexity changes investigation depth and answer size, not the delegation preference. No independent review, decomposition, PR requirement, model-family choice, successor work, or recursive delegation is added automatically. The [authority map](authority.md) describes the existing boundaries.

## Future automation

Use the lightweight discovery and agent-driven Markdown curation first. Only add module selection assistance, semantic retrieval, external storage, or background memory processing when actual use exposes a need. Any future integration needs coverage for provenance, explicit selection, conflicts, scope containment, worktree identity, concurrent writes, and retry/relaunch continuity. It must also prove that absent module selection preserves the base delegation preference and adds no workflow stages.
