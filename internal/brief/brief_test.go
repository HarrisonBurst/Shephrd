package brief

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
)

func TestRenderGoldenVariants(t *testing.T) {
	tests := []struct {
		name  string
		input Input
	}{
		{name: "claude-code-initial", input: goldenInput("task_claude", "attempt_claude", "claude-code", "claude-model", "initial assignment", "Driver assigned this task for the first run.")},
		{name: "pi-report-input", input: reportInputGolden()},
		{name: "codex-plan-report-inputs", input: planReportInputsGolden()},
		{name: "pi-clean-retry", input: retryGolden()},
		{name: "codex-same-worktree-relaunch", input: relaunchGolden()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Render(test.input)
			want, err := os.ReadFile(filepath.Join("testdata", test.name+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Content, want) {
				t.Fatalf("rendered brief differs from golden:\n%s", diffText(string(want), string(got.Content)))
			}
			again := Render(test.input)
			if !bytes.Equal(got.Content, again.Content) || len(got.Warnings) != len(again.Warnings) {
				t.Fatal("rendering is not deterministic")
			}
		})
	}
}

func TestRenderIncludesStrictTerminalExamplesAndAllowedFields(t *testing.T) {
	body := string(Render(goldenInput("task", "attempt", "pi", "model", "initial assignment", "assigned")).Content)
	for _, required := range []string{
		`{"type":"question","payload":"Full question, options, consequences, and recommendation"}`,
		`{"type":"done","payload":"Completed work and verification summary","artifact":"branch:exact-attempt-branch"}`,
		`{"type":"blocked","payload":"What is blocked and the exact recovery needed"}`,
		`{"type":"failed","payload":"What failed and why"}`,
		"Terminal fields are exactly `type`, `payload`, and optional `artifact`.",
		"Nested `question` or `options` fields and every unknown field are invalid",
	} {
		if !bytes.Contains([]byte(body), []byte(required)) {
			t.Fatalf("brief missing %q", required)
		}
	}
}

func TestCompactCheckpointGuidanceAndExamples(t *testing.T) {
	body := string(Render(goldenInput("task", "attempt", "pi", "model", "initial assignment", "assigned")).Content)
	protocol, err := os.ReadFile(filepath.Join("..", "..", "docs", "components", "worker-protocol.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, guidance, found := strings.Cut(body, "Prefer a one-sentence summary")
	if !found {
		t.Fatal("compact checkpoint guidance missing")
	}
	guidance, _, _ = strings.Cut(guidance, "\n")
	if !strings.Contains(string(protocol), "Prefer a one-sentence summary"+guidance) {
		t.Fatal("protocol and brief compact checkpoint guidance differ")
	}
	for _, text := range []string{body, string(protocol)} {
		for _, required := range []string{
			"one-sentence summary and terse factual entries, normally around 1 KB",
			"soft target, not a limit", "complete recovery snapshot, never a delta",
			"Carry forward unresolved blockers and decisions, exact checks/results, artifact/evidence identities and recovery requirements",
			"exceed the target rather than omit necessary evidence, within existing schema bounds",
			"Keep every schema field, typed objects and empty arrays",
			"Readiness and checks do not authorize lifecycle actions",
		} {
			if !strings.Contains(text, required) {
				t.Errorf("compact checkpoint guidance lost %q", required)
			}
		}
		foundExample := false
		for _, line := range strings.Split(text, "\n") {
			line = strings.Trim(line, "`")
			if !strings.HasPrefix(line, `<shephrd-event>{"type":"checkpoint"`) {
				continue
			}
			foundExample = true
			event, found, err := adapter.ParseEvent(line)
			if err != nil || !found || event.Checkpoint == nil {
				t.Fatalf("invalid checkpoint example: found=%t err=%v", found, err)
			}
		}
		if !foundExample {
			t.Fatal("checkpoint example missing")
		}
	}
}

func TestRenderRetainsCompleteCheckpointBeyondSoftTarget(t *testing.T) {
	input := goldenInput("task", "attempt", "pi", "model", "same-worktree relaunch", "assigned")
	input.Checkpoint = &model.AttemptCheckpoint{
		SchemaVersion: 1, Producer: "worker", Revision: 7, SourceCursor: 21,
		SourceAttemptID: "attempt_source", SourceRevision: 3,
		Summary:      "Recovery held pending exact workspace checks.",
		Completed:    []string{"Scoped inspection complete"},
		NextSteps:    []string{"Inspect exact attempt before authorized recovery"},
		Decisions:    []model.Decision{{Decision: "Hold workspace", Reason: "Identity uncertain"}},
		ChangedPaths: []string{"docs/fixture.md"},
		Checks:       []model.Check{{Command: "fixture inspect", Result: strings.Repeat("fixture evidence; ", 50)}},
		Blockers:     []string{strings.Repeat("Endpoint uncertain; ", 40)},
	}
	body := string(Render(input).Content)
	for _, required := range []string{
		input.Checkpoint.Summary, input.Checkpoint.Completed[0], input.Checkpoint.NextSteps[0],
		input.Checkpoint.Decisions[0].Decision, input.Checkpoint.Decisions[0].Reason,
		input.Checkpoint.ChangedPaths[0], input.Checkpoint.Checks[0].Command, input.Checkpoint.Checks[0].Result,
		input.Checkpoint.Blockers[0], "- Producer: worker", "- Revision: 7", "- Source cursor: 21",
		"- Provenance: attempt attempt_source revision 3", "- HEAD: head-commit", "- Dirty: true",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("recovery brief lost %q", required)
		}
	}
}

func TestRenderListsTargetContextWithoutInliningOrInjectingSkill(t *testing.T) {
	root := t.TempDir()
	contextPath := filepath.Join(root, "AGENTS.md")
	const sentinel = "TARGET_CONTEXT_CONTENT_MUST_NOT_BE_INLINED"
	if err := os.WriteFile(contextPath, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	input := goldenInput("task", "attempt", "pi", "model", "initial assignment", "assigned")
	input.Repo.Path = root
	input.Repo.ContextFile = contextPath
	body := string(Render(input).Content)
	for _, required := range []string{"## Context files", "- AGENTS.md"} {
		if !strings.Contains(body, required) {
			t.Fatalf("brief missing %q", required)
		}
	}
	for _, forbidden := range []string{sentinel, ".agents/skills/shephrd/SKILL.md", "## Portable safety baseline"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("brief unexpectedly contains %q", forbidden)
		}
	}
}

func TestMissingContextWarningDoesNotCreateFollowOnWork(t *testing.T) {
	input := goldenInput("task", "attempt", "pi", "", "initial assignment", "assigned")
	input.Repo.ContextFile = ""
	result := Render(input)
	if len(result.Warnings) != 1 || result.Warnings[0] != "repo has no AGENTS.md or CLAUDE.md context file" {
		t.Fatalf("missing context should be informational: %v", result.Warnings)
	}
}

func goldenInput(taskID, attemptID, harness, modelName, mode, reason string) Input {
	return Input{
		Task:           model.Task{Title: "Build worker brief", ID: taskID, Objective: "Build the worker brief", AcceptanceCriteria: "checks pass", Deliverable: "code"},
		Repo:           model.Repo{ID: "repo_demo", Name: "demo", Path: "/repo", ContextFile: "/repo/AGENTS.md"},
		Attempt:        model.Attempt{ID: attemptID, Number: 1, Harness: harness, Model: modelName, RunGeneration: 1, Branch: "shephrd/" + taskID, WorktreePath: "/work/" + taskID, WorkspaceBackend: model.WorkspaceBackendNative, BaseCommit: "base-commit"},
		ReportPath:     "/data/" + taskID + "/report.md",
		Mode:           mode,
		Reason:         reason,
		WorkspaceFacts: model.WorkspaceFacts{HeadCommit: "head-commit", Dirty: true},
		Messages:       []model.Message{{AttemptID: attemptID, Direction: "worker-to-driver", Type: "checkpoint", Payload: "checkpoint payload"}},
		ReadmePresent:  true,
		MemoryEnabled:  true,
	}
}

func reportInputGolden() Input {
	input := goldenInput("task_report", "attempt_report", "pi", "pi-model", "initial assignment", "Driver assigned this task for the first run.")
	input.Reports = []Report{{Input: model.ReportInput{
		Position: 1, ArtifactID: "artifact_report", ProducerRepoID: "repo_source", ProducerRepoName: "source", ProducerTaskID: "task_source", ProducerAttemptID: "attempt_source", DoneMessageID: 42,
		OriginalRef: "report:/data/source/report.md", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", SizeBytes: 18,
		VerifiedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), AttachedByDriverID: "driver:test", AttachedAt: time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC),
	}, Body: []byte("Verified report body")}}
	return input
}

func planReportInputsGolden() Input {
	input := goldenInput("task_plan", "attempt_plan", "codex", "", "initial assignment", "Driver assigned this task for the first run.")
	input.Task = model.Task{Title: "Implement release", ID: "task_plan", Objective: "Implement release from the selected plan report inputs", AcceptanceCriteria: "checks pass", Deliverable: "code"}
	input.Reports = []Report{{Input: model.ReportInput{Position: 1, ArtifactID: "artifact_second", ProducerRepoID: "repo_second", ProducerRepoName: "second", ProducerTaskID: "task_second", ProducerAttemptID: "attempt_second", DoneMessageID: 2, OriginalRef: "report:/data/second.md", SHA256: "2222222222222222222222222222222222222222222222222222222222222222", SizeBytes: 2, VerifiedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), AttachedByDriverID: "driver:plan", AttachedAt: time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC)}, Body: []byte("two\n")}, {Input: model.ReportInput{Position: 2, ArtifactID: "artifact_first", ProducerRepoID: "repo_first", ProducerRepoName: "first", ProducerTaskID: "task_first", ProducerAttemptID: "attempt_first", DoneMessageID: 1, OriginalRef: "report:/data/first.md", SHA256: "1111111111111111111111111111111111111111111111111111111111111111", SizeBytes: 1, VerifiedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), AttachedByDriverID: "driver:plan", AttachedAt: time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC)}, Body: []byte("one")}}
	return input
}

func retryGolden() Input {
	input := goldenInput("task_retry", "attempt_retry", "pi", "retry-model", "clean retry", "Driver intentionally selected a new attempt and clean worktree.")
	input.Attempt.Number = 2
	input.Attempt.ResumeSourceAttemptID = "attempt_source"
	input.Attempt.ResumeSourceRevision = 4
	input.Checkpoint = &model.AttemptCheckpoint{AttemptID: input.Attempt.ID, Revision: 1, Producer: "system", SourceCursor: 0, Summary: "retry assigned", Completed: []string{"reset"}, NextSteps: []string{"continue"}}
	input.SourceWorkerCheckpoint = false
	return input
}

func relaunchGolden() Input {
	input := goldenInput("task_relaunch", "attempt_relaunch", "codex", "codex-model", "same-worktree relaunch", "Driver requested a fresh session after the prior runner stopped.")
	input.Attempt.ResumeSourceAttemptID = ""
	input.Checkpoint = &model.AttemptCheckpoint{AttemptID: input.Attempt.ID, Revision: 3, Producer: "system", SourceCursor: 2, Summary: "relaunch assigned", Completed: []string{"hold"}, NextSteps: []string{"recover"}}
	return input
}

func diffText(want, got string) string {
	for index := 0; index < len(want) && index < len(got); index++ {
		if want[index] != got[index] {
			return fmt.Sprintf("first difference at byte %d", index)
		}
	}
	return "length differs"
}
