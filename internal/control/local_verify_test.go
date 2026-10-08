package control

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestVerifyLocalPersistsAttemptProofBeforeOneNativeRelease(t *testing.T) {
	service, state, task, attempt, _ := localVerifyFixture(t)
	defer state.Close()
	results := make(chan error, 2)
	var verified [2]bool
	var wait sync.WaitGroup
	for index := range verified {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, err := service.VerifyLocal(task.ID, "", "")
			verified[index] = result.Landed && result.Worktree.Released
			results <- err
		}(index)
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !verified[0] || !verified[1] {
		t.Fatalf("verification results = %v", verified)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.LandedProven || stored.LandingKind != "local_default_branch" || stored.LandedSourceCommit == "" || stored.LandedTargetRef != "refs/heads/main" || stored.LandedCheckpointRevision != 2 || stored.ReleaseState != "released" || stored.ReleasedAt == nil {
		t.Fatalf("attempt = %+v", stored)
	}
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Landed || current.RemoteDeliveryState != "unverified" || current.BranchPushed {
		t.Fatalf("task = %+v", current)
	}
	if _, err := os.Stat(attempt.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("released native worktree still exists: %v", err)
	}
	if _, err := service.VerifyLocal(task.ID, attempt.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	second, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyLocal(task.ID, attempt.ID, ""); err != nil {
		t.Fatal(err)
	}
	current, _ = state.Task(task.ID)
	if current.CurrentAttemptID != second.ID || current.Landed {
		t.Fatalf("superseded proof was projected onto current task: %+v", current)
	}
}

func TestReleaseClaimHelper(t *testing.T) {
	if os.Getenv("SHEPHRD_RELEASE_HELPER") != "1" {
		return
	}
	state, err := store.Open(os.Getenv("SHEPHRD_RELEASE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	claimed, _, err := state.ClaimRelease(os.Getenv("SHEPHRD_RELEASE_ATTEMPT"))
	if err != nil || !claimed {
		t.Fatalf("claim = %t, err = %v", claimed, err)
	}
}

func localVerifyFixture(t *testing.T) (Service, *store.Store, model.Task, model.Attempt, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	worker := filepath.Join(t.TempDir(), "worker")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, root, "init", "-b", "main")
	runLocalGit(t, root, "config", "user.name", "Test")
	runLocalGit(t, root, "config", "user.email", "test@example.com")
	runLocalGit(t, root, "commit", "--allow-empty", "-m", "base")
	base := runLocalGit(t, root, "rev-parse", "HEAD")
	runLocalGit(t, root, "worktree", "add", "-b", "shephrd/task-1", worker, "main")
	if err := os.WriteFile(filepath.Join(worker, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, worker, "add", "work.txt")
	runLocalGit(t, worker, "commit", "-m", "worker")
	source := runLocalGit(t, worker, "rev-parse", "HEAD")
	databasePath := filepath.Join(t.TempDir(), "state.db")
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "local", Objective: "local", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", worker, "", "shephrd/task-1"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetAttemptBaseCommit(attempt.ID, base); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{HeadCommit: base}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: 1, Summary: "done", NextSteps: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "done", Checkpoint: &checkpoint}, 1,
		model.WorkspaceFacts{HeadCommit: source}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "branch:shephrd/task-1"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, root, "merge", "--no-ff", "-m", "merge worker", "shephrd/task-1")
	task, _ = state.Task(task.ID)
	attempt, _ = state.Attempt(attempt.ID)
	service := New(config.Config{DataDir: t.TempDir(), WorktreeRoot: filepath.Dir(worker)}, state)
	return service, state, task, attempt, databasePath
}

func runLocalGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), stderr.String(), err)
	}
	return strings.TrimSpace(stdout.String())
}
