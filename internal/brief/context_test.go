package brief

import (
	"strings"
	"testing"
)

func TestProjectGuidanceSeparatesDiscoveryWorkflowAndMemory(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		input := goldenInput("task", "attempt", "pi", "", "initial assignment", "assigned")
		input.ProjectContextPath = ".shephrd/context.md"
		input.MemoryEnabled = enabled
		input.Task.Objective = "Read-only research; do not use project memory for this task."
		input.Task.AcceptanceCriteria = "Use the selected repository-research module only; do not delegate."
		for _, mode := range []string{"initial assignment", "clean retry", "same-worktree relaunch"} {
			input.Mode = mode
			body := string(Render(input).Content)
			for _, want := range []string{input.Task.Objective, input.Task.AcceptanceCriteria, input.ProjectContextPath, "The driver clarifies intent, dispatches, handles follow-ups, and synthesizes results"} {
				if !strings.Contains(body, want) {
					t.Fatalf("%s enabled=%t lost %q", mode, enabled, want)
				}
			}
			if enabled {
				for _, want := range []string{"enabled unless repository guidance or this task opts out", "Read-only work remains read-only", "before finalizing its artifact", "Missing optional notes are normal", "Index findings saved in canonical documentation too"} {
					if !strings.Contains(body, want) {
						t.Fatalf("enabled guidance lost %q", want)
					}
				}
			} else if !strings.Contains(body, MemoryDisabledInstruction) || strings.Contains(body, ".shephrd/memory.md") {
				t.Fatalf("disabled guidance included automatic recall: %s", body)
			}
		}
	}
}

func TestProportionateWorkerGuidanceSurvivesMemoryOptOutAndFollowUps(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		input := goldenInput("task", "attempt", "pi", "", "initial assignment", "assigned")
		input.MemoryEnabled = enabled
		input.Task.Objective = "Recommend one approach; inspect only the two named implementations."
		input.Task.AcceptanceCriteria = "Stop when the tradeoff is established; answer in roughly 150 words."
		bodies := []string{ProjectGuidance("", enabled)}
		for _, mode := range []string{"initial assignment", "clean retry", "same-worktree relaunch"} {
			input.Mode = mode
			body := string(Render(input).Content)
			if !strings.Contains(body, input.Task.Objective) || !strings.Contains(body, input.Task.AcceptanceCriteria) {
				t.Fatalf("%s lost bounded assignment", mode)
			}
			bodies = append(bodies, body)
		}
		for _, body := range bodies {
			for _, want := range []string{
				"bounded scope, stop condition, and expected answer size",
				"Stop once the requested outcome is established or report a concrete limitation",
				"do not expand a narrow question into a comprehensive audit",
				"short Markdown artifact without ceremonial sections",
				"not a universal hard cap", "Preserve required checks, artifact contracts, and structured events",
				"do not recursively delegate", "unless explicitly assigned",
				"current target, instructions and memory setting from authoritative sources on starts/resumes",
				"checkpoints do not substitute", "Only within the same native session",
				"skip immediate unchanged guidance/body rereads when fully read context remains available",
				"source contents/revisions, instructions and memory setting are current",
				"Fresh stateless starts/resumes", "compaction/context loss", "changed or disabled memory",
				"source or dirty-file changes", "uncertain identity require authoritative refresh",
				"HEAD alone cannot establish unchanged dirty instructions",
				"Disabled memory forbids reliance on earlier notes", "Do not use a cross-session policy cache",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("enabled=%t lost worker guidance %q", enabled, want)
				}
			}
		}
	}
}
