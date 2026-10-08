package control

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"shephrd/internal/brief"
	"shephrd/internal/model"
)

func TestBasicTaskGuidanceDelegatesWithoutAddingWorkflow(t *testing.T) {
	root := driverResourceRoot(t)
	skill := string(readDriverResource(t, root, ".agents/skills/shephrd/SKILL.md"))
	guidance := string(readDriverResource(t, root, ".agents/skills/shephrd/references/driver-policy.md"))
	input := brief.Input{
		Task: model.Task{
			ID: "task_dotfile", Title: "Update one dotfile line", Objective: "Change one alias in .zshrc",
			AcceptanceCriteria: "Only the requested alias changes", Deliverable: "code",
		},
		Attempt:       model.Attempt{ID: "attempt_dotfile", Branch: "shephrd/task_dotfile", Harness: "pi", RunGeneration: 1},
		Mode:          "initial assignment",
		MemoryEnabled: true,
	}
	for _, mode := range []string{"initial assignment", "clean retry", "same-worktree relaunch"} {
		t.Run(mode, func(t *testing.T) {
			input.Mode = mode
			rendered := brief.Render(input)
			assembled := strings.Join([]string{skill, guidance, string(rendered.Content), strings.Join(rendered.Warnings, "\n")}, "\n")
			for _, forbidden := range []string{
				`(?i)delegate (every|coding|all)`,
				`(?i)driver does not perform project`,
				`(?i)(create|spawn|requires?|remain) (a |an )?(required |strong |focused )?(independent|review)`,
				`(?i)(must|required|requires?|explicit delta) re-review`,
				`(?i)(create|open) (a |the )?(github )?(pr|pull request)\b`,
				`(?i)(use luna|use sol|fable 5|gpt.?5\.6|every claude-family)`,
				`(?i)(after decomposition|split when|parallel-wave scan|one task per repository)`,
				`(?i)(dispatch and spawn.*successor|run-to-boundary|scout task)`,
				`(?i)(load.*completely|never run.*complete policy|loads-driver-policy)`,
			} {
				if regexp.MustCompile(forbidden).MatchString(assembled) {
					t.Errorf("basic task inherited workflow policy matching %q", forbidden)
				}
			}
			for _, required := range []string{
				"Only the requested alias changes", "branch:shephrd/task_dotfile", "attempt_dotfile",
				"structured checkpoint", "exactly one terminal event", "Do not reset, clean, switch branches",
			} {
				if !strings.Contains(string(rendered.Content), required) {
					t.Errorf("brief lost protocol or task content %q", required)
				}
			}
		})
	}
	if len(skill) > 7500 || len(guidance) > 3500 {
		t.Fatalf("base guidance grew beyond its bounded core: skill=%d guidance=%d bytes", len(skill), len(guidance))
	}
	for _, required := range []string{
		"including small tasks", "Complexity justifies more depth, not delegation itself", "Load only the reference needed",
		"answers already supported by available context", "necessary coordination and lifecycle operations",
		"explicitly requested direct work", "one worker with a small assignment",
		"exact outcome, bounded scope, stop condition, and expected answer size",
		"Bound investigation as well as prose", "not a universal hard cap",
		"without ceremonial sections or a comprehensive audit", "Required checks and artifact contracts still apply",
		"remain available asynchronously", "Do not wait, poll, or duplicate worker work",
		"successor work, and recursive delegation are not automatic",
		"current ownership", "process state", "Dirty root files are not transferred",
		"accepted artifact", "explicit authorized merge", "explicit discard authorization",
		"Do not sleep, poll status, manually drain notifications", "shephrd wake ack --claim-token",
	} {
		if !strings.Contains(skill, required) {
			t.Errorf("skill lost core boundary %q", required)
		}
	}
}

func TestDriverGuidanceBoundsSmallAssignments(t *testing.T) {
	root := driverResourceRoot(t)
	guidance := string(readDriverResource(t, root, ".agents/skills/shephrd/references/driver-policy.md"))
	for _, required := range []string{
		"even for small tasks", "Use one worker for a small repository question",
		"exact outcome, bounded scope, stop condition, and expected answer size",
		"stop when the tradeoff is established", "roughly 150 words", "not a universal word cap",
		"Bound investigation too", "retain artifact contracts and required checks",
		"remain available asynchronously", "Do not wait, poll, or duplicate",
		"recursive delegation require explicit user or applicable repository instructions",
	} {
		if !strings.Contains(guidance, required) {
			t.Errorf("driver guidance lost %q", required)
		}
	}
	for _, path := range []string{
		"README.md", "docs/README.md", "docs/reference/workflow-modules.md",
		".shephrd/modules/quick-change.md", ".shephrd/modules/repository-research.md",
	} {
		body := string(readDriverResource(t, root, path))
		for _, forbidden := range []string{
			"Implement directly unless", "Direct work is allowed; delegation",
			"no added delegation", "does not require delegation",
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s contradicts delegation preference: %q", path, forbidden)
			}
		}
	}
}

func TestGuidanceResourcesPreserveAuthoritySplit(t *testing.T) {
	root := driverResourceRoot(t)
	agents := string(readDriverResource(t, root, "AGENTS.md"))
	if !strings.Contains(agents, "<!-- shephrd-authority: worker-guidance -->") || strings.Contains(agents, "references/driver-policy.md") {
		t.Fatal("repository guidance must remain worker-scoped without a mandatory driver import")
	}
	skill := string(readDriverResource(t, root, ".agents/skills/shephrd/SKILL.md"))
	for _, name := range []string{"driver-policy.md", "recovery.md", "delivery.md", "plans.md", "project-memory.md"} {
		if !strings.Contains(skill, "references/"+name) {
			t.Errorf("skill lacks on-demand reference %s", name)
		}
		readDriverResource(t, root, ".agents/skills/shephrd/references/"+name)
	}
	for _, path := range []string{"README.md", "docs/reference/authority.md"} {
		text := string(readDriverResource(t, root, path))
		if !strings.Contains(text, ".agents/skills/shephrd/SKILL.md") {
			t.Errorf("%s lacks skill link", path)
		}
	}
}

func driverResourceRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func readDriverResource(t *testing.T, root, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	return body
}
