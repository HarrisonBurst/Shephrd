package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestCLISettlementWakeE2E(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	databasePath := filepath.Join(root, "state.db")
	configPath := filepath.Join(root, "config.toml")
	binary := filepath.Join(root, "shephrd")
	binDir := filepath.Join(root, "bin")
	repoRoot := filepath.Join(root, "repo")
	for _, path := range []string{binDir, repoRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
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
	command := exec.Command("git", "init", "-b", "main")
	command.Dir = repoRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", output, err)
	}
	for _, args := range [][]string{{"config", "user.name", "Test"}, {"config", "user.email", "test@example.com"}} {
		command = exec.Command("git", args...)
		command.Dir = repoRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("settlement wake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "initial"}} {
		command = exec.Command("git", args...)
		command.Dir = repoRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	harness := `#!/usr/bin/env python3
import json, os, subprocess, time
branch = subprocess.check_output(["git", "branch", "--show-current"], text=True).strip()
pid_path = os.environ.get("SHEPHRD_SETTLEMENT_HARNESS_PID", "")
if pid_path:
    open(pid_path, "w").write(str(os.getpid()))
checkpoint = '<shephrd-event>{"type":"checkpoint","payload":"settlement ready","checkpoint":{"schema_version":1,"summary":"settlement ready","completed":["result prepared"],"next_steps":[],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>'
done = '<shephrd-event>{"type":"done","payload":"settlement complete","artifact":"branch:' + branch + '"}</shephrd-event>'
print(json.dumps({"type":"session","id":"settlement-e2e-session"}), flush=True)
for text in (checkpoint, done):
    print(json.dumps({"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":text}],"stopReason":"stop"}}), flush=True)
print(json.dumps({"type":"agent_end"}), flush=True)
exit_path = os.environ.get("SHEPHRD_SETTLEMENT_EXIT", "")
while exit_path and not os.path.exists(exit_path):
    time.sleep(0.02)
`
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(harness), 0o700); err != nil {
		t.Fatal(err)
	}
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
driver_id = "driver:settlement-e2e"

[notifications]
enabled = false
details = false
task_per_minute = 2
global_per_minute = 10

[pi_watcher]
enabled = false
poll_min = "1s"
poll_max = "15s"
`, databasePath, dataDir, filepath.Join(root, "worktrees"))
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := compoundCLIEnvironment(os.Environ(), map[string]string{
		"PATH":               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PI_SESSION_ID":      "",
		"SHEPHRD_CONFIG":     configPath,
		"SHEPHRD_EXECUTABLE": binary,
	})
	run := func(extra map[string]string, args ...string) []byte {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = compoundCLIEnvironment(environment, extra)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("shephrd %v: %s: %v", args, stderr.String(), err)
		}
		return stdout.Bytes()
	}
	run(nil, "repo", "add", repoRoot, "--name", "settlement", "--json")
	create := func(t *testing.T, feature string) model.Task {
		t.Helper()
		var task model.Task
		if err := json.Unmarshal(run(nil, "task", "create", "--repo", "settlement", "--feature", feature, "--title", "Settlement "+feature, "--deliverable", "code", "--driver-id", "driver:settlement-e2e", "Exercise settlement wake", "--json"), &task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	spawn := func(t *testing.T, task model.Task, exitPath, pidPath string) model.Attempt {
		t.Helper()
		extra := map[string]string{"SHEPHRD_SETTLEMENT_EXIT": exitPath, "SHEPHRD_SETTLEMENT_HARNESS_PID": pidPath}
		run(extra, "worker", "spawn", task.ID, "--harness", "pi", "--runtime", "headless", "--json")
		waitForCLITask(t, databasePath, task.ID, func(current model.Task) bool { return current.Status == model.TaskStatusDone && current.ProcessAlive })
		state, err := store.Open(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		current, err := state.Task(task.ID)
		if err != nil {
			state.Close()
			t.Fatal(err)
		}
		attempt, err := state.Attempt(current.CurrentAttemptID)
		state.Close()
		if err != nil {
			t.Fatal(err)
		}
		return attempt
	}
	drain := func(t *testing.T, taskID, generation string) model.NotificationDrain {
		t.Helper()
		var result model.NotificationDrain
		if err := json.Unmarshal(run(nil, "wake", "drain", taskID, "--driver-id", "driver:settlement-e2e", "--driver-generation", generation, "--limit", "10", "--json"), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	ack := func(t *testing.T, notification model.DriverNotification) {
		t.Helper()
		run(nil, "wake", "ack", "--claim-token", notification.ClaimToken, "--handling-id", "handling:"+strings.ReplaceAll(notification.Kind, "_", "-"), "--json")
	}
	inspectNotifications := func(t *testing.T, taskID string) []model.DriverNotification {
		t.Helper()
		state, err := store.Open(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		defer state.Close()
		notifications, err := state.Notifications(taskID)
		if err != nil {
			t.Fatal(err)
		}
		return notifications
	}

	t.Run("linger then death and release silence", func(t *testing.T) {
		task := create(t, "linger")
		exitPath := filepath.Join(root, task.ID+"-exit")
		spawn(t, task, exitPath, "")
		original := drain(t, task.ID, "generation:linger")
		if len(original.Notifications) != 1 || original.Notifications[0].Kind != "done" {
			t.Fatalf("original drain = %+v", original)
		}
		ack(t, original.Notifications[0])
		if err := os.WriteFile(exitPath, []byte("exit"), 0o600); err != nil {
			t.Fatal(err)
		}
		waitForCLITask(t, databasePath, task.ID, func(current model.Task) bool { return !current.ProcessAlive })
		settled := drain(t, task.ID, "generation:linger")
		if len(settled.Notifications) != 1 || settled.Notifications[0].Kind != model.SettledMessageType || !strings.Contains(settled.Notifications[0].Payload, "process_exited") {
			t.Fatalf("settled drain = %+v", settled)
		}
		if len(inspectNotifications(t, task.ID)) != 2 {
			t.Fatalf("notifications = %+v", inspectNotifications(t, task.ID))
		}
		run(nil, "workspace", "release", task.ID, "--discard", "--json")
		stored := inspectNotifications(t, task.ID)
		if stored[1].State != model.NotificationSuperseded || stored[1].SupersedeReason != "attempt released" {
			t.Fatalf("released settled notification = %+v", stored[1])
		}
		if afterRelease := drain(t, task.ID, "generation:linger"); len(afterRelease.Notifications) != 0 {
			t.Fatalf("post-release drain = %+v", afterRelease)
		}
	})

	t.Run("death before ack keeps original wake", func(t *testing.T) {
		task := create(t, "death-first")
		exitPath := filepath.Join(root, task.ID+"-exit")
		spawn(t, task, exitPath, "")
		if err := os.WriteFile(exitPath, []byte("exit"), 0o600); err != nil {
			t.Fatal(err)
		}
		waitForCLITask(t, databasePath, task.ID, func(current model.Task) bool { return !current.ProcessAlive })
		original := drain(t, task.ID, "generation:death-first")
		if len(original.Notifications) != 1 || original.Notifications[0].Kind != "done" {
			t.Fatalf("death-first drain = %+v", original)
		}
		ack(t, original.Notifications[0])
		if final := drain(t, task.ID, "generation:death-first"); len(final.Notifications) != 0 || len(inspectNotifications(t, task.ID)) != 1 {
			t.Fatalf("final drain = %+v notifications = %+v", final, inspectNotifications(t, task.ID))
		}
	})

	t.Run("crash before finish is swept once", func(t *testing.T) {
		task := create(t, "crash")
		exitPath := filepath.Join(root, task.ID+"-never-exit")
		pidPath := filepath.Join(root, task.ID+"-harness-pid")
		attempt := spawn(t, task, exitPath, pidPath)
		waitForCLIPath(t, pidPath)
		harnessPIDBody, err := os.ReadFile(pidPath)
		if err != nil {
			t.Fatal(err)
		}
		var harnessPID int
		if _, err := fmt.Sscan(string(harnessPIDBody), &harnessPID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(-harnessPID, syscall.SIGKILL) })
		original := drain(t, task.ID, "generation:crash")
		if len(original.Notifications) != 1 || original.Notifications[0].Kind != "done" {
			t.Fatalf("crash original drain = %+v", original)
		}
		ack(t, original.Notifications[0])
		if err := syscall.Kill(attempt.RunnerPID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		var swept model.NotificationDrain
		for time.Now().Before(deadline) {
			swept = drain(t, task.ID, "generation:crash")
			if len(swept.Notifications) != 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if len(swept.Notifications) != 1 || swept.Notifications[0].Kind != model.SettledMessageType || !strings.Contains(swept.Notifications[0].Payload, "swept_dead") {
			t.Fatalf("swept drain = %+v", swept)
		}
		ack(t, swept.Notifications[0])
		if replay := drain(t, task.ID, "generation:crash"); len(replay.Notifications) != 0 || len(inspectNotifications(t, task.ID)) != 2 {
			t.Fatalf("replay drain = %+v notifications = %+v", replay, inspectNotifications(t, task.ID))
		}
	})
}
