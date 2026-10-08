package store

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestArchiveTaskRequiresTerminalTaskWithoutLiveWorker(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "demo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "queued", Objective: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ArchiveTask(queued.ID); err == nil || !strings.Contains(err.Error(), "only terminal tasks can be archived") {
		t.Fatalf("nonterminal archive error = %v", err)
	}
	for _, status := range []string{"done", "blocked", "failed", "stopped"} {
		task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: status, Objective: status})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.db.Exec(`UPDATE tasks SET status=? WHERE id=?`, status, task.ID); err != nil {
			t.Fatal(err)
		}
		if status == "done" {
			if _, err := state.db.Exec(`UPDATE tasks SET process_alive=1 WHERE id=?`, task.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := state.ArchiveTask(task.ID); err == nil || !strings.Contains(err.Error(), "has a live worker") {
				t.Fatalf("live archive error = %v", err)
			}
			if _, err := state.db.Exec(`UPDATE tasks SET process_alive=0 WHERE id=?`, task.ID); err != nil {
				t.Fatal(err)
			}
		}
		archived, err := state.ArchiveTask(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if archived.Status != status || archived.ArchivedAt == nil {
			t.Fatalf("archived task = %+v", archived)
		}
	}
}

func TestArchiveTaskVisibilityIdempotencyAndHistoryRetention(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "demo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "archive", Objective: "archive", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tmp/archive-worktree", "lease-archive", "shephrd/archive"); err != nil {
		t.Fatal(err)
	}
	attempt.RunGeneration = prepareAttempt(t, state, attempt)
	if _, err := state.AddOutboundForRun(task.ID, attempt.ID, attempt.RunGeneration, "assign", "assignment"); err != nil {
		t.Fatal(err)
	}
	recordWorkerCheckpoint(t, state, attempt, attempt.RunGeneration, 1, []string{})
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "done", Payload: "complete", Artifact: "report:/tmp/report.md"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	before, err := state.Detail(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := state.ArchiveTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.ArchivedAt == nil {
		t.Fatal("archived_at was not recorded")
	}
	after, err := state.Detail(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Attempts, after.Attempts) {
		t.Fatalf("attempt history changed:\nbefore=%+v\nafter=%+v", before.Attempts, after.Attempts)
	}
	if !reflect.DeepEqual(before.Messages, after.Messages) {
		t.Fatalf("message history changed:\nbefore=%+v\nafter=%+v", before.Messages, after.Messages)
	}
	if !reflect.DeepEqual(before.Checkpoints, after.Checkpoints) {
		t.Fatalf("checkpoint history changed:\nbefore=%+v\nafter=%+v", before.Checkpoints, after.Checkpoints)
	}
	if !reflect.DeepEqual(before.Notifications, after.Notifications) {
		t.Fatalf("notification history changed:\nbefore=%+v\nafter=%+v", before.Notifications, after.Notifications)
	}
	if after.Task.Status != before.Task.Status || after.Task.CurrentAttemptID != before.Task.CurrentAttemptID || after.Task.ArtifactRef != before.Task.ArtifactRef || after.Task.Landed != before.Task.Landed || after.Task.DiscardAuthorized != before.Task.DiscardAuthorized {
		t.Fatalf("task lifecycle state changed:\nbefore=%+v\nafter=%+v", before.Task, after.Task)
	}
	listed, err := state.Tasks(TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("default tasks = %+v", listed)
	}
	listed, err = state.Tasks(TaskFilter{Status: "done", RepoID: repo.ID, IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != task.ID || listed[0].ArchivedAt == nil {
		t.Fatalf("included tasks = %+v", listed)
	}
	repeated, err := state.ArchiveTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !repeated.ArchivedAt.Equal(*archived.ArchivedAt) || !repeated.UpdatedAt.Equal(archived.UpdatedAt) {
		t.Fatalf("idempotent archive changed timestamps: first=%+v repeated=%+v", archived, repeated)
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	reactivated, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reactivated.ArchivedAt != nil || reactivated.Status != "queued" {
		t.Fatalf("retried task = %+v", reactivated)
	}
}
