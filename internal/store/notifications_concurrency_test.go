package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"shephrd/internal/model"
)

func TestConcurrentNotificationDrainsClaimOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	repo, err := first.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := first.CreateTask(model.Task{Title: "Test task", DriverID: "driver:one", RepoID: repo.ID, FeatureKey: "concurrent", Objective: "claim once"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := first.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ConfigureAttempt(attempt.ID, "session", "/tmp/tree", "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	attempt.RunGeneration = prepareAttempt(t, first, attempt)
	recordWorkerCheckpoint(t, first, attempt, attempt.RunGeneration, 1, []string{"ask"})
	if _, err := first.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "question", Payload: "one"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	results := make([]model.NotificationDrain, 2)
	errors := make([]error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		results[0], errors[0] = first.DrainNotifications(task.ID, "driver:one", "generation:one", 1, 30*time.Second)
	}()
	go func() {
		defer wait.Done()
		results[1], errors[1] = second.DrainNotifications(task.ID, "driver:two", "generation:two", 1, 30*time.Second)
	}()
	wait.Wait()
	for _, err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(results[0].Notifications) != 1 || len(results[1].Notifications) != 0 {
		t.Fatalf("owner-routed concurrent claims = %+v", results)
	}
}
