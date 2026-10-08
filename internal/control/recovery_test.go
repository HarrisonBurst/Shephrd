package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestRecoveryBriefContainsBoundedCheckpointContextAndTransferRule(t *testing.T) {
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "brief", Objective: "recover", AcceptanceCriteria: "checks pass", Deliverable: "report"})
	attempt, _ := state.BeginAttempt(task.ID, "pi", "Luna/Sol")
	_ = configureAttempt(t, state, attempt.ID, "session", root, "lease", "branch")
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: 1, Summary: "source progress", Completed: []string{"one"}, NextSteps: []string{"two"}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "source progress", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{HeadCommit: "head", Dirty: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "need choice"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddAnnotation(model.AnnotationScope{TaskID: task.ID}, "driver:test", 0, model.AnnotationInput{
		Judgment: "Approved findings 2 and 5", Reason: "Material review findings", NextAction: "worker send approved findings, then require re-review",
	}); err != nil {
		t.Fatal(err)
	}
	service := New(config.Config{DataDir: filepath.Join(root, "data")}, state)
	attempt, _ = state.Attempt(attempt.ID)
	initialPath, _, err := service.renderBrief(task, repo, attempt)
	if err != nil {
		t.Fatal(err)
	}
	initialBody, err := os.ReadFile(initialPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(initialBody), "Latest driver annotation") || strings.Contains(string(initialBody), "Approved findings 2 and 5") {
		t.Fatalf("initial brief included a driver annotation:\n%s", initialBody)
	}
	path, _, err := service.renderRecoveryBrief(task, repo, attempt, "same-worktree relaunch", "driver recovery")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(body)
	for _, want := range []string{"same-worktree relaunch", "source progress", "head", "Dirty", "driver recovery", "checkpoint", "immediately before every", "Luna/Sol", `"decision":"choice made"`, `"reason":"why"`, `"command":"test command"`, `"result":"passed"`, "Latest driver annotation", "Approved findings 2 and 5", "worker send approved findings, then require re-review"} {
		if !strings.Contains(strings.ToLower(brief), strings.ToLower(want)) {
			t.Fatalf("brief missing %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "runner.log") {
		t.Fatal("brief injected a raw runner log")
	}
}
