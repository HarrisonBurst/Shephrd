package control

import (
	"crypto/sha256"
	"encoding/hex"
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

func TestRealCmuxMVPIsOptIn(t *testing.T) {
	if os.Getenv("SHEPHRD_REAL_CMUX_E2E") != "1" {
		t.Skip("set SHEPHRD_REAL_CMUX_E2E=1 to run the isolated real cmux MVP")
	}
	parentClient := terminal.NewCmux(os.Getenv("CMUX_SOCKET_PATH"))
	parent, err := parentClient.ValidateParent()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repoPath, "init", "-b", "main")
	runLocalGit(t, repoPath, "config", "user.name", "Cmux E2E")
	runLocalGit(t, repoPath, "config", "user.email", "cmux-e2e@example.com")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("cmux e2e\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repoPath, "add", "README.md")
	runLocalGit(t, repoPath, "commit", "-m", "base")
	binary := filepath.Join(root, "shephrd")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = repositoryRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build E2E Shephrd: %s: %v", output, err)
	}
	var extensionConfig *config.ExtensionConfig
	if os.Getenv("SHEPHRD_REAL_CMUX_EXTENSION_E2E") == "1" {
		extensionExecutable := filepath.Join(root, "shephrd-terminal-cmux")
		extensionBuild := exec.Command("go", "build", "-o", extensionExecutable, "./cmd/shephrd-terminal-cmux")
		extensionBuild.Dir = repositoryRoot
		if output, err := extensionBuild.CombinedOutput(); err != nil {
			t.Fatalf("build E2E cmux extension: %s: %v", output, err)
		}
		if err := os.Chmod(extensionExecutable, 0o755); err != nil {
			t.Fatal(err)
		}
		extensionBytes, err := os.ReadFile(extensionExecutable)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(extensionBytes)
		extensionConfig = &config.ExtensionConfig{Command: []string{extensionExecutable}, SHA256: hex.EncodeToString(digest[:])}
	}
	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeCodex := `#!/bin/sh
set -eu
printf '%s\n' '{"type":"thread.started","thread_id":"cmux-e2e-thread"}'
sleep 2
printf 'cmux e2e\n' > cmux-e2e.txt
git add cmux-e2e.txt
git -c user.name='Cmux E2E' -c user.email='cmux-e2e@example.com' commit -m 'cmux e2e' >/dev/null
branch=$(git branch --show-current)
python3 - "$branch" <<'PY'
import json, sys
checkpoint = '<shephrd-event>' + json.dumps({
    "type": "checkpoint",
    "payload": "cmux E2E checkpoint",
    "checkpoint": {
        "schema_version": 1,
        "summary": "cmux E2E checkpoint",
        "completed": ["committed isolated fixture"],
        "next_steps": ["emit done"],
        "decisions": [],
        "changed_paths": ["cmux-e2e.txt"],
        "checks": [],
        "blockers": []
    }
}, separators=(",", ":")) + '</shephrd-event>'
done = '<shephrd-event>' + json.dumps({
    "type": "done",
    "payload": "cmux E2E complete",
    "artifact": "branch:" + sys.argv[1]
}, separators=(",", ":")) + '</shephrd-event>'
for text in (checkpoint, done):
    print(json.dumps({"type": "item.completed", "item": {"type": "agent_message", "text": text}}, separators=(",", ":")))
print('{"type":"turn.completed"}')
PY
`
	if err := os.WriteFile(filepath.Join(fakeBin, "codex"), []byte(fakeCodex), 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "state.db")
	dataDir := filepath.Join(root, "data")
	worktreeRoot := filepath.Join(root, "worktrees")
	configPath := filepath.Join(root, "config.toml")
	configBody := fmt.Sprintf("default_harness = \"codex\"\nworker_runtime = \"headless\"\ndatabase_path = %q\ndata_dir = %q\nworktree_root = %q\n", databasePath, dataDir, worktreeRoot)
	if extensionConfig != nil {
		configBody += fmt.Sprintf("[terminal_extensions.cmux]\ncommand = [%q]\nsha256 = %q\n", extensionConfig.Command[0], extensionConfig.SHA256)
	}
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "cmux-e2e", Path: repoPath, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:cmux-e2e", RepoID: repo.ID, FeatureKey: "cmux-mvp", Objective: "prove the isolated real cmux MVP", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHEPHRD_EXECUTABLE", binary)
	t.Setenv("SHEPHRD_CONFIG", configPath)
	serviceConfig := config.Config{DefaultHarness: "codex", WorkerRuntime: "headless", DatabasePath: databasePath, DataDir: dataDir, WorktreeRoot: worktreeRoot}
	serviceConfig.TerminalExtensions.Cmux = extensionConfig
	service := New(serviceConfig, state)
	spawned, err := service.SpawnWithModelSelection(task.ID, "codex", "", false, "cmux")
	if err != nil {
		t.Fatal(err)
	}
	attempt := spawned.Attempt
	if attempt.TerminalEndpoint == nil || attempt.TerminalEndpoint.Backend != "cmux" || attempt.TerminalEndpoint.WindowID != parent.WindowID || attempt.TerminalEndpoint.WorkspaceID == parent.WorkspaceID || attempt.TerminalEndpoint.SurfaceID == parent.SurfaceID {
		t.Fatalf("attempt endpoint = %+v parent = %+v", attempt.TerminalEndpoint, parent)
	}
	endpoint := terminalEndpoint(*attempt.TerminalEndpoint)
	stateAtStart, err := parentClient.Inspect(endpoint)
	if err != nil || stateAtStart.Focused || stateAtStart.PaneCount != 1 || stateAtStart.SurfaceCount != 1 {
		t.Fatalf("start state = %+v, err = %v", stateAtStart, err)
	}
	marker := filepath.Join(attempt.WorktreePath, "queued-input-reached-shell")
	if err := parentClient.Start(endpoint, "touch "+shellQuote(marker)); err != nil {
		t.Fatal(err)
	}
	peek, err := service.Peek(task.ID, 100)
	if err != nil || !peek.EndpointPresent || !strings.Contains(peek.Output, "prove the isolated real cmux MVP") {
		t.Fatalf("peek = %+v, err = %v", peek, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		current, taskErr := state.Task(task.ID)
		present, endpointErr := service.EndpointStatus(task.ID)
		if taskErr == nil && endpointErr == nil && current.Status == model.TaskStatusDone && !present {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	present, endpointErr := service.EndpointStatus(task.ID)
	if current.Status != model.TaskStatusDone || !current.ClaimedDone || stored.RunnerPID != 0 || present || endpointErr != nil {
		t.Fatalf("task=%+v attempt=%+v endpoint_present=%t endpoint_err=%v", current, stored, present, endpointErr)
	}
	checkpoint, err := state.LatestCheckpoint(attempt.ID)
	if err != nil || checkpoint.Producer != "worker" || checkpoint.Summary != "cmux E2E checkpoint" {
		t.Fatalf("checkpoint = %+v, err = %v", checkpoint, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("queued input reached a later shell: %v", err)
	}
	if _, err := os.Stat(attempt.WorktreePath); err != nil {
		t.Fatalf("unlanded worktree was not retained: %v", err)
	}
	parentAfter, err := parentClient.ValidateParent()
	if err != nil || parentAfter.WindowID != parent.WindowID || parentAfter.WorkspaceID != parent.WorkspaceID || parentAfter.SurfaceID != parent.SurfaceID {
		t.Fatalf("parent after = %+v, err = %v", parentAfter, err)
	}
}
