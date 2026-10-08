package control

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestReleaseContentionTimeoutUsesClockCapability(t *testing.T) {
	service, state, task, attempt, _ := localVerifyFixture(t)
	defer state.Close()
	if err := state.AuthorizeDiscard(task.ID, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if claimed, _, err := state.ClaimRelease(attempt.ID); err != nil || !claimed {
		t.Fatalf("claim = %t, err = %v", claimed, err)
	}
	current := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	var slept time.Duration
	service.clock = clockCapability{
		now: func() time.Time { return current },
		sleep: func(duration time.Duration) {
			current = current.Add(duration)
			slept += duration
		},
	}
	service.processes = processCapability{alive: func(int) bool { return false }}
	if err := service.Release(task.ID, attempt.ID, false); err == nil || !strings.Contains(err.Error(), "release is still in progress") {
		t.Fatalf("release error = %v", err)
	}
	if slept != 5*time.Second {
		t.Fatalf("simulated wait = %s", slept)
	}
}

func TestNativeSessionTimeoutUsesClockAndProcessCapabilities(t *testing.T) {
	service, state, _, attempt := capabilityAttempt(t, "pending:codex")
	defer state.Close()
	current := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	var slept time.Duration
	service.clock = clockCapability{
		now: func() time.Time { return current },
		sleep: func(duration time.Duration) {
			current = current.Add(duration)
			slept += duration
		},
	}
	service.processes = processCapability{alive: func(pid int) bool { return pid == attempt.RunnerPID }}
	if _, err := service.awaitNativeSession(attempt.ID); err == nil || !strings.Contains(err.Error(), "within 10s") {
		t.Fatalf("session wait error = %v", err)
	}
	if slept != 10*time.Second {
		t.Fatalf("simulated wait = %s", slept)
	}
}

func TestProcessCapabilityControlsLifecycleStop(t *testing.T) {
	service, state, task, attempt := capabilityAttempt(t, "session")
	defer state.Close()
	var stopped int
	service.processes = processCapability{stop: func(pid int) error {
		stopped = pid
		return nil
	}}
	if err := service.Stop(task.ID, "test stop", false); err != nil {
		t.Fatal(err)
	}
	if stopped != attempt.RunnerPID {
		t.Fatalf("stopped pid = %d, want %d", stopped, attempt.RunnerPID)
	}
}

func capabilityAttempt(t *testing.T, sessionID string) (Service, *store.Store, model.Task, model.Attempt) {
	t.Helper()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "capability", Objective: "capability"})
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "codex", "")
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	workspacePath := filepath.Join(root, "worktree")
	if err := state.BeginNativeAllocation(attempt.ID, workspacePath); err != nil {
		state.Close()
		t.Fatal(err)
	}
	if err := state.ConfigureNativeWorkspace(attempt.ID, model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: sessionID,
		Path: workspacePath, GitDir: filepath.Join(root, "git-dir"), CommonDir: filepath.Join(root, "common-dir"), Branch: "branch"}); err != nil {
		state.Close()
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	if err := state.SetRunnerForRun(attempt.ID, generation, 4242, true); err != nil {
		state.Close()
		t.Fatal(err)
	}
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	return New(config.Config{DataDir: filepath.Join(root, "data")}, state), state, task, attempt
}
