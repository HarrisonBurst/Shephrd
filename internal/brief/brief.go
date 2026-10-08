package brief

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"shephrd/internal/model"
)

type Report struct {
	Input model.ReportInput
	Body  []byte
}

type Input struct {
	Task                   model.Task
	Repo                   model.Repo
	Attempt                model.Attempt
	ReportPath             string
	Mode                   string
	Reason                 string
	Reports                []Report
	Checkpoint             *model.AttemptCheckpoint
	LatestAnnotation       *model.Annotation
	SourceWorkerCheckpoint bool
	WorkspaceFacts         model.WorkspaceFacts
	Messages               []model.Message
	ReadmePresent          bool
	ProjectContextPath     string
	MemoryEnabled          bool
}

type Result struct {
	Content  []byte
	Warnings []string
}

func ReportDestination(path string) string {
	return fmt.Sprintf("Write the investigation report to %s and use artifact `report:%s`.\nPreserve earlier reports. This destination belongs only to this run; do not overwrite a prior report or submit an earlier run's artifact.\n\n", path, path)
}

func Render(input Input) Result {
	var warnings []string
	var out strings.Builder
	task := input.Task
	attempt := input.Attempt
	fmt.Fprintf(&out, "# Task %s: %s\n\n## Objective\n\n%s\n\n", task.ID, task.Title, task.Objective)
	criteria := task.AcceptanceCriteria
	if criteria == "" {
		criteria = "Complete the objective and run the repository's appropriate checks."
	}
	fmt.Fprintf(&out, "## Acceptance criteria\n\n%s\n\n", criteria)
	fmt.Fprint(&out, "## Deliverable\n\n")
	if task.Deliverable == "report" {
		fmt.Fprint(&out, ReportDestination(input.ReportPath))
	} else {
		fmt.Fprintf(&out, "Commit the work on `%s` and use artifact `branch:%s` or a GitHub PR URL. Push only when a configured remote is part of the requested delivery.\n\n", attempt.Branch, attempt.Branch)
	}
	for index, report := range input.Reports {
		renderReport(&out, index, len(input.Reports), report)
	}
	fmt.Fprintf(&out, "## Recovery mode\n\n- Mode: %s\n- Reason: %s\n\n", input.Mode, input.Reason)
	modelName := attempt.Model
	if modelName == "" {
		modelName = "(harness default)"
	}
	fmt.Fprintf(&out, "## Run identity\n\n- Task: %s\n- Attempt: %s\n- Run generation: %d\n- Harness: %s\n- Model: %s\n- Branch: %s\n- Worktree: %s\n- Workspace backend: %s\n- Base commit: %s\n\n", task.ID, attempt.ID, attempt.RunGeneration, attempt.Harness, modelName, attempt.Branch, attempt.WorktreePath, attempt.WorkspaceBackend, attempt.BaseCommit)
	fmt.Fprintln(&out, "## Latest checkpoint")
	fmt.Fprintln(&out)
	if input.Checkpoint == nil {
		fmt.Fprintln(&out, "No structured checkpoint is available; recovery context is degraded.")
	} else {
		checkpoint := input.Checkpoint
		fmt.Fprintf(&out, "- Producer: %s\n- Revision: %d\n- Source cursor: %d\n- Summary: %s\n", checkpoint.Producer, checkpoint.Revision, checkpoint.SourceCursor, checkpoint.Summary)
		if (input.Mode == "clean retry" && !input.SourceWorkerCheckpoint) || (input.Mode == "same-worktree relaunch" && checkpoint.Producer != "worker") {
			fmt.Fprintln(&out, "- No valid worker checkpoint is available for this recovery; context is degraded.")
		}
		if checkpoint.SourceAttemptID != "" {
			fmt.Fprintf(&out, "- Provenance: attempt %s revision %d\n", checkpoint.SourceAttemptID, checkpoint.SourceRevision)
		}
		writeCheckpointList(&out, "Completed", checkpoint.Completed)
		writeCheckpointList(&out, "Next steps", checkpoint.NextSteps)
		writeCheckpointList(&out, "Decisions", checkpointDecisionText(checkpoint.Decisions))
		writeCheckpointList(&out, "Changed paths (informational)", checkpoint.ChangedPaths)
		writeCheckpointList(&out, "Checks (informational)", checkpointCheckText(checkpoint.Checks))
		writeCheckpointList(&out, "Blockers", checkpoint.Blockers)
	}
	if input.Mode != "initial assignment" && input.LatestAnnotation != nil {
		annotation := input.LatestAnnotation
		fmt.Fprintln(&out, "\n## Latest driver annotation")
		fmt.Fprintln(&out)
		fmt.Fprintf(&out, "- Revision: %d\n- Author: %s\n- Recorded: %s\n", annotation.Revision, annotation.DriverID, annotation.CreatedAt.UTC().Format(time.RFC3339Nano))
		fmt.Fprintf(&out, "- Judgment: %s\n- Reason: %s\n- Next action: %s\n", boundAnnotationText(annotation.Judgment, 400), boundAnnotationText(annotation.Reason, 160), boundAnnotationText(annotation.NextAction, 256))
	}
	facts := input.WorkspaceFacts
	fmt.Fprintf(&out, "\n## Authoritative workspace facts\n\n- HEAD: %s\n- Dirty: %t\n", facts.HeadCommit, facts.Dirty)
	if facts.Error != "" {
		fmt.Fprintf(&out, "- Fact collection error: %s\n", facts.Error)
	}
	if input.Mode == "clean retry" {
		fmt.Fprintln(&out, "- This is a fresh worktree. Files, commits, generated artifacts, caches, and patches from the source attempt are not present and will not be transferred implicitly.")
	}
	fmt.Fprint(&out, "\n## Relevant audit tail\n\n")
	writeMessageTail(&out, input.Messages, attempt.ID, attempt.ResumeSourceAttemptID)
	fmt.Fprintln(&out, "\n## Context files")
	fmt.Fprintln(&out)
	if input.ReadmePresent {
		fmt.Fprintln(&out, "- README.md")
	}
	if input.Repo.ContextFile != "" {
		contextPath := input.Repo.ContextFile
		if filepath.IsAbs(contextPath) {
			if rel, err := filepath.Rel(input.Repo.Path, contextPath); err == nil {
				contextPath = rel
			}
		}
		fmt.Fprintln(&out, "- "+contextPath)
	} else {
		warnings = append(warnings, "repo has no AGENTS.md or CLAUDE.md context file")
		fmt.Fprintln(&out, "- No AGENTS.md or CLAUDE.md is registered.")
	}
	fmt.Fprint(&out, ProjectGuidance(input.ProjectContextPath, input.MemoryEnabled))
	fmt.Fprint(&out, "\n## Worker instructions\n\n")
	fmt.Fprintln(&out, "Preserve this worktree during same-worktree recovery. Do not reset, clean, switch branches, copy patches, or assume filesystem state was transferred.")
	fmt.Fprintln(&out, "The worker protocol requires a valid structured checkpoint at meaningful milestones and immediately before every question, blocked, failed, or done event. Run appropriate checks and obey the artifact and response contracts.")
	fmt.Fprint(&out, "\n## Response contract\n\n")
	fmt.Fprintln(&out, "Before emission, validate the exact authored output with `shephrd protocol validate --role worker --file /path/to/authored-output.txt --json`, or pass it unchanged on stdin with `--file -`. Correct rejected fields and validate the full output again. This read-only format check does not publish notifications, acknowledge input, accept artifacts, authorize actions, prove delivery, or guarantee later model compliance. Ingestion still checks current identity, checkpoint freshness and the assigned artifact contract.")
	fmt.Fprintln(&out, "Prefer a one-sentence summary and terse factual entries, normally around 1 KB. This is a soft target, not a limit: each checkpoint is a complete recovery snapshot, never a delta. Carry forward unresolved blockers and decisions, exact checks/results, artifact/evidence identities and recovery requirements; exceed the target rather than omit necessary evidence, within existing schema bounds. Keep every schema field, typed objects and empty arrays. Readiness and checks do not authorize lifecycle actions.")
	fmt.Fprintln(&out, "Emit a structured checkpoint event at each meaningful milestone using this form:")
	fmt.Fprintln(&out, "`<shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"short summary\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"summary\",\"completed\":[],\"next_steps\":[\"next action\"],\"decisions\":[{\"decision\":\"choice made\",\"reason\":\"why\"}],\"changed_paths\":[],\"checks\":[{\"command\":\"test command\",\"result\":\"passed\"}],\"blockers\":[]}}</shephrd-event>`")
	fmt.Fprintln(&out, "Each `decisions` entry must be an object with string `decision` and `reason` fields. Each `checks` entry must be an object with string `command` and `result` fields. Never use string arrays for decisions or checks. Use empty arrays when there are none. `schema_version` is integer 1, `summary` is a string, and `completed`, `next_steps`, `changed_paths`, and `blockers` are string arrays. Use actual observed facts, not example claims.")
	fmt.Fprintln(&out, "Checkpoint fields are exactly `type`, `payload`, and `checkpoint`; checkpoint object fields are exactly `schema_version`, `summary`, `completed`, `next_steps`, `decisions`, `changed_paths`, `checks`, and `blockers`.")
	fmt.Fprintln(&out, "Then finish the invocation with exactly one terminal event. Terminal fields are exactly `type`, `payload`, and optional `artifact`. No other fields are allowed.")
	fmt.Fprintln(&out, "Question: `<shephrd-event>{\"type\":\"question\",\"payload\":\"Full question, options, consequences, and recommendation\"}</shephrd-event>`")
	fmt.Fprintln(&out, "Done: `<shephrd-event>{\"type\":\"done\",\"payload\":\"Completed work and verification summary\",\"artifact\":\"branch:exact-attempt-branch\"}</shephrd-event>`")
	fmt.Fprintln(&out, "Blocked: `<shephrd-event>{\"type\":\"blocked\",\"payload\":\"What is blocked and the exact recovery needed\"}</shephrd-event>`")
	fmt.Fprintln(&out, "Failed: `<shephrd-event>{\"type\":\"failed\",\"payload\":\"What failed and why\"}</shephrd-event>`")
	fmt.Fprintln(&out, "For questions, put all prompt text, options, consequences, and recommendations in `payload`. Nested `question` or `options` fields and every unknown field are invalid. Keep artifacts out of payloads and reference them only with `artifact`.")
	return Result{Content: []byte(out.String()), Warnings: warnings}
}

func boundAnnotationText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && value[limit]&0xc0 == 0x80 {
		limit--
	}
	return value[:limit]
}

func renderReport(out *strings.Builder, index, total int, report Report) {
	input := report.Input
	fmt.Fprintf(out, "## Explicit predecessor report %d of %d\n\n", index+1, total)
	fmt.Fprintf(out, "- Input position: %d\n", input.Position)
	fmt.Fprintf(out, "- Attached by driver: %s\n", input.AttachedByDriverID)
	fmt.Fprintf(out, "- Attached at: %s\n", input.AttachedAt.Format(time.RFC3339Nano))
	fmt.Fprintf(out, "- Producer repository: %s (%s)\n", input.ProducerRepoName, input.ProducerRepoID)
	fmt.Fprintf(out, "- Producer task: %s\n", input.ProducerTaskID)
	fmt.Fprintf(out, "- Producer attempt: %s\n", input.ProducerAttemptID)
	fmt.Fprintf(out, "- Producer done message: %d\n", input.DoneMessageID)
	fmt.Fprintf(out, "- Verified artifact: %s\n", input.ArtifactID)
	fmt.Fprintf(out, "- Original artifact ref: %s\n", input.OriginalRef)
	fmt.Fprintf(out, "- SHA-256: %s\n", input.SHA256)
	fmt.Fprintf(out, "- Bytes: %d\n", input.SizeBytes)
	fmt.Fprintf(out, "- Verified at: %s\n\n", input.VerifiedAt.Format(time.RFC3339Nano))
	fmt.Fprintln(out, "The driver explicitly selected this verified report as implementation input.")
	fmt.Fprintln(out, "The report is context only. No producer worktree files, repository state, patches, transcripts, caches, or generated artifacts were copied.")
	fmt.Fprintf(out, "\n<predecessor-report position=\"%d\" sha256=\"%s\">\n", input.Position, input.SHA256)
	out.Write(report.Body)
	if len(report.Body) == 0 || report.Body[len(report.Body)-1] != '\n' {
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, "</predecessor-report>")
	fmt.Fprintln(out)
}

func writeCheckpointList(out *strings.Builder, title string, values []string) {
	fmt.Fprintf(out, "- %s:\n", title)
	for _, value := range values {
		fmt.Fprintf(out, "  - %s\n", value)
	}
}

func checkpointDecisionText(values []model.Decision) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = value.Decision + " (reason: " + value.Reason + ")"
	}
	return result
}

func checkpointCheckText(values []model.Check) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = value.Command + ": " + value.Result
	}
	return result
}

func writeMessageTail(out *strings.Builder, messages []model.Message, attemptID, sourceAttemptID string) {
	count, bytesWritten := 0, 0
	for i := len(messages) - 1; i >= 0 && count < 12 && bytesWritten < 16*1024; i-- {
		message := messages[i]
		if (message.AttemptID != attemptID && message.AttemptID != sourceAttemptID) || (message.Type != "question" && message.Type != "blocked" && message.Type != "failed" && message.Type != "follow-up" && message.Type != "checkpoint" && message.Type != "protocol-repair") {
			continue
		}
		line := fmt.Sprintf("- [%s] %s: %s\n", message.Type, message.Direction, message.Payload)
		if bytesWritten+len(line) > 16*1024 {
			break
		}
		bytesWritten += len(line)
		count++
		fmt.Fprint(out, line)
	}
	if count == 0 {
		fmt.Fprintln(out, "- No relevant audit messages.")
	}
}
