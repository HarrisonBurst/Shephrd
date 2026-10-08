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

func sweepAttempt(t *testing.T, feature string) (Service, *store.Store, model.Task, model.Attempt) {
	t.Helper()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	repo, err := state.UpsertRepo(model.Repo{Name: "sweep", Path: filepath.Join(root, "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Sweep runner", DriverID: "driver:sweep", RepoID: repo.ID, FeatureKey: feature, Objective: "sweep runner", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "worktree")
	if err := state.BeginNativeAllocation(attempt.ID, worktree); err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureNativeWorkspace(attempt.ID, model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: "session", Path: worktree, GitDir: filepath.Join(root, "git-dir"), CommonDir: filepath.Join(root, "git-common"), Branch: "branch"}); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt.RunGeneration = generation
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if err := state.SetRunnerForRun(attempt.ID, generation, 98765, true); err != nil {
		t.Fatal(err)
	}
	service := New(config.Config{DataDir: filepath.Join(root, "data")}, state)
	service.processes.alive = func(int) bool { return false }
	return service, state, task, attempt
}

func TestSweepDeathsSettlesTerminalAttemptsAndRetainsPreTerminalFailure(t *testing.T) {
	t.Run("terminal", func(t *testing.T) {
		service, state, task, attempt := sweepAttempt(t, "terminal")
		checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "done ready", NextSteps: []string{"finish"}}
		if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "done", Payload: "done", Artifact: "branch:branch"}, 2, model.WorkspaceFacts{}); err != nil {
			t.Fatal(err)
		}
		drain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:sweep", 1, 30*time.Second)
		if err != nil || len(drain.Notifications) != 1 {
			t.Fatalf("drain = %+v, err = %v", drain, err)
		}
		terminal := drain.Notifications[0]
		if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: terminal.NotificationID, ClaimToken: terminal.ClaimToken, ConsumerID: task.DriverID, DriverGeneration: "generation:sweep", HandlingID: "handling:sweep"}); err != nil {
			t.Fatal(err)
		}
		if err := service.SweepDeaths(); err != nil {
			t.Fatal(err)
		}
		stored, err := state.Attempt(attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		notifications, err := state.Notifications(task.ID)
		if err != nil || stored.RunnerPID != 0 || stored.ExitCode == nil || *stored.ExitCode != -1 || len(notifications) != 2 || notifications[1].Kind != model.SettledMessageType || !strings.Contains(notifications[1].Payload, "swept_dead") {
			t.Fatalf("attempt = %+v notifications = %+v err = %v", stored, notifications, err)
		}
	})

	t.Run("pre-terminal", func(t *testing.T) {
		service, state, task, attempt := sweepAttempt(t, "pre-terminal")
		if err := service.SweepDeaths(); err != nil {
			t.Fatal(err)
		}
		storedTask, err := state.Task(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		storedAttempt, err := state.Attempt(attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		notifications, err := state.Notifications(task.ID)
		if err != nil || storedTask.Status != model.TaskStatusBlocked || storedAttempt.Status != model.AttemptStatusBlocked || storedAttempt.RunnerPID != 0 || len(notifications) != 1 || notifications[0].Kind != "blocked" || !strings.Contains(notifications[0].Payload, "died before a terminal result") {
			t.Fatalf("task = %+v attempt = %+v notifications = %+v err = %v", storedTask, storedAttempt, notifications, err)
		}
	})
}
