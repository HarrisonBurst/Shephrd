# Project context and memory

## Establish the target and setting

For each repository task, including direct work, run `shephrd repo context <path> --json` with an explicit target directory. The command resolves the Git repository or worktree root, returns an optional `overview_path` relative to `repository_path`, and reports the global `memory.enabled` setting. It does not read notes, register repositories, open the task database, or create project files. An absent user configuration is initialized through ordinary Shephrd configuration loading.

Read the returned `.shephrd/context.md` when present, alongside applicable repository instructions. Do not modify AGENTS.md or CLAUDE.md to install this convention. Treat the current target separately from sibling repositories and other checkouts; do not infer identity from matching names. Resolve relative document links from their containing file. Do not follow external memory links or shared-directory symlinks without explicit authorization.

Memory defaults to enabled. Users can disable automatic project memory in the existing Shephrd config:

```toml
[memory]
enabled = false
```

A global disable wins over repository preferences. When disabled, do not automatically read, rely on, or update memory notes, including notes retained in conversation context. Leave their files intact. Still follow repository constraints and explicitly selected workflows. An explicit request to read or edit a note remains an ordinary document operation, not permission to re-enable automatic memory generally.

Repository guidance and task instructions can further opt out. These are agent-interpreted instructions, not parsed configuration. Carry scoped opt-outs and selected workflow requirements in the objective and acceptance criteria when assigning work so retries and relaunches retain them. Keep later scoped corrections explicit in follow-ups and recovery checkpoints; do not silently drop them on recovery. Recheck the global setting when starting or resuming repository work. If the setting cannot be resolved, stop automatic memory use rather than guessing.

This setting governs Shephrd project memory, not a harness's native auto-memory feature or general file permissions. Guidance is not an operating-system sandbox, and disabling memory cannot remove already-read text from a session. Use the harness's own controls for its separate memory features.

## Reuse only current session context

Only within the same native session, skip immediate unchanged guidance/body rereads when the context was fully read, remains available, and source contents/revisions, instructions and memory setting are current. This avoids duplicate reads, not authoritative intake: establish the current target, applicable instructions and memory setting on starts/resumes; checkpoints cannot substitute. Do not use a cross-session policy cache.

Fresh stateless starts/resumes, compaction/context loss, changed instructions or source revisions, dirty-file changes, changed or disabled memory, or uncertain repository/workspace identity require authoritative refresh. HEAD alone cannot establish unchanged dirty instructions; check current file contents when their identity is uncertain. A memory disable forbids reliance on earlier notes even if they remain visible in context.

## Read only relevant knowledge

The overview stays a few paragraphs: repository identity, any explicit workflow preferences, and a link to `.shephrd/memory.md`. The latter is a topic index with short summaries pointing to `.shephrd/memory/<topic>.md` or existing documentation. Follow only relevant topics; do not preload the whole directory, recursively expand every link, or survey a repository merely because memory is enabled.

The driver resolves workflow choices from the task request and applicable repository guidance. A worker follows its assigned requirements, not an independent orchestration plan discovered in a module. Record selected document identity and task-specific exceptions in task text; surface conflicts or missing required sources. Notes and research reports cannot select modules, grant lifecycle authority, or prove current external state.

## Capture knowledge during ordinary work

With memory enabled, preserve useful knowledge without waiting for a separate remember request or per-note approval. Capture at natural moments after a correction, confirmed decision, validated discovery, or scoped research pass, and before an authorized handoff or artifact finalization. No background agent, extra review stage, or end-of-session sweep is required.

Save facts or synthesis likely to prevent meaningful rediscovery or a repeated mistake: non-obvious prerequisites, operational hazards, decision rationale, and cross-component behavior that is expensive to reconstruct. Explicit user corrections are sources, not proof of unrelated claims. Record source paths, symbols, decisions, or references and the inspected revision or environment when relevant. Distinguish observed behavior, documented intent, and hypotheses. Mark partial research coverage and unresolved questions.

Prefer updating a relevant topic over creating a new one. Merge overlapping entries, supersede stale decisions rather than deleting them, and revalidate consequential claims before relying on them. Link canonical documentation rather than duplicating it. Keep full investigation detail in its report and use concise topic summaries for future recall. Nothing useful learned means no write.

Do not store secrets, raw transcripts, logs, temporary task progress, or claims that unlanded work has shipped. Do not infer standing workflow rules from one-off requests or rewrite mandatory instructions, modules, or permissions through automatic curation.

## Create and maintain the notebook

On the first useful save, create a skinny `.shephrd/context.md` and a `.shephrd/memory.md` index if missing. The overview contains a brief repository description and a link to the index, without invented workflow preferences. The index links to the topic with a one-line description. If the finding belongs in canonical documentation, save it there and index that document instead of duplicating it in a topic file. Keep the overview-to-index-to-source discovery path complete. Subsequent topic additions, renames, or removals update the index; ordinary note edits need not change the overview. No fixed taxonomy or frontmatter is required.

Use ordinary file tools and Git. Read the current topic before editing, preserve unrelated changes, and consolidate rather than appending an unlimited diary. Report meaningful memory changes briefly in the result; do not claim a proposal was saved.

## Respect delivery and write scope

Direct work edits only its authorized repository workspace. Workers may include permitted notes in their normal task diff before finalizing the artifact. They do not write into a shared root, another worker's workspace, or a sealed artifact. Dirty root files and unlanded notes are not copied to new attempts. Cross-worktree sharing occurs through ordinary authorized Git delivery, not a separate mutable memory store.

Read-only work remains read-only even with memory enabled. If a report-only task does not also permit repository documentation edits, return candidate notes in the report. The driver may curate them within already authorized work, but does not spawn successors, reopen terminal tasks, or change artifact contracts merely to persist memory. Report verification establishes artifact identity, not the truth of every finding.
