package store

import (
	"path/filepath"
	"testing"

	"shephrd/internal/model"
)

func TestRetryCarriesCheckpointProvenanceWithoutFiles(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "retry", Objective: "work", Deliverable: "report"})
	first, _ := state.BeginAttempt(task.ID, "pi", "")
	_ = state.ConfigureAttempt(first.ID, "session", "/old", "lease-old", "branch-old")
	generation, err := state.ReserveRunGeneration(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(first.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: 1, Summary: "progress", Completed: []string{"one"}, NextSteps: []string{"two"}}
	if _, err := state.AddEventForRun(first.ID, generation, model.Event{Type: "checkpoint", Payload: "progress", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(first.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "report:/old"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	second, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ResumeSourceAttemptID != first.ID || second.ResumeSourceRevision != 2 {
		t.Fatalf("source = %+v", second)
	}
	_ = state.ConfigureAttempt(second.ID, "session-new", "/new", "lease-new", "branch-new")
	newGeneration, err := state.ReserveRunGeneration(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := state.RecordSystemCheckpoint(second.ID, newGeneration, "assigned", []string{"work"}, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	if inherited.Producer != "system" || inherited.SourceAttemptID != first.ID || inherited.SourceRevision != 2 || inherited.Summary != "progress" {
		t.Fatalf("inherited = %+v", inherited)
	}
	if inherited.Branch != "branch-new" || inherited.SessionID != "session-new" {
		t.Fatalf("new identity = %+v", inherited)
	}
	continued := model.Checkpoint{SchemaVersion: 1, Summary: "continued", NextSteps: []string{"done"}}
	if _, err := state.AddEventForRun(second.ID, newGeneration, model.Event{Type: "checkpoint", Payload: "continued", Checkpoint: &continued}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
}
