package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestCLIExplicitCrossRepositoryReportHandoffE2E(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	databasePath := filepath.Join(root, "state.db")
	configPath := filepath.Join(root, "config.toml")
	binary := filepath.Join(root, "shephrd")
	binDir := filepath.Join(root, "bin")
	producerRoot := filepath.Join(root, "producer")
	targetRoot := filepath.Join(root, "target")
	for _, path := range []string{binDir, producerRoot, targetRoot} {
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
	handlerExecutable := filepath.Join(root, "report-handler")
	build = exec.Command("go", "build", "-o", handlerExecutable, "./internal/lifecycle/testdata/reporthandler")
	build.Dir = projectRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build report handler E2E fixture: %s: %v", output, err)
	}
	if err := os.Chmod(handlerExecutable, 0o755); err != nil {
		t.Fatal(err)
	}
	handlerBody, err := os.ReadFile(handlerExecutable)
	if err != nil {
		t.Fatal(err)
	}
	handlerDigest := sha256.Sum256(handlerBody)
	memoryDir := filepath.Join(root, "memory")
	for _, repoRoot := range []string{producerRoot, targetRoot} {
		command := exec.Command("git", "init", "-b", "main")
		command.Dir = repoRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %s: %v", repoRoot, output, err)
		}
		if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte(filepath.Base(repoRoot)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		command = exec.Command("git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "add", "README.md")
		command.Dir = repoRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git add %s: %s: %v", repoRoot, output, err)
		}
		command = exec.Command("git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
		command.Dir = repoRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git commit %s: %s: %v", repoRoot, output, err)
		}
	}
	pi := `#!/bin/bash
set -eu
prompt="${!#}"
if [[ "${SHEPHRD_E2E_MODE:-}" == target ]]; then
  printf '%s' "$prompt" > "$SHEPHRD_E2E_CAPTURE"
fi
printf '%s\n' '{"type":"session","id":"native-session"}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"E2E checkpoint\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"E2E checkpoint\",\"completed\":[],\"next_steps\":[\"finish\"],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>"}],"stopReason":"stop"}}'
if [[ "${SHEPHRD_E2E_MODE:-}" == producer ]]; then
  SHEPHRD_E2E_REPORT=$(printf '%s\n' "$prompt" | sed -n 's/Write the investigation report to \(.*\) and use artifact.*/\1/p')
  mkdir -p "$(dirname "$SHEPHRD_E2E_REPORT")"
  printf '%s\n' 'CLI_CROSS_REPO_SENTINEL' 'Use strategy OMEGA.' > "$SHEPHRD_E2E_REPORT"
  printf '%s\n' "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"<shephrd-event>{\\\"type\\\":\\\"done\\\",\\\"payload\\\":\\\"report complete\\\",\\\"artifact\\\":\\\"report:$SHEPHRD_E2E_REPORT\\\"}</shephrd-event>\"}],\"stopReason\":\"stop\"}}"
else
  printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"blocked\",\"payload\":\"captured implementation brief\"}</shephrd-event>"}],"stopReason":"stop"}}'
fi
printf '%s\n' '{"type":"agent_end"}'
`
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(pi), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := fmt.Sprintf(`default_harness = "pi"
worker_runtime = "headless"
database_path = %q
data_dir = %q

[wake]
enabled = true
default_batch = 10
max_batch = 20
claim_ttl = "5m"
claim_ttl_min = "30s"
claim_ttl_max = "30m"
driver_id = ""

[notifications]
enabled = false
details = false
task_per_minute = 2
global_per_minute = 10

[pi_watcher]
enabled = false
poll_min = "1s"
poll_max = "15s"

[[lifecycle_handlers.report_accepted]]
name = "memory"
extension_id = "fixture.report-memory"
command = [%q, %q, "success"]
sha256 = %q
`, databasePath, dataDir, handlerExecutable, memoryDir, hex.EncodeToString(handlerDigest[:]))
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	// SHEPHRD_EXECUTABLE is pinned to the test binary so detached runners
	// never fall back to an installed binary whose migration registry can
	// refuse the test database.
	environment := append(os.Environ(), "SHEPHRD_CONFIG="+configPath, "SHEPHRD_EXECUTABLE="+binary, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "PI_SESSION_ID=e2e-driver", "SHEPHRD_PI_WATCHER_ENABLED=0")
	run := func(extraEnvironment []string, args ...string) []byte {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = append(environment, extraEnvironment...)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("shephrd %v: %s: %v", args, stderr.String(), err)
		}
		return stdout.Bytes()
	}
	run(nil, "repo", "add", producerRoot, "--name", "producer", "--json")
	run(nil, "repo", "add", targetRoot, "--name", "target", "--json")
	var producer model.Task
	if err := json.Unmarshal(run(nil, "task", "create", "--title", "Test task", "--repo", "producer", "--feature", "investigate", "--deliverable", "report", "Investigate", "--json"), &producer); err != nil {
		t.Fatal(err)
	}
	run([]string{"SHEPHRD_E2E_MODE=producer"}, "worker", "spawn", producer.ID, "--harness", "pi", "--json")
	waitForCLITask(t, databasePath, producer.ID, func(task model.Task) bool { return task.Status == "done" && task.Landed })
	waitForCLIReportLifecycle(t, databasePath, producer.ID)
	var producerDetail model.TaskDetail
	if err := json.Unmarshal(run(nil, "task", "inspect", producer.ID, "--json"), &producerDetail); err != nil {
		t.Fatal(err)
	}
	if len(producerDetail.ReportLifecycleInvocations) != 1 || producerDetail.ReportLifecycleInvocations[0].EventName != "report.accepted" || producerDetail.ReportLifecycleInvocations[0].EventVersion != 1 || producerDetail.ReportLifecycleInvocations[0].State != "succeeded" || producerDetail.ReportLifecycleInvocations[0].Annotation == "" || producerDetail.ReportLifecycleInvocations[0].ReceiptSystem != "fixture-memory" {
		t.Fatalf("report lifecycle invocation = %+v", producerDetail.ReportLifecycleInvocations)
	}
	memoryEntries, err := os.ReadDir(memoryDir)
	if err != nil || len(memoryEntries) != 1 {
		t.Fatalf("memory entries=%v err=%v", memoryEntries, err)
	}
	if err := os.WriteFile(filepath.Join(producerRoot, "producer-only.txt"), []byte("must not transfer"), 0o600); err != nil {
		t.Fatal(err)
	}

	var target model.Task
	if err := json.Unmarshal(run(nil, "task", "create", "--title", "Test task", "--repo", "target", "--feature", "implement", "--with-report-from", producer.ID, "Implement", "--json"), &target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimPrefix(producerDetail.Task.ArtifactRef, "report:"), []byte("MUTATED_AFTER_ATTACHMENT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var detail model.TaskDetail
	if err := json.Unmarshal(run(nil, "task", "inspect", target.ID, "--json"), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Inputs) != 1 || detail.Inputs[0].Position != 1 || detail.Inputs[0].ProducerTaskID != producer.ID || detail.Inputs[0].AttachedByDriverID != "driver:pi:e2e-driver" || detail.Inputs[0].SHA256 == "" || detail.Inputs[0].SizeBytes == 0 {
		t.Fatalf("task inspect report input = %+v", detail.Inputs)
	}
	capturePath := filepath.Join(root, "target-prompt.md")
	run([]string{"SHEPHRD_E2E_MODE=target", "SHEPHRD_E2E_CAPTURE=" + capturePath}, "worker", "spawn", target.ID, "--harness", "pi", "--json")
	waitForCLITask(t, databasePath, target.ID, func(task model.Task) bool { return task.Status == "blocked" })
	prompt, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(prompt)
	for _, want := range []string{"CLI_CROSS_REPO_SENTINEL", "Use strategy OMEGA.", producer.ID, detail.Inputs[0].ProducerAttemptID,
		fmt.Sprint(detail.Inputs[0].DoneMessageID), detail.Inputs[0].ArtifactID, detail.Inputs[0].SHA256, "driver:pi:e2e-driver"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("captured target prompt missing %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "MUTATED_AFTER_ATTACHMENT") {
		t.Fatalf("captured target prompt used mutable producer path:\n%s", brief)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "producer-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("producer file was copied into target repository: %v", err)
	}
}

func waitForCLIReportLifecycle(t *testing.T, databasePath, taskID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state, err := store.Open(databasePath)
		if err == nil {
			invocations, invocationErr := state.ReportLifecycleInvocations(taskID)
			state.Close()
			if invocationErr == nil && len(invocations) == 1 && invocations[0].State != "pending" && invocations[0].State != "invoking" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("report lifecycle handler did not reach a visible outcome")
}

func waitForCLITask(t *testing.T, databasePath, taskID string, done func(model.Task) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state, err := store.Open(databasePath)
		if err == nil {
			task, taskErr := state.Task(taskID)
			state.Close()
			if taskErr == nil && done(task) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not reach expected state")
}
