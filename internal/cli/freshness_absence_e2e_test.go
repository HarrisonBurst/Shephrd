package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestCLIStaleSelfHostedBinaryHasNoLifecycleDiagnosticsE2E(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	root := t.TempDir()
	checkout := filepath.Join(root, "shephrd")
	copyFreshnessAbsenceProject(t, projectRoot, checkout)
	runFreshnessAbsenceSetup(t, checkout, "git", "init", "-b", "main")
	runFreshnessAbsenceSetup(t, checkout, "git", "add", ".")
	runFreshnessAbsenceSetup(t, checkout, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	binary := filepath.Join(checkout, "bin", "shephrd")
	runFreshnessAbsenceSetup(t, checkout, "go", "build", "-o", binary, "./cmd/shephrd")
	readme := filepath.Join(checkout, "README.md")
	fileHandle, err := os.OpenFile(readme, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileHandle.WriteString("\npost-build source change\n"); err != nil {
		fileHandle.Close()
		t.Fatal(err)
	}
	if err := fileHandle.Close(); err != nil {
		t.Fatal(err)
	}
	runFreshnessAbsenceSetup(t, checkout, "git", "add", "README.md")
	runFreshnessAbsenceSetup(t, checkout, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "advance source")

	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	runFreshnessAbsenceSetup(t, target, "git", "init", "-b", "main")
	runFreshnessAbsenceSetup(t, target, "git", "config", "user.name", "Test")
	runFreshnessAbsenceSetup(t, target, "git", "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runFreshnessAbsenceSetup(t, target, "git", "add", "README.md")
	runFreshnessAbsenceSetup(t, target, "git", "commit", "-m", "initial")

	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "pi"), "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"session\",\"id\":\"native-session\"}'\nprintf '%s\\n' '{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"<shephrd-event>{\\\"type\\\":\\\"checkpoint\\\",\\\"payload\\\":\\\"ready\\\",\\\"checkpoint\\\":{\\\"schema_version\\\":1,\\\"summary\\\":\\\"ready\\\",\\\"completed\\\":[],\\\"next_steps\\\":[\\\"wait\\\"],\\\"decisions\\\":[],\\\"changed_paths\\\":[],\\\"checks\\\":[],\\\"blockers\\\":[]}}</shephrd-event>\"}],\"stopReason\":\"stop\"}}'\nprintf '%s\\n' '{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"<shephrd-event>{\\\"type\\\":\\\"question\\\",\\\"payload\\\":\\\"wait\\\"}</shephrd-event>\"}],\"stopReason\":\"stop\"}}'\nprintf '%s\\n' '{\"type\":\"agent_end\"}'\n")
	configPath := filepath.Join(root, "config.toml")
	databasePath := filepath.Join(root, "state.db")
	config := fmt.Sprintf("default_harness = \"pi\"\nworker_runtime = \"headless\"\ndatabase_path = %q\ndata_dir = %q\nworktree_root = %q\n", databasePath, filepath.Join(root, "data"), filepath.Join(root, "worktrees"))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"), "SHEPHRD_CONFIG="+configPath, "SHEPHRD_EXECUTABLE="+binary)
	run := func(args ...string) (string, string, error) {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = environment
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		return stdout.String(), stderr.String(), err
	}
	if stdout, stderr, err := run("repo", "add", target, "--name", "target", "--json"); err != nil || stderr != "" {
		t.Fatalf("register target stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	createTask := func(feature string) model.Task {
		t.Helper()
		stdout, stderr, err := run("task", "create", "--repo", target, "--feature", feature, "--driver-id", "driver:test", "stale binary", "--json")
		if err != nil || stderr != "" {
			t.Fatalf("create %s stdout=%q stderr=%q err=%v", feature, stdout, stderr, err)
		}
		var task model.Task
		if err := json.Unmarshal([]byte(stdout), &task); err != nil {
			t.Fatalf("create %s output %q: %v", feature, stdout, err)
		}
		return task
	}
	assertNoFreshness := func(output string) {
		t.Helper()
		lower := strings.ToLower(output)
		if strings.Contains(lower, "freshness") || strings.Contains(lower, "self-hosted executable advisory") {
			t.Fatalf("lifecycle output retained freshness diagnostics: %q", output)
		}
	}

	task := createTask("stale-lifecycle-absence")
	stdout, stderr, err := run("worker", "spawn", task.ID, "--harness", "pi", "--runtime", "headless", "--json")
	if err != nil || stderr != "" {
		t.Fatalf("spawn stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	assertNoFreshness(stdout)
	var spawned model.SpawnResult
	if err := json.Unmarshal([]byte(stdout), &spawned); err != nil || spawned.Task.ID != task.ID || spawned.Attempt.RuntimeExecutable != binary {
		t.Fatalf("spawn result=%+v err=%v body=%q", spawned, err, stdout)
	}
	waitForCLITask(t, databasePath, task.ID, func(task model.Task) bool { return task.Status == model.TaskStatusWaiting && !task.ProcessAlive })
	for _, args := range [][]string{{"worker", "relaunch", task.ID, "--json"}, {"worker", "retry", task.ID, "--json"}} {
		stdout, stderr, err := run(args...)
		if err != nil || stderr != "" {
			t.Fatalf("%v stdout=%q stderr=%q err=%v", args, stdout, stderr, err)
		}
		assertNoFreshness(stdout)
		if err := json.Unmarshal([]byte(stdout), &spawned); err != nil || spawned.Task.ID != task.ID {
			t.Fatalf("%v result=%+v err=%v body=%q", args, spawned, err, stdout)
		}
		waitForCLITask(t, databasePath, task.ID, func(task model.Task) bool { return task.Status == model.TaskStatusWaiting && !task.ProcessAlive })
	}

	failureTask := createTask("stale-error-absence")
	runFreshnessAbsenceSetup(t, target, "git", "branch", "shephrd/"+failureTask.ID)
	_, stderr, err = run("worker", "spawn", failureTask.ID, "--harness", "pi", "--runtime", "headless", "--json")
	if err == nil || !strings.Contains(stderr, "already exists") {
		t.Fatalf("failure stderr=%q err=%v", stderr, err)
	}
	assertNoFreshness(stderr)
	var failure map[string]any
	if err := json.Unmarshal([]byte(stderr), &failure); err != nil {
		t.Fatalf("failure JSON=%q err=%v", stderr, err)
	}
	for key := range failure {
		if strings.Contains(strings.ToLower(key), "freshness") || strings.Contains(strings.ToLower(key), "advisory") {
			t.Fatalf("failure retained field %q: %v", key, failure)
		}
	}
}

func runFreshnessAbsenceSetup(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %s: %v", name, args, output, err)
	}
	return string(output)
}

func copyFreshnessAbsenceProject(t *testing.T, source, target string) {
	t.Helper()
	err := filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == ".git" || relative == "bin" || relative == ".lavish" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		destination := filepath.Join(target, relative)
		if info.IsDir() {
			return os.MkdirAll(destination, info.Mode().Perm())
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		inputCloseErr := input.Close()
		outputCloseErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputCloseErr != nil {
			return inputCloseErr
		}
		return outputCloseErr
	})
	if err != nil {
		t.Fatal(err)
	}
}
