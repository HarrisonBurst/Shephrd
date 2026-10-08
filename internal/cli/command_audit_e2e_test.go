package cli

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/repository/discovery"
	"shephrd/internal/store"
)

func TestRenamedCommandAuditContractsE2E(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(root, "projects")
	knownPath := filepath.Join(workspaceRoot, "known")
	discoveredPath := filepath.Join(workspaceRoot, "discovered")
	for _, path := range []string{knownPath, discoveredPath} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("git", "init", "-b", "main")
		command.Dir = path
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %s: %v", path, output, err)
		}
		command = exec.Command("git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base")
		command.Dir = path
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git commit %s: %s: %v", path, output, err)
		}
	}
	databasePath := filepath.Join(root, "state.db")
	configPath := filepath.Join(root, "config.toml")
	scannerPath := filepath.Join(root, "shephrd-repository-scanner")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", scannerPath, "./cmd/shephrd-repository-scanner")
	build.Dir = filepath.Clean(filepath.Join(workingDirectory, "../.."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build repository scanner: %s: %v", output, err)
	}
	scannerBody, err := os.ReadFile(scannerPath)
	if err != nil {
		t.Fatal(err)
	}
	scannerDigest := sha256.Sum256(scannerBody)
	configBody := fmt.Sprintf("database_path = %q\ndata_dir = %q\nworktree_root = %q\n\n[repository_discovery]\nroots = [%q]\ncommand = [%q]\nsha256 = %q\n", databasePath, filepath.Join(root, "data"), filepath.Join(root, "worktrees"), workspaceRoot, scannerPath, hex.EncodeToString(scannerDigest[:]))
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", configPath)
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "known", Path: knownPath, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "queued-stop", Objective: "stop before spawn"})
	if err != nil {
		t.Fatal(err)
	}
	workspaceTask, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "release", Objective: "release no workspace"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(workspaceTask.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"inspect the checkpoint"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: 1, Summary: "operator checkpoint", Completed: []string{"created fixture"}, NextSteps: []string{"inspect the checkpoint"}, Decisions: []model.Decision{{Decision: "render structured fields", Reason: "text parity"}}, ChangedPaths: []string{"internal/example.go"}, Checks: []model.Check{{Command: "go test ./...", Result: "passed"}}, Blockers: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "operator checkpoint", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if err := state.BeginNativeAllocation(attempt.ID, filepath.Join(root, "missing-workspace")); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkNoWorkspace(attempt.ID, "allocation ended before a workspace existed"); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) ([]byte, error) {
		t.Helper()
		command := New()
		var stdout, stderr bytes.Buffer
		command.SetOut(&stdout)
		command.SetErr(&stderr)
		command.SetArgs(args)
		err := command.Execute()
		if err != nil {
			return stderr.Bytes(), err
		}
		return stdout.Bytes(), nil
	}
	realPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	body, err := run("repo", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var repos []model.Repo
	if err := json.Unmarshal(body, &repos); err != nil || len(repos) != 1 || repos[0].ID != repo.ID {
		t.Fatalf("pure repo list=%+v err=%v body=%q", repos, err, body)
	}
	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := state.Repos()
	state.Close()
	if err != nil || len(registered) != 1 {
		t.Fatalf("repo list mutated registry: %+v err=%v", registered, err)
	}
	body, err = run("task", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var tasks []model.Task
	if err := json.Unmarshal(body, &tasks); err != nil || len(tasks) != 2 {
		t.Fatalf("task inventory=%+v err=%v", tasks, err)
	}
	body, err = run("task", "inspect", workspaceTask.ID)
	if err != nil || !bytes.Contains(body, []byte("operator checkpoint")) || !bytes.Contains(body, []byte("next steps: inspect the checkpoint")) || !bytes.Contains(body, []byte("decision: render structured fields (reason: text parity)")) || !bytes.Contains(body, []byte("check: go test ./...: passed")) {
		t.Fatalf("task inspect error=%v body=%q", err, body)
	}
	body, err = run("task", "obligations", "--all-drivers", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var obligations model.AttentionSnapshot
	if err := json.Unmarshal(body, &obligations); err != nil || obligations.SchemaVersion != model.AttentionSchemaVersion {
		t.Fatalf("task obligations=%+v err=%v", obligations, err)
	}
	body, err = run("worker", "stop", queued.ID, "--discard", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var stopped workerStopResponse
	if err := json.Unmarshal(body, &stopped); err != nil || stopped.SchemaVersion != 1 || !stopped.Stopped || stopped.Discarded || !stopped.NoWorkspace {
		t.Fatalf("queued stop=%+v err=%v body=%q", stopped, err, body)
	}
	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	stoppedTask, taskErr := state.Task(queued.ID)
	attempts, attemptsErr := state.Attempts(queued.ID)
	state.Close()
	if taskErr != nil || attemptsErr != nil || stoppedTask.Status != model.TaskStatusStopped || len(attempts) != 0 {
		t.Fatalf("queued stop state task=%+v attempts=%+v taskErr=%v attemptsErr=%v", stoppedTask, attempts, taskErr, attemptsErr)
	}
	body, err = run("workspace", "release", workspaceTask.ID, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var released workspaceReleaseResponse
	if err := json.Unmarshal(body, &released); err != nil || released.SchemaVersion != 1 || released.AttemptID != attempt.ID || !released.Released {
		t.Fatalf("workspace release=%+v err=%v body=%q", released, err, body)
	}
	body, err = run("workspace", "reconcile")
	if err != nil || !bytes.Contains(body, []byte("recovery state mutated")) {
		t.Fatalf("workspace reconcile error=%v body=%q", err, body)
	}

	t.Setenv("PATH", realPath)
	body, err = run("repo", "scan", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var candidates []discovery.Candidate
	if err := json.Unmarshal(body, &candidates); err != nil || len(candidates) != 2 {
		t.Fatalf("repo scan=%+v err=%v body=%q", candidates, err, body)
	}
	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	registered, err = state.Repos()
	state.Close()
	if err != nil || len(registered) != 1 {
		t.Fatalf("repo scan mutated registry: %+v err=%v", registered, err)
	}
	body, err = run("repo", "add", discoveredPath, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var discovered model.Repo
	if err := json.Unmarshal(body, &discovered); err != nil || discovered.Path != candidates[1].Path && discovered.Path != candidates[0].Path {
		t.Fatalf("repo add=%+v err=%v body=%q", discovered, err, body)
	}

	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	registered, err = state.Repos()
	if err != nil || len(registered) != 2 {
		t.Fatalf("explicit repo add registry: %+v err=%v", registered, err)
	}
	codeMismatch, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "legacy-code-mismatch", Objective: "reject report", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	reportMismatch, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "legacy-report-mismatch", Objective: "reject branch", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	codeAttempt, err := state.BeginAttempt(codeMismatch.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	reportAttempt, err := state.BeginAttempt(reportMismatch.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET status='done', claimed_done=1, artifact_ref='report:/tmp/wrong.md' WHERE id=?`, codeMismatch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE attempts SET status='done' WHERE id=?`, codeAttempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET status='done', claimed_done=1, artifact_ref='branch:shephrd/wrong' WHERE id=?`, reportMismatch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE attempts SET status='done' WHERE id=?`, reportAttempt.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, taskID := range []string{codeMismatch.ID, reportMismatch.ID} {
		if _, err := run("task", "verify-delivery", taskID, "--json"); err == nil || !strings.Contains(err.Error(), "task artifact") {
			t.Fatalf("deliverable mismatch verification for %s = %v", taskID, err)
		}
	}

	for _, args := range [][]string{
		{"repo", "ls"}, {"task", "ls"}, {"task", "show", workspaceTask.ID}, {"task", "attention"}, {"task", "verify", workspaceTask.ID},
		{"send", workspaceTask.ID, "answer"}, {"stop", workspaceTask.ID}, {"retry", workspaceTask.ID}, {"reconcile"}, {"worker", "release", workspaceTask.ID},
		{"task-list", "ls"}, {"task-list", "dispatch", "list", "item"}, {"decision", "ls", workspaceTask.ID}, {"memory", "ls", repo.ID},
	} {
		if output, err := run(args...); err == nil {
			t.Fatalf("removed command %s succeeded: %s", strings.Join(args, " "), output)
		}
	}
}
