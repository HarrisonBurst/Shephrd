package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"shephrd/internal/model"
)

func TestSQLiteConstraintClassificationUsesCodes(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", sqliteCodeError(sqliteConstraintUnique))
	if !isSQLiteConstraint(err, sqliteConstraintUnique) || isSQLiteConstraint(err, sqliteConstraintTrigger) {
		t.Fatalf("constraint classification = %v", err)
	}
}

func TestConstraintErrorContracts(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "repo-1"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "repo-2"), DefaultBranch: "main"}); !errors.Is(err, ErrRepoNameConflict) || err.Error() != `repo name "demo" is already registered; use --name with a unique alias` {
		t.Fatalf("repo name conflict = %v", err)
	}

	task := model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "feature", Objective: "work"}
	if _, err := state.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CreateTask(task); !errors.Is(err, ErrTaskFeatureConflict) || err.Error() != `feature "feature" already has a task in this repo; choose a different --feature key` {
		t.Fatalf("task feature conflict = %v", err)
	}

	firstList, err := state.CreatePlan("release", "driver:first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.CreatePlan("release", "driver:first"); !errors.Is(err, ErrPlanNameConflict) || err.Error() != `driver driver:first already has a plan named "release"` {
		t.Fatalf("plan name conflict = %v", err)
	}
	secondList, err := state.CreatePlan("release", "driver:second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.AdoptPlan(secondList.ID, "driver:second", "driver:first"); !errors.Is(err, ErrPlanNameConflict) || err.Error() != `driver driver:first already owns a plan with this name` {
		t.Fatalf("adopt plan name conflict = %v", err)
	}

	first, err := state.AddPlanItem(firstList.ID, model.PlanItem{Title: "Test item", Objective: "first"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.AddPlanItem(firstList.ID, model.PlanItem{Title: "Test item", Objective: "second"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.AddPlanPrerequisite(firstList.ID, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := state.AddPlanPrerequisite(firstList.ID, second.ID, first.ID); !errors.Is(err, ErrPlanPrerequisiteCycle) || err.Error() != "adding prerequisite "+first.ID+" to item "+second.ID+" would create a cycle" {
		t.Fatalf("plan prerequisite cycle = %v", err)
	}
}

func TestStatusVocabularies(t *testing.T) {
	for _, status := range []string{
		model.TaskStatusQueued,
		model.TaskStatusStarting,
		model.TaskStatusWorking,
		model.TaskStatusWaiting,
		model.TaskStatusDone,
		model.TaskStatusBlocked,
		model.TaskStatusFailed,
		model.TaskStatusStopped,
	} {
		if !model.ValidTaskStatus(status) {
			t.Errorf("task status %q is not valid", status)
		}
	}
	for _, status := range []string{
		model.AttemptStatusStarting,
		model.AttemptStatusWorking,
		model.AttemptStatusWaiting,
		model.AttemptStatusDone,
		model.AttemptStatusBlocked,
		model.AttemptStatusFailed,
		model.AttemptStatusStopped,
		model.AttemptStatusSuperseded,
		model.AttemptStatusWorkspaceUnknown,
	} {
		if !model.ValidAttemptStatus(status) {
			t.Errorf("attempt status %q is not valid", status)
		}
	}
	for _, status := range []string{
		model.WorkspaceStateUnassigned,
		model.WorkspaceStateAllocating,
		model.WorkspaceStateHeld,
		model.WorkspaceStateNoWorkspace,
		model.WorkspaceStateUnknown,
		model.WorkspaceStateReleasing,
		model.WorkspaceStateReleased,
	} {
		if !model.ValidWorkspaceState(status) {
			t.Errorf("workspace state %q is not valid", status)
		}
	}
	if model.ValidTaskStatus("mystery") || model.ValidAttemptStatus("mystery") || model.ValidWorkspaceState("mystery") {
		t.Fatal("unrecognized status was accepted")
	}
	if model.ValidAttemptStatus(model.TaskStatusQueued) {
		t.Fatal("queued task status was accepted as an attempt status")
	}
}
