package cli

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestConcurrentWorkerSpawnProcessesWaitForDatabaseSetupAndLaunchOnce(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	repoPath := filepath.Join(root, "repo")
	worktrees := filepath.Join(root, "worktrees")
	for _, path := range []string{binDir, repoPath, worktrees} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runGitE2E(t, repoPath, "init", "-b", "main")
	runGitE2E(t, repoPath, "config", "user.name", "Test")
	runGitE2E(t, repoPath, "config", "user.email", "test@example.com")
	runGitE2E(t, repoPath, "commit", "--allow-empty", "-m", "base")
	piLog := filepath.Join(root, "pi.log")
	writeExecutable(t, filepath.Join(binDir, "pi"), `#!/bin/sh
basename "$(dirname "$PWD")" >> "$SHEPHRD_TEST_PI_LOG"
printf '%s\n' '{"type":"session","id":"native-session"}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"ready\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"ready\",\"completed\":[],\"next_steps\":[\"wait\"],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>"}],"stopReason":"stop"}}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"question\",\"payload\":\"wait\"}</shephrd-event>"}],"stopReason":"stop"}}'
printf '%s\n' '{"type":"agent_end"}'
`)

	databasePath := filepath.Join(root, "state", "shephrd.db")
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "config.toml")
	configBody := fmt.Sprintf(`default_harness = "pi"
worker_runtime = "headless"
database_path = %q
data_dir = %q
worktree_root = %q

[wake]
enabled = true
default_batch = 10
max_batch = 20
claim_ttl = "5m"
claim_ttl_min = "30s"
claim_ttl_max = "30m"
driver_id = "driver:test"

[notifications]
enabled = false
details = false
task_per_minute = 2
global_per_minute = 10

[pi_watcher]
enabled = false
poll_min = "1s"
poll_max = "15s"
`, databasePath, dataDir, worktrees)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "concurrent", Path: repoPath, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	tasks := make([]model.Task, 2)
	for index := range tasks {
		tasks[index], err = state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: fmt.Sprintf("spawn-%d", index), Objective: "wait for input", Deliverable: "report"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "shephrd")
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shephrd: %s: %v", output, err)
	}

	locker, err := sql.Open("sqlite", databasePath+"?_busy_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	lockConn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var lockMode string
	if err := lockConn.QueryRowContext(context.Background(), "PRAGMA locking_mode=EXCLUSIVE").Scan(&lockMode); err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}

	type invocation struct {
		command *exec.Cmd
		stdout  bytes.Buffer
		stderr  bytes.Buffer
	}
	invocations := make([]invocation, len(tasks))
	environment := append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SHEPHRD_CONFIG="+configPath,
		// Pin the runtime executable to the freshly built test binary: an
		// inherited SHEPHRD_EXECUTABLE would make detached runners use an
		// installed binary whose migration registry can refuse the test
		// database.
		"SHEPHRD_EXECUTABLE="+binary,
		"SHEPHRD_TEST_PI_LOG="+piLog,
	)
	for index, task := range tasks {
		command := exec.Command(binary, "worker", "spawn", task.ID, "--harness", "pi", "--json")
		command.Env = environment
		command.Stdout = &invocations[index].stdout
		command.Stderr = &invocations[index].stderr
		invocations[index].command = command
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := lockConn.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := lockConn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := locker.Close(); err != nil {
		t.Fatal(err)
	}
	for index := range invocations {
		if err := invocations[index].command.Wait(); err != nil {
			t.Fatalf("spawn %d: %v\nstdout: %s\nstderr: %s", index, err, invocations[index].stdout.String(), invocations[index].stderr.String())
		}
	}

	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		waiting := true
		for _, task := range tasks {
			current, err := state.Task(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			waiting = waiting && current.Status == "waiting" && !current.ProcessAlive
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			for _, task := range tasks {
				detail, _ := state.Detail(task.ID)
				t.Logf("task detail: %+v", detail)
			}
			t.Fatal("workers did not reach waiting state")
		}
		time.Sleep(10 * time.Millisecond)
	}

	launchCounts := lineCounts(t, piLog)
	for _, task := range tasks {
		attempts, err := state.Attempts(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(attempts) != 1 || attempts[0].Number != 1 || attempts[0].RunGeneration != 1 {
			t.Fatalf("task %s attempts = %+v", task.ID, attempts)
		}
		attempt := attempts[0]
		if attempt.WorkspaceBackend != model.WorkspaceBackendNative || attempt.LeaseID != "" {
			t.Fatalf("attempt %s workspace = %+v", attempt.ID, attempt)
		}
		if launchCounts[attempt.ID] != 1 {
			t.Fatalf("attempt %s launch count = %d", attempt.ID, launchCounts[attempt.ID])
		}
	}
	if len(launchCounts) != 2 {
		t.Fatalf("launch counts=%v", launchCounts)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func lineCounts(t *testing.T, path string) map[string]int {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		counts[line]++
	}
	return counts
}
