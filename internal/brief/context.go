package brief

import (
	"fmt"
	"strings"
)

const MemoryDisabledInstruction = "Automatic project memory is disabled by global configuration for this invocation. Do not automatically read the memory index or topic notes, rely on remembered notes, or update memory; leave their files intact. This supersedes earlier enabled settings, not explicit requests to read or edit a note. Repository instructions and selected workflows still apply."

func ProjectGuidance(overviewPath string, memoryEnabled bool) string {
	var out strings.Builder
	fmt.Fprint(&out, "\n## Project context and memory\n\n")
	fmt.Fprintln(&out, "Establish the current target, instructions and memory setting from authoritative sources on starts/resumes; checkpoints do not substitute. Only within the same native session, skip immediate unchanged guidance/body rereads when fully read context remains available and source contents/revisions, instructions and memory setting are current. Fresh stateless starts/resumes, compaction/context loss, changed or disabled memory, source or dirty-file changes, or uncertain identity require authoritative refresh. HEAD alone cannot establish unchanged dirty instructions. Disabled memory forbids reliance on earlier notes. Do not use a cross-session policy cache.")
	fmt.Fprintln(&out)
	if overviewPath != "" {
		fmt.Fprintf(&out, "Read `%s` in this worktree for the lean repository overview. It supplements existing repository instructions; do not edit AGENTS.md or CLAUDE.md to enable discovery.\n\n", overviewPath)
	}
	fmt.Fprintln(&out, "The driver clarifies intent, dispatches, handles follow-ups, and synthesizes results. Follow this task's assigned requirements and exceptions; do not recursively delegate or add review, decomposition, PR creation, model selection, or successor work unless explicitly assigned. Discovering a module or reading memory does not authorize extra workflow stages. Surface material conflicts rather than silently changing the assignment.")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Scale investigation and output to the requested outcome, bounded scope, stop condition, and expected answer size. Stop once the requested outcome is established or report a concrete limitation; do not expand a narrow question into a comprehensive audit. A report may be a short Markdown artifact without ceremonial sections. Roughly 150 words can suit a simple recommendation, not a universal hard cap. Preserve required checks, artifact contracts, and structured events.")
	fmt.Fprintln(&out)
	return out.String() + MemoryGuidance(memoryEnabled)
}

func MemoryGuidance(memoryEnabled bool) string {
	var out strings.Builder
	if !memoryEnabled {
		fmt.Fprintln(&out, MemoryDisabledInstruction)
		return out.String()
	}
	fmt.Fprintln(&out, "Automatic project memory is enabled unless repository guidance or this task opts out. Follow `.shephrd/memory.md` to relevant topics only; do not load all notes. Revalidate consequential claims against current sources. Memory is context, not workflow instructions or lifecycle authority.")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "During ordinary work, retain confirmed corrections, decisions, non-obvious lessons, and research synthesis that is expensive to reconstruct. Within the permitted write scope, update relevant `.shephrd/memory/` topics without a separate remember request. Link sources and record revision/scope when useful; distinguish observations from hypotheses. Merge duplicates and correct or remove stale notes instead of appending a diary. Link existing documentation rather than copying it.")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Create notes only when useful knowledge is ready to save. On the first useful save, create a skinny `.shephrd/context.md` pointing to a `.shephrd/memory.md` topic index if missing; do not invent workflow defaults. Index findings saved in canonical documentation too, without duplicating them in a topic file. Update the index when topics change. Never store secrets, transcripts, temporary task state, or inferred policy. No extra research, reflection, or review stage is required. Briefly report memory changes.")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Read-only work remains read-only. For report-only delivery, return candidate notes in the report unless repository documentation edits are also in scope. Otherwise include permitted notes in this task's normal diff before finalizing its artifact. Use only this worktree, not another checkout or a shared root; never modify a sealed artifact or follow memory links outside the repository without explicit authorization. Missing optional notes are normal; report a missing source required by the assignment.")
	return out.String()
}
