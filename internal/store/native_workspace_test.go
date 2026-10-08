package store

import (
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func nativeStateFixture(t *testing.T) (*Store, model.Task, model.Attempt) {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "native", Objective: "native"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "claude-code", "")
	if err != nil {
		t.Fatal(err)
	}
	return state, task, attempt
}

func TestNativeAllocationStateMachine(t *testing.T) {
	state, _, attempt := nativeStateFixture(t)
	if err := state.BeginNativeAllocation(attempt.ID, ""); err == nil {
		t.Fatal("empty intended path was accepted")
	}
	if err := state.BeginNativeAllocation(attempt.ID, "/worktrees/repo/attempt/demo"); err != nil {
		t.Fatal(err)
	}
	stored, _ := state.Attempt(attempt.ID)
	if stored.WorkspaceBackend != model.WorkspaceBackendNative || stored.WorkspaceState != model.WorkspaceStateAllocating ||
		stored.IntendedWorktreePath != "/worktrees/repo/attempt/demo" || stored.WorkspaceStateChangedAt == nil {
		t.Fatalf("allocating attempt = %+v", stored)
	}
	if err := state.BeginNativeAllocation(attempt.ID, "/elsewhere"); err == nil {
		t.Fatal("double allocation intent was accepted")
	}
	// Release refuses an unresolved allocation.
	if claimed, claimState, err := state.ClaimRelease(attempt.ID); err != nil || claimed || claimState != model.WorkspaceStateAllocating {
		t.Fatalf("claim during allocating = %t/%q, err = %v", claimed, claimState, err)
	}
	wrong := model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: "session", Path: "/wrong/path",
		GitDir: "gitdir", CommonDir: "common", Branch: "branch"}
	if err := state.ConfigureNativeWorkspace(attempt.ID, wrong); err == nil {
		t.Fatal("identity persistence with a mismatched intent path was accepted")
	}
	workspace := model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: "session", Path: "/worktrees/repo/attempt/demo",
		GitDir: "/common/worktrees/demo", CommonDir: "/common", Branch: "shephrd/task"}
	if err := state.ConfigureNativeWorkspace(attempt.ID, workspace); err != nil {
		t.Fatal(err)
	}
	stored, _ = state.Attempt(attempt.ID)
	if stored.WorkspaceState != model.WorkspaceStateHeld || stored.WorktreePath != "/worktrees/repo/attempt/demo" ||
		stored.WorktreeGitDir != "/common/worktrees/demo" || stored.WorktreeCommonDir != "/common" ||
		stored.Branch != "shephrd/task" || stored.Status != "working" || stored.ReleaseState != "held" {
		t.Fatalf("held attempt = %+v", stored)
	}
	// held -> releasing -> released keeps workspace and release state coherent.
	if claimed, _, err := state.ClaimRelease(attempt.ID); err != nil || !claimed {
		t.Fatalf("claim = %t, err = %v", claimed, err)
	}
	stored, _ = state.Attempt(attempt.ID)
	if stored.WorkspaceState != model.WorkspaceStateReleasing || stored.ReleaseState != "releasing" {
		t.Fatalf("releasing attempt = %+v", stored)
	}
	if err := state.ResetReleaseClaim(attempt.ID); err != nil {
		t.Fatal(err)
	}
	stored, _ = state.Attempt(attempt.ID)
	if stored.WorkspaceState != model.WorkspaceStateHeld || stored.ReleaseState != "held" {
		t.Fatalf("reset attempt = %+v", stored)
	}
	if claimed, _, err := state.ClaimRelease(attempt.ID); err != nil || !claimed {
		t.Fatalf("reclaim = %t, err = %v", claimed, err)
	}
	if err := state.MarkReleasedWithReason(attempt.ID, "native worktree removal completed"); err != nil {
		t.Fatal(err)
	}
	stored, _ = state.Attempt(attempt.ID)
	if stored.WorkspaceState != model.WorkspaceStateReleased || stored.ReleaseState != "released" || stored.ReleasedAt == nil {
		t.Fatalf("released attempt = %+v", stored)
	}
}

func TestNativeAllocationFailureAndUnknownClassification(t *testing.T) {
	state, _, attempt := nativeStateFixture(t)
	if err := state.BeginNativeAllocation(attempt.ID, "/worktrees/repo/attempt/demo"); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkNoWorkspace(attempt.ID, "workspace acquisition failed before any worktree existed"); err != nil {
		t.Fatal(err)
	}
	stored, _ := state.Attempt(attempt.ID)
	if stored.WorkspaceState != model.WorkspaceStateNoWorkspace || stored.ReleaseState != "no_workspace" || stored.ReleasedAt != nil {
		t.Fatalf("no_workspace attempt = %+v", stored)
	}

	_, _, unknown := nativeStateFixtureAttempt(t, state)
	if err := state.BeginNativeAllocation(unknown.ID, "/worktrees/repo/attempt2/demo"); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkWorkspaceUnknown(unknown.ID, "partial external evidence"); err != nil {
		t.Fatal(err)
	}
	stored, _ = state.Attempt(unknown.ID)
	if stored.WorkspaceState != model.WorkspaceStateUnknown || stored.Status != "unknown" {
		t.Fatalf("unknown attempt = %+v", stored)
	}
	if err := state.MarkNoWorkspace(unknown.ID, "late reclassification"); err == nil {
		t.Fatal("unknown workspace was reclassified as no_workspace")
	}
	if claimed, claimState, err := state.ClaimRelease(unknown.ID); err != nil || claimed || claimState != model.WorkspaceStateUnknown {
		t.Fatalf("claim of unknown workspace = %t/%q, err = %v", claimed, claimState, err)
	}
	// CompleteNativeAllocation only recovers a still-allocating intent.
	if err := state.CompleteNativeAllocation(unknown.ID, "/worktrees/repo/attempt2/demo", "gitdir", "common", "branch"); err == nil {
		t.Fatal("unknown workspace accepted allocation completion")
	}
}

func nativeStateFixtureAttempt(t *testing.T, state *Store) (model.Repo, model.Task, model.Attempt) {
	t.Helper()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo2", Path: filepath.Join(t.TempDir(), "repo2"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "native2", Objective: "native2"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "claude-code", "")
	if err != nil {
		t.Fatal(err)
	}
	return repo, task, attempt
}

func TestNativeCrashRecoveryCompletesAllocationExactlyOnce(t *testing.T) {
	state, _, attempt := nativeStateFixture(t)
	if err := state.BeginNativeAllocation(attempt.ID, "/worktrees/repo/attempt/demo"); err != nil {
		t.Fatal(err)
	}
	if err := state.CompleteNativeAllocation(attempt.ID, "/worktrees/repo/attempt/demo", "/common/worktrees/demo", "/common", "shephrd/task"); err != nil {
		t.Fatal(err)
	}
	stored, _ := state.Attempt(attempt.ID)
	if stored.WorkspaceState != model.WorkspaceStateHeld || stored.WorktreePath != "/worktrees/repo/attempt/demo" || stored.Branch != "shephrd/task" {
		t.Fatalf("recovered attempt = %+v", stored)
	}
	if err := state.CompleteNativeAllocation(attempt.ID, "/worktrees/repo/attempt/demo", "/common/worktrees/demo", "/common", "shephrd/task"); err == nil {
		t.Fatal("allocation completion is not single-shot")
	}
}

func TestTreehouseConfigurationIsRetired(t *testing.T) {
	state, _, attempt := nativeStateFixture(t)
	workspace := model.AttemptWorkspace{Backend: model.WorkspaceBackendTreehouse, SessionID: "session", Path: "/tree", LeaseID: "lease-1", Branch: "branch"}
	if err := state.ConfigureNativeWorkspace(attempt.ID, workspace); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("retired workspace configuration = %v", err)
	}
	stored, _ := state.Attempt(attempt.ID)
	if stored.WorkspaceBackend != "" || stored.WorkspaceState != model.WorkspaceStateUnassigned || stored.WorktreePath != "" || stored.LeaseID != "" {
		t.Fatalf("retired workspace configuration mutated attempt: %+v", stored)
	}
}

func TestClaimReleaseStillReportsNoWorkspace(t *testing.T) {
	state, _, attempt := nativeStateFixture(t)
	if err := state.MarkNoWorkspace(attempt.ID, "failed before workspace"); err != nil {
		t.Fatal(err)
	}
	claimed, claimState, err := state.ClaimRelease(attempt.ID)
	if err != nil || claimed || !strings.Contains(claimState, "no_workspace") {
		t.Fatalf("claim = %t/%q, err = %v", claimed, claimState, err)
	}
}
