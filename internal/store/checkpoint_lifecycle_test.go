package store

import (
	"path/filepath"
	"testing"

	"shephrd/internal/model"
)

func TestCheckpointLifecycleAndGenerationFence(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "r", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "x", Objective: "x", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(attempt.ID, "sess", "/tmp", "lease", "b"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assign", []string{"go"}, model.WorkspaceFacts{HeadCommit: "abc", Dirty: true}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: 1, Summary: "did", NextSteps: []string{"done"}}
	message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "did", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{HeadCommit: "def", Dirty: true})
	if err != nil || message.RunGeneration != generation {
		t.Fatalf("message = %+v, err = %v", message, err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "ok", Artifact: "report:x"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	latest, err := state.LatestCheckpoint(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Producer != "worker" || latest.Revision != 2 || latest.HeadCommit != "def" || !latest.WorktreeDirty {
		t.Fatalf("latest = %+v", latest)
	}
	stale, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "late", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{HeadCommit: "bad"})
	if err != nil || !stale.Stale {
		t.Fatalf("stale = %+v, err = %v", stale, err)
	}
	latest, _ = state.LatestCheckpoint(attempt.ID)
	if latest.HeadCommit != "def" {
		t.Fatalf("stale checkpoint replaced latest: %+v", latest)
	}
	messages, err := state.Messages(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 || messages[1].CheckpointJSON == "" || messages[2].Type != "done" {
		t.Fatalf("messages = %+v", messages)
	}
	oldAttempt, _ := state.Attempt(attempt.ID)
	if err := state.FinishRunnerForRun(attempt.ID, generation, 0, ""); err != nil {
		t.Fatal(err)
	}
	nextGeneration, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil || nextGeneration != generation+1 {
		t.Fatalf("next generation = %d, err = %v", nextGeneration, err)
	}
	if err := state.SetRunnerForRun(attempt.ID, generation, 99999, true); err == nil {
		t.Fatal("old generation mutation was accepted")
	}
	current, _ := state.Attempt(attempt.ID)
	if current.RunGeneration != oldAttempt.RunGeneration+1 {
		t.Fatalf("generation did not advance: %+v", current)
	}
}
