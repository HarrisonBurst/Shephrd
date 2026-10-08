package cli

import (
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

// TestNativeBackendSpawnsIsolatedWorktreesForCodeAndReportWorkers exercises
// the full CLI path: concurrent code and report workers each get an
// attempt-owned native Git worktree created from the exact registered
// default-branch head, with no Treehouse binary present at all, and the
// existing spawn UX is unchanged.
func TestNativeBackendSpawnsIsolatedWorktreesForCodeAndReportWorkers(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	repoPath := filepath.Join(root, "repo")
	worktreeRoot := filepath.Join(root, "worktrees")
	for _, path := range []string{binDir, repoPath, worktreeRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runNativeBackendSpawnE2E(t, root, binDir, repoPath, worktreeRoot)
}

func runNativeBackendSpawnE2E(t *testing.T, root, binDir, repoPath, worktreeRoot string) {
	t.Helper()
	git := func(dir string, args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	git(repoPath, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(repoPath, "add", "README.md")
	git(repoPath, "commit", "-m", "base")
	head := git(repoPath, "rev-parse", "HEAD")
	// Registered-root dirt that must never reach a worker checkout.
	if err := os.WriteFile(filepath.Join(repoPath, "untracked.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	piLog := filepath.Join(root, "pi.log")
	writeExecutable(t, filepath.Join(binDir, "pi"), `#!/bin/sh
printf '%s\n' "$PWD" >> "$SHEPHRD_TEST_PI_LOG"
printf '%s\n' '{"type":"session","id":"native-session"}'
case "$*" in
  *SHEPHRD_PROTOCOL_REPAIR*)
    case "${SHEPHRD_TEST_STRICT:-}" in
      code) printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"repair\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"repair\",\"completed\":[],\"next_steps\":[],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>\n<shephrd-event>{\"type\":\"done\",\"payload\":\"wrong report\",\"artifact\":\"report:/tmp/wrong.md\"}</shephrd-event>"}],"stopReason":"stop"}}' ;;
      report) printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"repair\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"repair\",\"completed\":[],\"next_steps\":[],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>\n<shephrd-event>{\"type\":\"done\",\"payload\":\"wrong PR\",\"artifact\":\"https://github.com/acme/demo/pull/1\"}</shephrd-event>"}],"stopReason":"stop"}}' ;;
    esac
    printf '%s\n' '{"type":"agent_end"}'
    exit 0
    ;;
esac
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"ready\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"ready\",\"completed\":[],\"next_steps\":[\"wait\"],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>"}],"stopReason":"stop"}}'
case "${SHEPHRD_TEST_STRICT:-}" in
  code) printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"done\",\"payload\":\"wrong report\",\"artifact\":\"report:/tmp/wrong.md\"}</shephrd-event>"}],"stopReason":"stop"}}' ;;
  report) printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"done\",\"payload\":\"wrong PR\",\"artifact\":\"https://github.com/acme/demo/pull/1\"}</shephrd-event>"}],"stopReason":"stop"}}' ;;
  *) printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"question\",\"payload\":\"wait\"}</shephrd-event>"}],"stopReason":"stop"}}' ;;
esac
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
`, databasePath, dataDir, worktreeRoot)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: repoPath, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	codeTask, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "native-code", Objective: "code work", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	reportTask, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "native-report", Objective: "report work", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	strictCodeTask, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "strict-code", Objective: "reject report", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	strictReportTask, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "strict-report", Objective: "reject code", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
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

	// PATH deliberately contains the fake pi and real git, but no treehouse:
	// the native backend must never need it.
	environment := append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SHEPHRD_CONFIG="+configPath,
		"SHEPHRD_EXECUTABLE="+binary,
		"SHEPHRD_TEST_PI_LOG="+piLog,
	)
	for _, task := range []model.Task{codeTask, reportTask} {
		command := exec.Command(binary, "worker", "spawn", task.ID, "--harness", "pi", "--json")
		command.Env = environment
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("spawn %s: %s: %v", task.ID, output, err)
		}
	}
	for _, strict := range []struct {
		task model.Task
		mode string
	}{{strictCodeTask, "code"}, {strictReportTask, "report"}} {
		command := exec.Command(binary, "worker", "spawn", strict.task.ID, "--harness", "pi", "--json")
		command.Env = append(environment, "SHEPHRD_TEST_STRICT="+strict.mode)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("spawn strict %s: %s: %v", strict.task.ID, output, err)
		}
	}

	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		settled := true
		for _, task := range []model.Task{codeTask, reportTask} {
			current, err := state.Task(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			settled = settled && current.Status == "waiting" && !current.ProcessAlive
		}
		for _, task := range []model.Task{strictCodeTask, strictReportTask} {
			current, err := state.Task(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			settled = settled && current.Status == "blocked" && !current.ProcessAlive
		}
		if settled {
			break
		}
		if time.Now().After(deadline) {
			for _, task := range []model.Task{codeTask, reportTask} {
				detail, _ := state.Detail(task.ID)
				t.Logf("task detail: %+v", detail)
			}
			t.Fatal("native workers did not reach their expected terminal state")
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, task := range []model.Task{strictCodeTask, strictReportTask} {
		current, err := state.Task(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		messages, err := state.Messages(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.ClaimedDone || current.ArtifactRef != "" || len(messages) == 0 || !strings.Contains(messages[len(messages)-1].Payload, "creation-time deliverable contract") {
			t.Fatalf("strict deliverable task=%+v messages=%+v", current, messages)
		}
	}
	paths := map[string]bool{}
	for _, task := range []model.Task{codeTask, reportTask} {
		current, err := state.Task(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := state.Attempt(current.CurrentAttemptID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt.WorkspaceBackend != model.WorkspaceBackendNative || attempt.WorkspaceState != model.WorkspaceStateHeld || attempt.LeaseID != "" {
			t.Fatalf("task %s attempt workspace = %+v", task.ID, attempt)
		}
		wantPath := filepath.Join(worktreeRoot, repo.ID, attempt.ID, "demo")
		if attempt.WorktreePath != wantPath {
			t.Fatalf("task %s worktree = %q, want %q", task.ID, attempt.WorktreePath, wantPath)
		}
		paths[attempt.WorktreePath] = true
		if got := git(attempt.WorktreePath, "rev-parse", "HEAD"); got != head {
			t.Fatalf("task %s worktree HEAD = %q, want %q", task.ID, got, head)
		}
		if attempt.BaseCommit != head {
			t.Fatalf("task %s base commit = %q, want %q", task.ID, attempt.BaseCommit, head)
		}
		if _, err := os.Stat(filepath.Join(attempt.WorktreePath, "untracked.txt")); !os.IsNotExist(err) {
			t.Fatalf("registered-root dirt leaked into %s", attempt.WorktreePath)
		}
	}
	if len(paths) != 2 {
		t.Fatalf("code and report workers shared a worktree: %v", paths)
	}
	launches := lineCounts(t, piLog)
	if len(launches) != 4 {
		t.Fatalf("harness launch directories = %v", launches)
	}
	for path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		if launches[path] != 1 && launches[resolved] != 1 {
			t.Fatalf("worker did not run inside its native worktree: %v", launches)
		}
	}
	followUp := exec.Command(binary, "worker", "send", codeTask.ID, "continue", "--json")
	followUp.Env = environment
	if output, err := followUp.CombinedOutput(); err != nil {
		t.Fatalf("worker send: %s: %v", output, err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		current, err := state.Task(codeTask.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == model.TaskStatusWaiting && !current.ProcessAlive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker send did not return to waiting: %+v", current)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
