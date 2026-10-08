package control

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
)

func TestRealCmuxAgentReleaseMatrixIsOptIn(t *testing.T) {
	if os.Getenv("SHEPHRD_REAL_CMUX_AGENTS_E2E") != "1" {
		t.Skip("set SHEPHRD_REAL_CMUX_AGENTS_E2E=1 to run real Pi, Claude, and Codex cmux workers")
	}
	parentClient := terminal.NewCmux(os.Getenv("CMUX_SOCKET_PATH"))
	parent, err := parentClient.ValidateParent()
	if err != nil {
		t.Fatal(err)
	}
	parentEndpoint := terminal.Endpoint{Backend: parent.Backend, SocketPath: parent.SocketPath, WindowID: parent.WindowID, WorkspaceID: parent.WorkspaceID, PaneID: parent.PaneID, SurfaceID: parent.SurfaceID}
	for _, harness := range []string{"pi", "claude-code", "codex"} {
		t.Run(harness, func(t *testing.T) {
			cmuxRealAgentFlow(t, parentClient, parentEndpoint, harness)
		})
	}
}

func cmuxRealAgentFlow(t *testing.T, parentClient terminal.CmuxClient, parentEndpoint terminal.Endpoint, harness string) {
	t.Helper()
	command := map[string]string{"pi": "pi", "claude-code": "claude", "codex": "codex"}[harness]
	if _, err := exec.LookPath(command); err != nil {
		t.Fatalf("%s is unavailable: %v", command, err)
	}
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repoPath, "init", "-b", "main")
	runLocalGit(t, repoPath, "config", "user.name", "Cmux Release Gate")
	runLocalGit(t, repoPath, "config", "user.email", "cmux-release@example.com")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("isolated cmux release gate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repoPath, "add", "README.md")
	runLocalGit(t, repoPath, "commit", "-m", "base")
	binary := filepath.Join(root, "shephrd")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build E2E Shephrd: %s: %v", output, err)
	}
	databasePath := filepath.Join(root, "state.db")
	dataDir := filepath.Join(root, "data")
	worktreeRoot := filepath.Join(root, "worktrees")
	configPath := filepath.Join(root, "config.toml")
	configBody := fmt.Sprintf("default_harness = %q\nworker_runtime = \"headless\"\ndatabase_path = %q\ndata_dir = %q\nworktree_root = %q\n", harness, databasePath, dataDir, worktreeRoot)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "cmux-" + strings.ReplaceAll(harness, "-", ""), Path: repoPath, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	objective := fmt.Sprintf("In this disposable repository, create %s-release.txt, commit it, then emit a checkpoint and question asking for the exact text release-approved. Do not emit done before that follow-up. After receiving it, emit a fresh checkpoint and done with the exact current branch artifact.", harness)
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:cmux-release", RepoID: repo.ID, FeatureKey: "real-" + strings.ReplaceAll(harness, "-", ""), Objective: objective, AcceptanceCriteria: "The committed marker exists and completion occurs only after release-approved.", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_EXECUTABLE", binary)
	t.Setenv("SHEPHRD_CONFIG", configPath)
	service := New(config.Config{DefaultHarness: harness, WorkerRuntime: "headless", DatabasePath: databasePath, DataDir: dataDir, WorktreeRoot: worktreeRoot}, state)
	spawned, err := service.SpawnWithModelSelection(task.ID, harness, "", false, "cmux")
	if err != nil {
		t.Fatal(err)
	}
	attempt := spawned.Attempt
	defer func() {
		current, currentErr := state.Task(task.ID)
		if currentErr == nil && !isTerminal(current.Status) {
			_ = service.Stop(task.ID, "cmux release fixture cleanup", false)
		}
		_ = parentClient.Focus(parentEndpoint)
	}()
	if attempt.TerminalEndpoint == nil || attempt.TerminalEndpoint.Backend != "cmux" {
		t.Fatalf("endpoint = %+v", attempt.TerminalEndpoint)
	}
	if _, err := service.Peek(task.ID, 80); err != nil {
		t.Fatal(err)
	}
	if err := service.Focus(task.ID); err != nil {
		t.Fatal(err)
	}
	if harness != "claude-code" {
		if err := parentClient.Focus(parentEndpoint); err != nil {
			t.Fatal(err)
		}
	}
	waitForCmuxTaskState(t, service, state, task.ID, model.TaskStatusWaiting, 5*time.Minute)
	if err := parentClient.Focus(parentEndpoint); err != nil {
		t.Fatal(err)
	}
	before, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(task.ID, "release-approved"); err != nil {
		t.Fatal(err)
	}
	waitForCmuxTaskState(t, service, state, task.ID, model.TaskStatusDone, 5*time.Minute)
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RunGeneration != before.RunGeneration+1 || current.ArtifactRef != "branch:"+stored.Branch {
		t.Fatalf("task = %+v, attempt = %+v", current, stored)
	}
	if _, err := os.Stat(filepath.Join(stored.WorktreePath, harness+"-release.txt")); err != nil {
		t.Fatal(err)
	}
}

func waitForCmuxTaskState(t *testing.T, service Service, state *store.Store, taskID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, taskErr := state.Task(taskID)
		if taskErr == nil && task.Status != want && isTerminal(task.Status) {
			attempt, _ := state.Attempt(task.CurrentAttemptID)
			messages, _ := state.Messages(taskID)
			t.Fatalf("task %s reached %s instead of %s: failure=%q messages=%+v", taskID, task.Status, want, attempt.FailureReason, messages)
		}
		if taskErr == nil && task.Status == want {
			if want != model.TaskStatusWaiting {
				return
			}
			attempt, attemptErr := state.Attempt(task.CurrentAttemptID)
			present, endpointErr := service.EndpointStatus(taskID)
			if attemptErr == nil && endpointErr == nil && attempt.RunnerPID == 0 && !present {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	task, _ := state.Task(taskID)
	t.Fatalf("task %s did not reach %s, current = %+v", taskID, want, task)
}
