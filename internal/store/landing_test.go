package store

import (
	"path/filepath"
	"testing"

	"shephrd/internal/model"
)

func TestLandingProofIsImmutableAttemptBoundAndMonotonic(t *testing.T) {
	state, task, attempt := landingAttempt(t)
	defer state.Close()
	proof := model.LandingProof{TaskID: task.ID, AttemptID: attempt.ID, RunGeneration: attempt.RunGeneration,
		Kind: "local_default_branch", SourceCommit: "source", TargetRef: "refs/heads/main",
		TargetCommit: "target-1", CheckpointRevision: 2}
	stored, err := state.RecordLandingProof(attempt.ID, proof, "landed locally")
	if err != nil {
		t.Fatal(err)
	}
	proof.TargetCommit = "target-2"
	again, err := state.RecordLandingProof(attempt.ID, proof, "later target")
	if err != nil {
		t.Fatal(err)
	}
	if again.TargetCommit != stored.TargetCommit || again.VerifiedAt != stored.VerifiedAt {
		t.Fatalf("immutable proof changed: first=%+v again=%+v", stored, again)
	}
	current, _ := state.Task(task.ID)
	if !current.Landed {
		t.Fatalf("task = %+v", current)
	}
}

func TestRemoteDeliveryCannotCreateLandingProof(t *testing.T) {
	state, task, attempt := landingAttempt(t)
	defer state.Close()
	if err := state.SetRemoteDelivery(task.ID, attempt.ID, task.CurrentAttemptID, attempt.RunGeneration, true, "pushed", "OPEN"); err != nil {
		t.Fatal(err)
	}
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.BranchPushed || current.RemoteDeliveryState != "pushed" || current.PRState != "OPEN" || current.Landed || stored.LandedProven {
		t.Fatalf("delivery created landing proof: task=%+v attempt=%+v", current, stored)
	}
}

func TestLandingProofRejectsStaleCurrentAttemptSnapshot(t *testing.T) {
	state, task, attempt := landingAttempt(t)
	defer state.Close()
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.BeginAttempt(task.ID, "pi", ""); err != nil {
		t.Fatal(err)
	}
	_, err := state.RecordLandingProof(attempt.ID, model.LandingProof{TaskID: task.ID, AttemptID: attempt.ID,
		RunGeneration: attempt.RunGeneration, Kind: "local_default_branch", SourceCommit: "source",
		TargetRef: "refs/heads/main", TargetCommit: "target", CheckpointRevision: 2}, "stale")
	if err == nil {
		t.Fatal("stale current-attempt snapshot received landing proof")
	}
	stored, _ := state.Attempt(attempt.ID)
	if stored.LandedProven {
		t.Fatalf("attempt = %+v", stored)
	}
}

func landingAttempt(t *testing.T) (*Store, model.Task, model.Attempt) {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "landing", Objective: "landing", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(attempt.ID, "session", t.TempDir(), "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetAttemptBaseCommit(attempt.ID, "base"); err != nil {
		t.Fatal(err)
	}
	generation := prepareAttempt(t, state, attempt)
	checkpoint := model.Checkpoint{SchemaVersion: 1, Summary: "done", NextSteps: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "done", Checkpoint: &checkpoint}, 1,
		model.WorkspaceFacts{HeadCommit: "source"}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "branch:branch"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	task, _ = state.Task(task.ID)
	attempt, _ = state.Attempt(attempt.ID)
	return state, task, attempt
}
