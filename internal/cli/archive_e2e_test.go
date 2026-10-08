package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestCLITaskArchiveE2E(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "shephrd")
	binDir := filepath.Join(root, "bin")
	databasePath := filepath.Join(root, "state.db")
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "config.toml")
	markerPath := filepath.Join(root, "treehouse-called")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = projectRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Shephrd E2E binary: %s: %v", output, err)
	}
	treehouse := "#!/bin/sh\nprintf '%s\\n' called > \"$SHEPHRD_E2E_TREEHOUSE_MARKER\"\nexit 99\n"
	if err := os.WriteFile(filepath.Join(binDir, "treehouse"), []byte(treehouse), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := fmt.Sprintf("database_path = %q\ndata_dir = %q\n", databasePath, dataDir)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(root, "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:e2e", RepoID: repo.ID, FeatureKey: "queued", Objective: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	terminalTask, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:e2e", RepoID: repo.ID, FeatureKey: "terminal", Objective: "terminal", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(terminalTask.ID, "pi", "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", filepath.Join(root, "worktree"), "lease-e2e", "shephrd/archive-e2e"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"finish"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddOutboundForRun(terminalTask.ID, attempt.ID, generation, "assign", "assignment"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetRunnerForRun(attempt.ID, generation, os.Getpid(), true); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "complete", NextSteps: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "complete", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "complete", Artifact: "report:/tmp/e2e-report.md"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "SHEPHRD_CONFIG="+configPath, "SHEPHRD_E2E_TREEHOUSE_MARKER="+markerPath, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := func(args ...string) []byte {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = environment
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("shephrd %v: %s: %v", args, stderr.String(), err)
		}
		return stdout.Bytes()
	}
	runFailure := func(args ...string) string {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = environment
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err == nil {
			t.Fatalf("shephrd %v unexpectedly succeeded: %s", args, stdout.String())
		}
		return stderr.String()
	}
	if output := runFailure("task", "archive", queued.ID, "--json"); !strings.Contains(output, "only terminal tasks can be archived") {
		t.Fatalf("nonterminal archive error = %s", output)
	}
	if output := runFailure("task", "archive", terminalTask.ID, "--json"); !strings.Contains(output, "has a live worker") {
		t.Fatalf("live archive error = %s", output)
	}
	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.FinishRunnerForRun(attempt.ID, generation, 0, ""); err != nil {
		t.Fatal(err)
	}
	before, err := state.Detail(terminalTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	var archived model.Task
	if err := json.Unmarshal(run("task", "archive", terminalTask.ID, "--json"), &archived); err != nil {
		t.Fatal(err)
	}
	if archived.ArchivedAt == nil || archived.Status != "done" {
		t.Fatalf("archive result = %+v", archived)
	}
	var visible []model.Task
	if err := json.Unmarshal(run("task", "list", "--json"), &visible); err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].ID != queued.ID {
		t.Fatalf("default task list = %+v", visible)
	}
	var all []model.Task
	if err := json.Unmarshal(run("task", "list", "--include-archived", "--json"), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[1].ID != terminalTask.ID || all[1].ArchivedAt == nil {
		t.Fatalf("include-archived task list = %+v", all)
	}
	var after model.TaskDetail
	if err := json.Unmarshal(run("task", "inspect", terminalTask.ID, "--json"), &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Attempts, after.Attempts) || !reflect.DeepEqual(before.Messages, after.Messages) || !reflect.DeepEqual(before.Checkpoints, after.Checkpoints) || !reflect.DeepEqual(before.Notifications, after.Notifications) {
		t.Fatalf("archive changed retained history:\nbefore=%+v\nafter=%+v", before, after)
	}
	if len(after.Attempts) != 1 || after.Attempts[0].WorktreePath != filepath.Join(root, "worktree") || after.Attempts[0].LeaseID != "" || after.Attempts[0].ReleaseState != "held" || after.Attempts[0].ReleasedAt != nil {
		t.Fatalf("archive changed attempt or worktree state: %+v", after.Attempts)
	}
	var repeated model.Task
	if err := json.Unmarshal(run("task", "archive", terminalTask.ID, "--json"), &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.ArchivedAt == nil || !repeated.ArchivedAt.Equal(*archived.ArchivedAt) || !repeated.UpdatedAt.Equal(archived.UpdatedAt) {
		t.Fatalf("repeated archive = %+v, first = %+v", repeated, archived)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("archive invoked Treehouse: %v", err)
	}
}
