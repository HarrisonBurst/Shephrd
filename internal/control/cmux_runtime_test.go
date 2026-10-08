package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
)

const (
	controlCmuxParentWorkspace = "11111111-1111-4111-8111-111111111111"
	controlCmuxParentPane      = "22222222-2222-4222-8222-222222222222"
	controlCmuxParentSurface   = "33333333-3333-4333-8333-333333333333"
	controlCmuxWindow          = "44444444-4444-4444-8444-444444444444"
	controlCmuxWorkspace       = "55555555-5555-4555-8555-555555555555"
	controlCmuxPane            = "66666666-6666-4666-8666-666666666666"
	controlCmuxSurface         = "77777777-7777-4777-8777-777777777777"
)

type controlCmuxRunner struct {
	executable      string
	worktree        string
	label           string
	calls           [][]string
	running         bool
	closed          bool
	focused         bool
	endpointFailure string
	failStart       bool
}

func (r *controlCmuxRunner) Run(_ string, _ string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) < 5 {
		return nil, nil, errors.New("missing global cmux arguments")
	}
	command := args[5:]
	key := strings.Join(command, " ")
	switch {
	case key == "capabilities":
		methods := []string{"system.capabilities", "system.identify", "system.top", "window.current", "window.list", "workspace.action", "workspace.create", "workspace.list", "workspace.close", "workspace.select", "surface.send_text", "surface.send_key", "surface.read_text"}
		output, err := json.Marshal(map[string]any{"socket_path": "/socket", "protocol": "cmux-socket", "version": 2, "methods": methods})
		return output, nil, err
	case key == "version":
		return []byte("cmux 0.64.22 (102) [ddd4a01bc]"), nil, nil
	case strings.HasPrefix(key, "identify "):
		return []byte(fmt.Sprintf(`{"socket_path":"/socket","caller":{"window_id":"%s","workspace_id":"%s","pane_id":"%s","surface_id":"%s","surface_type":"terminal","is_browser_surface":false}}`, controlCmuxWindow, controlCmuxParentWorkspace, controlCmuxParentPane, controlCmuxParentSurface)), nil, nil
	case strings.HasPrefix(key, "workspace create "):
		for index, value := range command {
			if value == "--cwd" && index+1 < len(command) {
				r.worktree = command[index+1]
			}
			if value == "--name" && index+1 < len(command) {
				r.label = command[index+1]
			}
		}
		r.closed = false
		return []byte(fmt.Sprintf(`{"window_id":"%s","workspace_id":"%s","surface_id":"%s"}`, controlCmuxWindow, controlCmuxWorkspace, controlCmuxSurface)), nil, nil
	case key == "workspace list --window "+controlCmuxWindow:
		return []byte(fmt.Sprintf(`{"workspaces":[{"id":"%s","current_directory":%q}]}`, controlCmuxWorkspace, r.worktree)), nil, nil
	case key == "tree --workspace "+controlCmuxWorkspace+" --window "+controlCmuxWindow:
		if r.endpointFailure != "" {
			return nil, []byte(r.endpointFailure), errors.New("exit")
		}
		if r.closed {
			return nil, []byte("Error: not_found: Workspace not found"), errors.New("exit")
		}
		return []byte(fmt.Sprintf(`{"windows":[{"id":"%s","workspaces":[{"id":"%s","title":%q,"selected":%t,"panes":[{"id":"%s","focused":%t,"surfaces":[{"id":"%s","pane_id":"%s","type":"terminal","focused":%t,"selected":%t}]}]}]}]}`, controlCmuxWindow, controlCmuxWorkspace, r.label, r.focused, controlCmuxPane, r.focused, controlCmuxSurface, controlCmuxPane, r.focused, r.focused)), nil, nil
	case strings.HasPrefix(key, "send --workspace "):
		return []byte(`{"ok":true}`), nil, nil
	case strings.HasPrefix(key, "send-key --workspace "):
		if r.failStart {
			return nil, []byte("Error: connection reset"), errors.New("exit")
		}
		r.running = true
		return []byte(`{"ok":true}`), nil, nil
	case key == "top --workspace "+controlCmuxWorkspace+" --window "+controlCmuxWindow+" --processes":
		processes := "[]"
		if r.running {
			processes = fmt.Sprintf(`[{"pid":900,"pgid":900,"name":"shephrd","path":%q,"children":[]}]`, r.executable)
		}
		return []byte(fmt.Sprintf(`{"windows":[{"id":"%s","workspaces":[{"id":"%s","panes":[{"id":"%s","surfaces":[{"id":"%s","processes":%s}]}]}]}]}`, controlCmuxWindow, controlCmuxWorkspace, controlCmuxPane, controlCmuxSurface, processes)), nil, nil
	case strings.HasPrefix(key, "read-screen "):
		return []byte("native output\n"), nil, nil
	case strings.HasPrefix(key, "set-status "), strings.HasPrefix(key, "clear-status "):
		return []byte(`{"ok":true}`), nil, nil
	case strings.HasPrefix(key, "workspace select "), strings.HasPrefix(key, "focus-pane "):
		r.focused = true
		return []byte(`{"ok":true}`), nil, nil
	case key == "workspace close "+controlCmuxWorkspace+" --window "+controlCmuxWindow:
		r.closed = true
		r.running = false
		r.focused = false
		return []byte(`{"ok":true}`), nil, nil
	default:
		return nil, nil, fmt.Errorf("unexpected cmux command %q", key)
	}
}

func TestCmuxPiAndClaudePreserveInteractiveBridgeBoundaries(t *testing.T) {
	for _, harness := range []string{"pi", "claude-code"} {
		t.Run(harness, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			var service Service
			var state *store.Store
			var task model.Task
			var attempt model.Attempt
			if harness == "pi" {
				service, state, task, attempt = interactivePiFixture(t, root, dataDir, "done", filepath.Join(root, "args"), "")
			} else {
				service, state, task, attempt = interactiveClaudeFixture(t, root, dataDir, "done", filepath.Join(root, "args"))
			}
			defer state.Close()
			if err := state.SetRuntimeBackend(attempt.ID, "cmux"); err != nil {
				t.Fatal(err)
			}
			endpoint := model.TerminalEndpoint{Backend: "cmux", SocketPath: "/socket", WindowID: controlCmuxWindow, WorkspaceID: controlCmuxWorkspace, PaneID: controlCmuxPane, SurfaceID: controlCmuxSurface}
			if _, err := state.SetTerminalEndpointForRun(attempt.ID, attempt.RunGeneration, endpoint); err != nil {
				t.Fatal(err)
			}
			attempt, _ = state.Attempt(attempt.ID)
			bridgeClient := terminal.NewCmuxWithRunner("/cmux", "/socket", &controlCmuxRunner{})
			bridgeClient.Platform = "darwin"
			service.TerminalProviders = []terminal.Provider{cmuxClientProvider{CmuxClient: bridgeClient}}
			reportPath := filepath.Join(dataDir, task.ID, "report.md")
			t.Setenv("SHEPHRD_TEST_REPORT", reportPath)
			inputPath := writeInput(t, dataDir, task.ID, "cmux-brief.md", "cmux bridge prompt")
			if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
				t.Fatal(err)
			}
			current, _ := state.Task(task.ID)
			if current.Status != model.TaskStatusDone || !current.ClaimedDone {
				t.Fatalf("task = %+v", current)
			}
			invocations := readArgv(t, filepath.Join(root, "args"))
			if len(invocations) != 1 {
				t.Fatalf("invocations = %q", invocations)
			}
			joined := strings.Join(invocations[0], "\x00")
			if harness == "pi" {
				if strings.Contains(joined, "--mode\x00json") || !strings.Contains(joined, "--extension") {
					t.Fatalf("Pi invocation = %q", invocations[0])
				}
			} else if strings.Contains(joined, "--output-format") || !strings.Contains(joined, "--settings") {
				t.Fatalf("Claude invocation = %q", invocations[0])
			}
		})
	}
}

func TestCmuxSpawnPreflightFailsBeforeAttemptOrWorktree(t *testing.T) {
	tests := []struct {
		name        string
		environment []string
		errorKind   string
		errorText   string
	}{
		{
			name: "marked parent reaches provider validation",
			environment: []string{
				"CMUX_SOCKET_PATH=/socket",
				"CMUX_WORKSPACE_ID=" + controlCmuxParentWorkspace,
				"CMUX_SURFACE_ID=" + controlCmuxParentSurface,
			},
			errorKind: "terminal_provider_invalid",
			errorText: "only on macOS",
		},
		{
			name:      "missing parent is unavailable",
			errorKind: "terminal_provider_unavailable",
			errorText: "not available in the sanitized parent context",
		},
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
			task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "cmux-preflight", Objective: "fail safely"})
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			service := New(config.Config{DataDir: filepath.Join(root, "data"), WorktreeRoot: filepath.Join(root, "worktrees")}, state)
			service.TerminalProviders = []terminal.Provider{cmuxClientProvider{CmuxClient: terminal.CmuxClient{Platform: "linux"}}}
			service.environment.list = func() []string { return test.environment }
			_, spawnErr := service.SpawnWithModelSelection(task.ID, "pi", "", false, "cmux")
			var selectionErr *terminal.SelectionError
			if !errors.As(spawnErr, &selectionErr) || selectionErr.ErrorKind() != test.errorKind || !strings.Contains(spawnErr.Error(), test.errorText) {
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

func TestCmuxQuestionSendStopRelaunchAndRetryUseReplacementEndpoints(t *testing.T) {
	fixture := newNativeFixture(t)
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "pi"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(filepath.Dir(fixture.databasePath), "config.toml"))
	t.Setenv("CMUX_SOCKET_PATH", "/socket")
	t.Setenv("CMUX_WORKSPACE_ID", controlCmuxParentWorkspace)
	t.Setenv("CMUX_SURFACE_ID", controlCmuxParentSurface)
	executable, err := fixture.service.workerExecutable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &controlCmuxRunner{executable: executable}
	cmuxExecutable := filepath.Join(fakeBin, "cmux")
	if err := os.WriteFile(cmuxExecutable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := terminal.NewCmuxWithRunner(cmuxExecutable, "/socket", runner)
	client.Platform = "darwin"
	client.SocketCheck = func(string) error { return nil }
	fixture.service.TerminalProviders = []terminal.Provider{cmuxClientProvider{CmuxClient: client}}
	fixture.service.processes = processCapability{
		alive: func(pid int) bool { return pid > 0 && runner.running },
		stop:  func(int) error { runner.running = false; return nil },
	}
	task := fixture.createTask(t, "cmux-lifecycle")
	spawned, err := fixture.service.SpawnWithModelSelection(task.ID, "pi", "", false, "cmux")
	if err != nil {
		t.Fatal(err)
	}
	attempt := spawned.Attempt
	runner.running = false
	fixture.reachWaiting(t, attempt, workspaceFacts(attempt.WorktreePath))
	if err := fixture.state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, ""); err != nil {
		t.Fatal(err)
	}
	attempt, _ = fixture.state.Attempt(attempt.ID)
	if err := fixture.service.finalizeTerminalEndpoint(attempt); err != nil {
		t.Fatal(err)
	}
	followup, err := fixture.service.Send(task.ID, "continue")
	if err != nil {
		t.Fatal(err)
	}
	if followup.ID != attempt.ID || followup.RunGeneration != attempt.RunGeneration+1 || followup.RuntimeGeneration != attempt.RuntimeGeneration+1 || followup.TerminalEndpoint == nil {
		t.Fatalf("follow-up attempt = %+v", followup)
	}
	runner.running = false
	fixture.reachWaiting(t, followup, workspaceFacts(followup.WorktreePath))
	if err := fixture.state.FinishRunnerForRun(followup.ID, followup.RunGeneration, 0, ""); err != nil {
		t.Fatal(err)
	}
	followup, _ = fixture.state.Attempt(followup.ID)
	if err := fixture.service.finalizeTerminalEndpoint(followup); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Stop(task.ID, "pause", false); err != nil {
		t.Fatal(err)
	}
	relaunched, err := fixture.service.Relaunch(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if relaunched.Attempt.ID != attempt.ID || relaunched.Attempt.RunGeneration != followup.RunGeneration+1 || relaunched.Attempt.SessionID == followup.SessionID || relaunched.Attempt.TerminalEndpoint == nil {
		t.Fatalf("relaunched attempt = %+v", relaunched.Attempt)
	}
	runner.running = false
	if err := fixture.service.recordRunnerDeath(relaunched.Attempt.ID, relaunched.Attempt.RunGeneration, "fixture runner ended"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.finalizeTerminalEndpoint(relaunched.Attempt); err != nil {
		t.Fatal(err)
	}
	retried, err := fixture.service.RetryWithModelSelection(task.ID, "", "", false, "cmux")
	if err != nil {
		t.Fatal(err)
	}
	if retried.Attempt.ID == attempt.ID || retried.Attempt.Number != attempt.Number+1 || retried.Attempt.RuntimeBackend != "cmux" || retried.Attempt.WorktreePath == attempt.WorktreePath || retried.Attempt.TerminalEndpoint == nil {
		t.Fatalf("retried attempt = %+v", retried.Attempt)
	}
}

func TestCmuxInterruptedStartClosesExactPersistedEndpoint(t *testing.T) {
	root := t.TempDir()
	selected := writeRuntimeExecutable(t, filepath.Join(root, "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	t.Setenv("SHEPHRD_EXECUTABLE", selected)
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("CMUX_SOCKET_PATH", "/socket")
	t.Setenv("CMUX_WORKSPACE_ID", controlCmuxParentWorkspace)
	t.Setenv("CMUX_SURFACE_ID", controlCmuxParentSurface)
	service, state, attempt, _, inputPath := runtimeLaunchFixture(t, root, "cmux", "codex")
	defer state.Close()
	task, _ := state.Task(attempt.TaskID)
	runner := &controlCmuxRunner{executable: selected, worktree: root, label: workerLabelFor(task), failStart: true}
	client := terminal.NewCmuxWithRunner(filepath.Join(root, "bin", "cmux"), "/socket", runner)
	client.Platform = "darwin"
	client.SocketCheck = func(string) error { return nil }
	if err := os.WriteFile(client.Executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	service.TerminalProviders = []terminal.Provider{cmuxClientProvider{CmuxClient: client}}
	service.processes = processCapability{alive: func(int) bool { return runner.running }}
	if err := service.launch(attempt, inputPath, false); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("launch error = %v", err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalEndpoint == nil || stored.TerminalEndpoint.WorkspaceID != controlCmuxWorkspace || !runner.closed || runner.running {
		t.Fatalf("attempt = %+v, runner = %+v", stored, runner)
	}
}

func TestCmuxLaunchPersistsEndpointBeforeStartingAndSupportsOperations(t *testing.T) {
	root := t.TempDir()
	selected := writeRuntimeExecutable(t, filepath.Join(root, "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	t.Setenv("SHEPHRD_EXECUTABLE", selected)
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("CMUX_SOCKET_PATH", "/socket")
	t.Setenv("CMUX_WORKSPACE_ID", controlCmuxParentWorkspace)
	t.Setenv("CMUX_SURFACE_ID", controlCmuxParentSurface)
	service, state, attempt, _, inputPath := runtimeLaunchFixture(t, root, "cmux", "codex")
	defer state.Close()
	task, err := state.Task(attempt.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	runner := &controlCmuxRunner{executable: selected, worktree: root, label: workerLabelFor(task)}
	client := terminal.NewCmuxWithRunner(filepath.Join(root, "bin", "cmux"), "/socket", runner)
	client.Platform = "darwin"
	client.SocketCheck = func(string) error { return nil }
	if err := os.WriteFile(client.Executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	service.TerminalProviders = []terminal.Provider{cmuxClientProvider{CmuxClient: client}}
	service.processes = processCapability{
		alive: func(pid int) bool { return pid > 0 && runner.running },
		stop:  func(int) error { runner.running = false; return nil },
	}
	if err := service.launch(attempt, inputPath, false); err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RuntimeBackend != "cmux" || stored.RuntimeGeneration != 1 || stored.RuntimeExecutable != selected || stored.TerminalEndpoint == nil || stored.TerminalEndpoint.WindowID != controlCmuxWindow || stored.TerminalEndpoint.WorkspaceID != controlCmuxWorkspace || stored.TerminalEndpoint.PaneID != controlCmuxPane || stored.TerminalEndpoint.SurfaceID != controlCmuxSurface || stored.TerminalEndpoint.ProviderVersion != "0.64.22 (102) [ddd4a01bc]" || stored.TerminalEndpoint.ProtocolVersion != "2" {
		t.Fatalf("attempt = %+v", stored)
	}
	startIndex, persisted := -1, false
	for index, call := range runner.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "send --workspace") {
			startIndex = index
			persisted = stored.TerminalEndpoint != nil
			break
		}
	}
	if startIndex < 0 || !persisted {
		t.Fatalf("start call missing or endpoint not persisted: %q", runner.calls)
	}
	launcher := filepath.Join(service.Config.DataDir, task.ID, fmt.Sprintf("attempt-%s-run-%d-launcher.sh", attempt.ID, attempt.RunGeneration))
	info, err := os.Stat(launcher)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("launcher mode = %v, err = %v", info, err)
	}
	body, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "exec '"+selected+"' '_run'") || strings.Contains(string(body), "CMUX_SOCKET_PASSWORD") || strings.Contains(string(body), "CMUX_SOCKET_CAPABILITY") {
		t.Fatalf("launcher = %q", body)
	}
	peek, err := service.Peek(task.ID, 20)
	if err != nil || !peek.EndpointPresent || peek.Output != "native output\n" {
		t.Fatalf("peek = %+v, err = %v", peek, err)
	}
	if err := service.Focus(task.ID); err != nil {
		t.Fatal(err)
	}
	runner.endpointFailure = "Error: Socket not found at /socket"
	present, err := service.EndpointStatus(task.ID)
	if err == nil || !present || terminal.IsNotFound(err) {
		t.Fatalf("socket loss present = %t, err = %v", present, err)
	}
	runner.endpointFailure = ""
	runner.running = false
	ok, reason, err := service.reconcileEndpoint(stored, task)
	if err != nil || !ok || reason != "" {
		t.Fatalf("reconcile ok=%t reason=%q err=%v", ok, reason, err)
	}
	present, err = service.EndpointStatus(task.ID)
	if err != nil || present {
		t.Fatalf("endpoint present = %t, err = %v", present, err)
	}
}
