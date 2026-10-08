package store

import (
	"errors"
	"path/filepath"
	"testing"

	"shephrd/internal/model"
)

func TestProgressWithoutWorkerCheckpointRemainsNonterminal(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "progress", Objective: "work", Deliverable: "report"})
	attempt, _ := state.BeginAttempt(task.ID, "pi", "")
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tree", "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "progress", Payload: "started"}, 1, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	if message.Stale || message.Wake {
		t.Fatalf("message = %+v", message)
	}
	current, _ := state.Task(task.ID)
	if current.Status != "working" {
		t.Fatalf("task = %+v", current)
	}
}

func TestTerminalWithoutWorkerCheckpointBlocksAndRetainsSystemContext(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "protocol", Objective: "work", Deliverable: "report"})
	attempt, _ := state.BeginAttempt(task.ID, "pi", "")
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tree", "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	_, err = state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "report:x"}, 1, model.WorkspaceFacts{})
	if !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("error = %v", err)
	}
	current, _ := state.Task(task.ID)
	if current.Status != "blocked" {
		t.Fatalf("task = %+v", current)
	}
	checkpoint, err := state.LatestCheckpoint(attempt.ID)
	if err != nil || checkpoint.Producer != "system" {
		t.Fatalf("checkpoint = %+v, err = %v", checkpoint, err)
	}
}
