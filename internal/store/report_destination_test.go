package store

import (
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
)

func TestReportDestinationFencesTerminalAcceptance(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, destination := range []string{"current", "legacy", "stale-run", "foreign-attempt", "foreign-task", "arbitrary"} {
			t.Run(destination+map[bool]string{false: "/single", true: "/batch"}[batch], func(t *testing.T) {
				root := t.TempDir()
				state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(root, "state.db"))
				path, err := state.AssignReportDestination(task.ID, attempt.ID, generation, root)
				if err != nil {
					t.Fatal(err)
				}
				stored, err := state.Attempt(attempt.ID)
				if err != nil || stored.ReportPath != path {
					t.Fatalf("destination projection = %+v err=%v", stored, err)
				}
				if _, err := state.AssignReportDestination(task.ID, attempt.ID, generation, root); err == nil {
					t.Fatal("destination could be reassigned")
				}
				submitted := path
				switch destination {
				case "legacy":
					submitted = filepath.Join(root, task.ID, "report.md")
				case "stale-run":
					submitted, _ = model.RunReportPath(root, task.ID, attempt.ID, generation-1)
				case "foreign-attempt":
					submitted, _ = model.RunReportPath(root, task.ID, "attempt_foreign", generation)
				case "foreign-task":
					submitted, _ = model.RunReportPath(root, "task_foreign", attempt.ID, generation)
				case "arbitrary":
					submitted = filepath.Join(root, "draft.md")
				}
				recordWorkerCheckpoint(t, state, attempt, generation, 1, []string{"finish"})
				event := model.Event{Type: "done", Payload: "report complete", Artifact: "report:" + submitted}
				var diagnostic *adapter.Diagnostic
				if batch {
					_, diagnostic, err = state.AddEventCandidateForRun(attempt.ID, generation, "session", []model.Event{event}, 2, model.WorkspaceFacts{}, EventRepairRequest{ID: "repair_report", CandidateHash: strings.Repeat("a", 64)}, false)
				} else {
					_, err = state.AddEventForRun(attempt.ID, generation, event, 2, model.WorkspaceFacts{})
				}
				current, _ := state.Task(task.ID)
				if destination == "current" {
					if err != nil || diagnostic != nil || !current.ClaimedDone || current.ArtifactRef != event.Artifact {
						t.Fatalf("current report rejected: %+v diagnostic=%+v err=%v", current, diagnostic, err)
					}
				} else if current.ClaimedDone || current.ArtifactRef != "" || err == nil && diagnostic == nil {
					t.Fatalf("foreign destination accepted: %+v diagnostic=%+v err=%v", current, diagnostic, err)
				}
			})
		}
	}
}

func TestReportDestinationAssignmentRejectsStaleAndForeignIdentity(t *testing.T) {
	root := t.TempDir()
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(root, "state.db"))
	for _, identity := range []struct {
		task, attempt string
		generation    int
	}{{task.ID, attempt.ID, generation - 1}, {task.ID, "attempt_foreign", generation}, {"task_foreign", attempt.ID, generation}} {
		if _, err := state.AssignReportDestination(identity.task, identity.attempt, identity.generation, root); err != ErrStaleRun {
			t.Fatalf("assignment %+v: %v", identity, err)
		}
	}
}
