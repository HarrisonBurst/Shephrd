package verify

import (
	"errors"
	"reflect"
	"testing"

	"shephrd/internal/delivery"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestCompleteProofRecordsBeforeConditionalRelease(t *testing.T) {
	state, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "verify", Objective: "verify", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	boundary := Boundary{Store: state, Release: func(taskID, attemptID string) error {
		calls = append(calls, "release:"+taskID+":"+attemptID)
		return nil
	}}
	record := func(*delivery.Result) error {
		calls = append(calls, "record")
		return nil
	}
	result, err := boundary.completeProof(task, attempt, delivery.Result{Landed: true}, record, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"record", "release:" + task.ID + ":" + attempt.ID}
	if !reflect.DeepEqual(calls, want) || result.Worktree.ReleaseState != "held" {
		t.Fatalf("calls=%v result=%+v", calls, result)
	}

	calls = nil
	if _, err := boundary.completeProof(task, attempt, delivery.Result{Landed: true}, record, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"record"}) {
		t.Fatalf("non-release calls=%v", calls)
	}

	calls = nil
	persistErr := errors.New("persist failed")
	if _, err := boundary.completeProof(task, attempt, delivery.Result{Landed: true}, func(*delivery.Result) error { return persistErr }, true); !errors.Is(err, persistErr) {
		t.Fatalf("error=%v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("release ran after persistence failure: %v", calls)
	}
}
