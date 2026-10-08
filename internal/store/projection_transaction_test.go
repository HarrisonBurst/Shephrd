package store

import (
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func projectionTransactionFixture(t *testing.T) (*Store, model.Task, model.Attempt) {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "projection", Objective: "projection"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "model")
	if err != nil {
		t.Fatal(err)
	}
	return state, task, attempt
}

func installFailingTaskProjectionTrigger(t *testing.T, state *Store, name, column string) {
	t.Helper()
	statement := `CREATE TRIGGER ` + name + ` BEFORE UPDATE OF ` + column + ` ON tasks BEGIN SELECT RAISE(ABORT, 'forced task projection failure'); END`
	if _, err := state.db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func dropProjectionTrigger(t *testing.T, state *Store, name string) {
	t.Helper()
	if _, err := state.db.Exec(`DROP TRIGGER ` + name); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureNativeWorkspaceRollsBackWhenTaskProjectionFails(t *testing.T) {
	state, task, attempt := projectionTransactionFixture(t)
	path := "/worktrees/repo/attempt/demo"
	if err := state.BeginNativeAllocation(attempt.ID, path); err != nil {
		t.Fatal(err)
	}
	installFailingTaskProjectionTrigger(t, state, "fail_native_configure_projection", "status")
	workspace := model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: "session", Path: path,
		GitDir: "/common/worktrees/demo", CommonDir: "/common", Branch: "shephrd/task"}
	if err := state.ConfigureNativeWorkspace(attempt.ID, workspace); err == nil {
		t.Fatal("ConfigureNativeWorkspace succeeded when the task projection failed")
	}
	storedAttempt, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedTask, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedAttempt.Status != model.AttemptStatusStarting || storedAttempt.SessionID != "" || storedAttempt.WorktreePath != "" ||
		storedAttempt.WorktreeGitDir != "" || storedAttempt.WorktreeCommonDir != "" || storedAttempt.Branch != "" ||
		storedAttempt.WorkspaceBackend != model.WorkspaceBackendNative || storedAttempt.WorkspaceState != model.WorkspaceStateAllocating ||
		storedAttempt.IntendedWorktreePath != path {
		t.Fatalf("native attempt configuration partially committed: %+v", storedAttempt)
	}
	if storedTask.Status != model.TaskStatusStarting {
		t.Fatalf("task status = %s, want %s", storedTask.Status, model.TaskStatusStarting)
	}
}

func TestRunnerProjectionWritesRollBackTogether(t *testing.T) {
	state, task, attempt := projectionTransactionFixture(t)
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tree", "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation := prepareAttempt(t, state, attempt)

	installFailingTaskProjectionTrigger(t, state, "fail_set_runner_projection", "process_alive")
	if err := state.SetRunnerForRun(attempt.ID, generation, 4321, true); err == nil {
		t.Fatal("SetRunnerForRun succeeded when the task projection failed")
	}
	storedAttempt, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedTask, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedAttempt.RunnerPID != 0 || storedAttempt.Status != model.AttemptStatusStarting || storedTask.ProcessAlive {
		t.Fatalf("runner start partially committed: task=%+v attempt=%+v", storedTask, storedAttempt)
	}
	dropProjectionTrigger(t, state, "fail_set_runner_projection")

	if err := state.SetRunnerForRun(attempt.ID, generation, 4321, true); err != nil {
		t.Fatal(err)
	}
	installFailingTaskProjectionTrigger(t, state, "fail_clear_runner_projection", "process_alive")
	if err := state.ClearRunnerForRun(attempt.ID, generation); err == nil {
		t.Fatal("ClearRunnerForRun succeeded when the task projection failed")
	}
	storedAttempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedTask, err = state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedAttempt.RunnerPID != 4321 || !storedTask.ProcessAlive {
		t.Fatalf("runner clear partially committed: task=%+v attempt=%+v", storedTask, storedAttempt)
	}
	dropProjectionTrigger(t, state, "fail_clear_runner_projection")

	installFailingTaskProjectionTrigger(t, state, "fail_finish_runner_projection", "process_alive")
	if err := state.FinishRunnerForRun(attempt.ID, generation, 9, "forced failure"); err == nil {
		t.Fatal("FinishRunnerForRun succeeded when the task projection failed")
	}
	storedAttempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedTask, err = state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedAttempt.RunnerPID != 4321 || storedAttempt.ExitCode != nil || storedAttempt.FailureReason != "" || !storedTask.ProcessAlive {
		t.Fatalf("runner finish partially committed: task=%+v attempt=%+v", storedTask, storedAttempt)
	}
}

func TestStopRollsBackStatusNotificationAndMessageTogether(t *testing.T) {
	state, task, attempt := projectionTransactionFixture(t)
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tree", "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation := prepareAttempt(t, state, attempt)
	recordWorkerCheckpoint(t, state, attempt, generation, 1, []string{"answer"})
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "answer?"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if err := state.SetRunnerForRun(attempt.ID, generation, 2468, true); err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil || len(notifications) != 1 || notifications[0].State != model.NotificationPending {
		t.Fatalf("notifications=%+v err=%v", notifications, err)
	}
	if _, err := state.db.Exec(`CREATE TRIGGER fail_stop_message BEFORE INSERT ON messages WHEN NEW.type='stop' BEGIN SELECT RAISE(ABORT, 'forced stop message failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := state.Stop(task.ID, "stop now"); err == nil {
		t.Fatal("Stop succeeded when its audit message failed")
	}
	storedTask, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedAttempt, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedNotification, err := state.Notification(notifications[0].NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := state.Messages(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedTask.Status != model.TaskStatusWorking || !storedTask.ProcessAlive || storedAttempt.Status != model.AttemptStatusWorking || storedAttempt.RunnerPID != 2468 || storedAttempt.EndedAt != nil {
		t.Fatalf("stop partially committed: task=%+v attempt=%+v", storedTask, storedAttempt)
	}
	if storedNotification.State != model.NotificationPending || storedNotification.SupersededAt != nil {
		t.Fatalf("notification supersession partially committed: %+v", storedNotification)
	}
	for _, message := range messages {
		if message.Type == "stop" {
			t.Fatalf("failed stop message was committed: %+v", message)
		}
	}
	if logs, err := state.NotificationDeliveryLogs(storedNotification.NotificationID, 10); err != nil || len(logs) != 0 {
		t.Fatalf("failed stop delivery logs=%+v err=%v", logs, err)
	}

	dropProjectionTrigger(t, state, "fail_stop_message")
	if err := state.Stop(task.ID, "stop now"); err != nil {
		t.Fatal(err)
	}
	storedTask, _ = state.Task(task.ID)
	storedAttempt, _ = state.Attempt(attempt.ID)
	storedNotification, _ = state.Notification(notifications[0].NotificationID)
	messages, _ = state.Messages(task.ID)
	if storedTask.Status != model.TaskStatusStopped || storedTask.ProcessAlive || storedAttempt.Status != model.AttemptStatusStopped || storedAttempt.RunnerPID != 0 || storedAttempt.EndedAt == nil {
		t.Fatalf("committed stop is incoherent: task=%+v attempt=%+v", storedTask, storedAttempt)
	}
	if storedNotification.State != model.NotificationSuperseded {
		t.Fatalf("notification state = %s", storedNotification.State)
	}
	last := messages[len(messages)-1]
	if last.Type != "stop" || last.Direction != "driver-to-worker" || !strings.Contains(last.Payload, "Checkpoint capture: degraded") {
		t.Fatalf("stop message = %+v", last)
	}
}
