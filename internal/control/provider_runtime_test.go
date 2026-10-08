package control

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
)

func TestExplicitHeadlessDoesNotLaunchConfiguredExtension(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "invoked")
	executable := filepath.Join(t.TempDir(), "cmux-extension")
	body := []byte("#!/bin/sh\ntouch " + marker + "\nexit 1\n")
	if err := os.WriteFile(executable, body, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	extension := &config.ExtensionConfig{Command: []string{executable}, SHA256: hex.EncodeToString(digest[:])}
	service := Service{Config: config.Config{TerminalExtensions: config.TerminalExtensions{Herdr: extension, Cmux: extension}}}
	service.environment.list = func() []string {
		return []string{"HERDR_ENV=1", "HERDR_SOCKET_PATH=/socket", "HERDR_WORKSPACE_ID=w7", "HERDR_PANE_ID=w7:p1", "CMUX_SOCKET_PATH=/socket", "CMUX_WORKSPACE_ID=11111111-1111-4111-8111-111111111111", "CMUX_SURFACE_ID=22222222-2222-4222-8222-222222222222"}
	}
	selection, err := service.selectRuntime("headless")
	if err != nil || selection.Backend != "headless" {
		t.Fatalf("selection = %+v, err = %v", selection, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("configured extension was invoked: %v", err)
	}
}

func TestConfiguredCmuxExtensionRoutesCoreOrchestrationThroughRealSubprocesses(t *testing.T) {
	root := t.TempDir()
	selected := writeRuntimeExecutable(t, filepath.Join(root, "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	extensionExecutable := filepath.Join(root, "bin", "shephrd-terminal-cmux")
	build := exec.Command("go", "build", "-o", extensionExecutable, "./internal/terminal/testdata/cmuxextension")
	build.Dir = repositoryRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture extension: %s: %v", output, err)
	}
	if err := os.Chmod(extensionExecutable, 0o755); err != nil {
		t.Fatal(err)
	}
	extensionBytes, err := os.ReadFile(extensionExecutable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(extensionBytes)
	logPath := filepath.Join(root, "extension-operations.log")
	t.Setenv("SHEPHRD_EXECUTABLE", selected)
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("CMUX_SOCKET_PATH", "/socket")
	t.Setenv("CMUX_WORKSPACE_ID", "22222222-2222-4222-8222-222222222222")
	t.Setenv("CMUX_SURFACE_ID", "44444444-4444-4444-8444-444444444444")
	service, state, attempt, _, inputPath := runtimeLaunchFixture(t, root, "cmux", "codex")
	defer state.Close()
	service.TerminalProviders = nil
	service.Config.TerminalExtensions.Cmux = &config.ExtensionConfig{Command: []string{extensionExecutable, logPath, selected}, SHA256: hex.EncodeToString(digest[:])}
	service.processes = processCapability{alive: func(pid int) bool { return pid == 8801 }}
	if err := service.launch(attempt, inputPath, false); err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalEndpoint == nil || stored.TerminalEndpoint.Backend != "cmux" || stored.TerminalEndpoint.ProtocolVersion != "2" || stored.RunnerPID != 8801 {
		t.Fatalf("attempt = %+v", stored)
	}
	operations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"detect", "create", "commit", "start", "inspect", "process_info", "read"} {
		if !strings.Contains(string(operations), operation+"\n") {
			t.Fatalf("operation %q missing from %q", operation, operations)
		}
	}
}

func TestConfiguredHerdrExtensionRoutesCoreOrchestrationThroughRealSubprocesses(t *testing.T) {
	root := t.TempDir()
	selected := writeRuntimeExecutable(t, filepath.Join(root, "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	extensionExecutable := filepath.Join(root, "bin", "shephrd-terminal-herdr")
	build := exec.Command("go", "build", "-o", extensionExecutable, "./internal/terminal/testdata/herdrextension")
	build.Dir = repositoryRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture extension: %s: %v", output, err)
	}
	if err := os.Chmod(extensionExecutable, 0o755); err != nil {
		t.Fatal(err)
	}
	extensionBytes, err := os.ReadFile(extensionExecutable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(extensionBytes)
	logPath := filepath.Join(root, "extension-operations.log")
	t.Setenv("SHEPHRD_EXECUTABLE", selected)
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SOCKET_PATH", "/socket")
	t.Setenv("HERDR_WORKSPACE_ID", "w7")
	t.Setenv("HERDR_PANE_ID", "w7:p1")
	service, state, attempt, _, inputPath := runtimeLaunchFixture(t, root, "herdr", "codex")
	defer state.Close()
	service.TerminalProviders = nil
	service.Config.TerminalExtensions.Herdr = &config.ExtensionConfig{Command: []string{extensionExecutable, logPath, selected}, SHA256: hex.EncodeToString(digest[:])}
	service.processes = processCapability{alive: func(pid int) bool { return pid == 8801 }}
	if err := service.launch(attempt, inputPath, false); err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalEndpoint == nil || stored.TerminalEndpoint.Backend != "herdr" || stored.TerminalEndpoint.ProtocolVersion != "17" || stored.TerminalEndpoint.TabID != "w7:t2" || stored.RunnerPID != 8801 {
		t.Fatalf("attempt = %+v", stored)
	}
	operations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"detect", "create", "commit", "start", "inspect", "process_info", "read"} {
		if !strings.Contains(string(operations), operation+"\n") {
			t.Fatalf("operation %q missing from %q", operation, operations)
		}
	}
	originalTask, err := state.Task(attempt.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	legacyTask, err := state.CreateTask(model.Task{Title: "Legacy held Herdr attempt", DriverID: "driver:test", RepoID: originalTask.RepoID, FeatureKey: "legacy-herdr", Objective: "inspect a held endpoint"})
	if err != nil {
		t.Fatal(err)
	}
	legacyAttempt, err := state.BeginAttempt(legacyTask.ID, "codex", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetRuntimeBackend(legacyAttempt.ID, "herdr"); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, legacyAttempt.ID, "legacy-session", root, "legacy-lease", "legacy-branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(legacyAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetTerminalEndpointForRun(legacyAttempt.ID, generation, model.TerminalEndpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}); err != nil {
		t.Fatal(err)
	}
	peek, err := service.Peek(legacyTask.ID, 25)
	if err != nil || peek.Output != "fixture output" || !peek.EndpointPresent {
		t.Fatalf("legacy held peek = %+v, err = %v", peek, err)
	}
}

func TestUnconfiguredTerminalRuntimeIsRefusedBeforeAttemptOrWorktree(t *testing.T) {
	tests := []struct {
		name        string
		backend     string
		environment []string
	}{
		{name: "herdr parent marker present", backend: "herdr", environment: []string{"HERDR_ENV=1", "HERDR_SOCKET_PATH=/socket", "HERDR_WORKSPACE_ID=w7", "HERDR_PANE_ID=w7:p1"}},
		{name: "cmux parent marker present", backend: "cmux", environment: []string{"CMUX_SOCKET_PATH=/socket", "CMUX_WORKSPACE_ID=22222222-2222-4222-8222-222222222222", "CMUX_SURFACE_ID=44444444-4444-4444-8444-444444444444"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			state, err := store.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(root, "repo"), DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "cutover", Objective: "fail safely"})
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			service := New(config.Config{DataDir: filepath.Join(root, "data"), WorktreeRoot: filepath.Join(root, "worktrees")}, state)
			service.environment.list = func() []string { return test.environment }
			_, spawnErr := service.SpawnWithModelSelection(task.ID, "pi", "", false, test.backend)
			var selectionErr *terminal.SelectionError
			if !errors.As(spawnErr, &selectionErr) || selectionErr.ErrorKind() != "terminal_extension_not_configured" {
				t.Fatalf("error = %#v", spawnErr)
			}
			stored, err := state.Task(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			attempts, err := state.Attempts(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != model.TaskStatusQueued || stored.CurrentAttemptID != "" || len(attempts) != 0 {
				t.Fatalf("task = %+v, attempts = %+v", stored, attempts)
			}
			if _, err := os.Stat(filepath.Join(root, "worktrees")); !os.IsNotExist(err) {
				t.Fatalf("worktree root was created: %v", err)
			}
		})
	}
}

func TestAutoSelectionStaysHeadlessWithoutConfiguredExtensions(t *testing.T) {
	service := Service{Config: config.Config{}}
	service.environment.list = func() []string {
		return []string{"HERDR_ENV=1", "HERDR_SOCKET_PATH=/socket", "HERDR_WORKSPACE_ID=w7", "HERDR_PANE_ID=w7:p1", "CMUX_SOCKET_PATH=/socket", "CMUX_WORKSPACE_ID=22222222-2222-4222-8222-222222222222", "CMUX_SURFACE_ID=44444444-4444-4444-8444-444444444444"}
	}
	selection, err := service.selectRuntime("auto")
	if err != nil || selection.Backend != "headless" || selection.Provider != nil || len(selection.Detections) != 0 {
		t.Fatalf("selection = %+v, err = %v", selection, err)
	}
}

func TestRunnerProcessAttributionRejectsMultipleMatchingGroups(t *testing.T) {
	attempt := model.Attempt{ID: "attempt_exact", RuntimeExecutable: "/opt/shephrd"}
	info := terminal.ProcessInfo{ForegroundProcesses: []terminal.Process{
		{PID: 101, PGID: 101, Path: "/opt/shephrd"},
		{PID: 202, PGID: 202, Cmdline: "shephrd _run attempt_exact"},
	}}
	if _, err := runnerPID(info, attempt); terminal.Classify(err) != terminal.ErrorAmbiguous {
		t.Fatalf("error = %v", err)
	}
}
