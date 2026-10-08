package control

import (
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

func TestWorkerExecutableSelection(t *testing.T) {
	root := t.TempDir()
	repositoryBinary := writeRuntimeExecutable(t, filepath.Join(root, "Projects", "Shephrd", "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	goBinary := writeRuntimeExecutable(t, filepath.Join(root, "go", "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	installedBinary := writeRuntimeExecutable(t, filepath.Join(root, "installed", "shephrd"), "#!/bin/sh\nexit 0\n")
	homeManagerBinary := filepath.Join(root, ".pi", "agent", "bin", "shephrd")
	homeManagerTarget := filepath.Join(root, "nix", "home-manager-files", ".pi", "agent", "bin", "shephrd")
	if err := os.MkdirAll(filepath.Dir(homeManagerBinary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(homeManagerTarget), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repositoryBinary, homeManagerTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(homeManagerTarget, homeManagerBinary); err != nil {
		t.Fatal(err)
	}
	selected, err := selectWorkerExecutable(homeManagerBinary, goBinary)
	if err != nil {
		t.Fatal(err)
	}
	if selected != homeManagerBinary {
		t.Fatalf("Home Manager selection = %q, want exact symlink %q", selected, homeManagerBinary)
	}
	for _, test := range []struct {
		name       string
		configured string
		fallback   string
		want       string
	}{
		{name: "Go-bin invoker falls back only when unconfigured", fallback: goBinary, want: goBinary},
		{name: "direct repository binary", fallback: repositoryBinary, want: repositoryBinary},
		{name: "installed binary outside repository", fallback: installedBinary, want: installedBinary},
		{name: "missing configured path", configured: filepath.Join(root, "missing"), fallback: goBinary, want: goBinary},
		{name: "relative configured path", configured: "shephrd", fallback: goBinary, want: goBinary},
		{name: "directory configured path", configured: root, fallback: goBinary, want: goBinary},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected, err := selectWorkerExecutable(test.configured, test.fallback)
			if err != nil {
				t.Fatal(err)
			}
			if selected != test.want {
				t.Fatalf("selection = %q, want %q", selected, test.want)
			}
		})
	}
	nonExecutable := filepath.Join(root, "not-executable")
	if err := os.WriteFile(nonExecutable, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err = selectWorkerExecutable(nonExecutable, goBinary)
	if err != nil || selected != goBinary {
		t.Fatalf("non-executable selection = %q, err = %v", selected, err)
	}
}

func TestWorkerRuntimeExecutableIsSharedAcrossBackendsAndHarnesses(t *testing.T) {
	for _, backend := range []string{"headless", "herdr"} {
		for _, harness := range []string{"claude-code", "pi", "codex"} {
			t.Run(backend+"/"+harness, func(t *testing.T) {
				root := t.TempDir()
				selected := writeRuntimeExecutable(t, filepath.Join(root, "configured", "shephrd"), "#!/bin/sh\nprintf '%s\\n%s\\n%s\\n%s\\n' \"${SHEPHRD_EXECUTABLE:-}\" \"${SHEPHRD_PI_WATCHER_ENABLED:-}\" \"${SHEPHRD_DRIVER_HARNESS:-}\" \"${SHEPHRD_DRIVER_MODEL:-}\" > \"$SHEPHRD_TEST_RUNTIME\"\n")
				marker := filepath.Join(root, "runtime-environment")
				t.Setenv("SHEPHRD_EXECUTABLE", selected)
				t.Setenv("SHEPHRD_PI_WATCHER_ENABLED", "1")
				t.Setenv("SHEPHRD_DRIVER_HARNESS", "pi")
				t.Setenv("SHEPHRD_DRIVER_MODEL", "driver-model")
				t.Setenv("SHEPHRD_TEST_RUNTIME", marker)
				t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
				service, state, attempt, runner, inputPath := runtimeLaunchFixture(t, root, backend, harness)
				defer state.Close()
				if err := service.launch(attempt, inputPath, false); err != nil {
					t.Fatal(err)
				}
				stored, err := state.Attempt(attempt.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.RuntimeExecutable != selected {
					t.Fatalf("runtime executable = %q, want %q", stored.RuntimeExecutable, selected)
				}
				if backend == "headless" {
					waitFor(t, func() bool {
						body, readErr := os.ReadFile(marker)
						if readErr != nil {
							return false
						}
						lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
						if len(lines) != 4 || lines[0] != selected || lines[2] != "" || lines[3] != "" {
							t.Fatalf("headless environment = %q", body)
						}
						if harness == "pi" && lines[1] != "0" {
							t.Fatalf("Pi watcher override = %q", lines[1])
						}
						return true
					})
					return
				}
				calls := runner.joinedCalls()
				if !strings.Contains(calls, "tab create --workspace w7 --cwd "+root+" --label demo-Runtime-parity-runtime --no-focus") {
					t.Fatalf("Herdr calls missing repository-prefixed tab label:\n%s", calls)
				}
				if !strings.Contains(calls, "--title demo-Runtime-parity-runtime --display-agent demo-Runtime-parity-runtime") {
					t.Fatalf("Herdr calls missing repository-prefixed pane metadata:\n%s", calls)
				}
				if !strings.Contains(calls, "--env SHEPHRD_EXECUTABLE="+selected) {
					t.Fatalf("Herdr calls missing selected executable:\n%s", calls)
				}
				for _, variable := range []string{"SHEPHRD_DRIVER_HARNESS", "SHEPHRD_DRIVER_MODEL"} {
					if !strings.Contains(calls, "--env "+variable+"=") {
						t.Fatalf("Herdr calls did not clear %s:\n%s", variable, calls)
					}
				}
				watcherDisabled := strings.Contains(calls, "--env SHEPHRD_PI_WATCHER_ENABLED=0")
				if watcherDisabled != (harness == "pi") {
					t.Fatalf("Herdr watcher override for %s = %t:\n%s", harness, watcherDisabled, calls)
				}
			})
		}
	}
}

type runtimeLaunchRunner struct {
	attemptID string
	calls     []string
}

func (r *runtimeLaunchRunner) Run(_ string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	if len(args) < 2 {
		return nil, nil, fmt.Errorf("unexpected Herdr call: %v", args)
	}
	switch strings.Join(args[:2], " ") {
	case "status server":
		return []byte(`{"running":true,"protocol":17,"compatible":true,"socket":"/socket"}`), nil, nil
	case "tab create":
		return []byte(`{"result":{"root_pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7"},"tab":{"tab_id":"w7:t2","workspace_id":"w7","pane_count":1}}}`), nil, nil
	case "pane get":
		if len(args) > 2 && args[2] == "w7:p1" {
			return []byte(`{"result":{"pane":{"pane_id":"w7:p1","tab_id":"w7:t1","workspace_id":"w7"}}}`), nil, nil
		}
		return []byte(`{"result":{"pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7"}}}`), nil, nil
	case "tab get":
		if len(args) > 2 && args[2] == "w7:t1" {
			return []byte(`{"result":{"tab":{"tab_id":"w7:t1","workspace_id":"w7","pane_count":1}}}`), nil, nil
		}
		return []byte(`{"result":{"tab":{"tab_id":"w7:t2","workspace_id":"w7","pane_count":1}}}`), nil, nil
	case "pane report-metadata", "pane rename", "pane report-agent", "pane run":
		return []byte(`{"result":{"type":"ok"}}`), nil, nil
	case "pane process-info":
		return []byte(fmt.Sprintf(`{"result":{"process_info":{"foreground_process_group_id":999999,"foreground_processes":[{"pid":999999,"name":"shephrd","cmdline":"shephrd _run %s"}],"pane_id":"w7:p2","shell_pid":999998}}}`, r.attemptID)), nil, nil
	default:
		return nil, nil, fmt.Errorf("unexpected Herdr call: %s", strings.Join(args, " "))
	}
}

func (r *runtimeLaunchRunner) joinedCalls() string {
	return strings.Join(r.calls, "\n")
}

func runtimeLaunchFixture(t *testing.T, root, backend, harness string) (Service, *store.Store, model.Attempt, *runtimeLaunchRunner, string) {
	t.Helper()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Runtime parity", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "runtime", Objective: "runtime parity"})
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, harness, "")
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	if err := state.SetRuntimeBackend(attempt.ID, backend); err != nil {
		state.Close()
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", root, "lease", "branch"); err != nil {
		state.Close()
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	attempt.RunGeneration = generation
	attempt.TerminalEndpoint = &model.TerminalEndpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: "w7"}
	if backend == "herdr" {
		t.Setenv("HERDR_ENV", "1")
		t.Setenv("HERDR_SOCKET_PATH", "/socket")
		t.Setenv("HERDR_WORKSPACE_ID", "w7")
		t.Setenv("HERDR_PANE_ID", "w7:p1")
	}
	inputPath := filepath.Join(root, "data", task.ID, "brief.md")
	if err := os.MkdirAll(filepath.Dir(inputPath), 0o700); err != nil {
		state.Close()
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, []byte("work\n"), 0o600); err != nil {
		state.Close()
		t.Fatal(err)
	}
	runner := &runtimeLaunchRunner{attemptID: attempt.ID}
	service := New(config.Config{DataDir: filepath.Join(root, "data")}, state)
	service.TerminalProviders = []terminal.Provider{herdrClientProvider{Client: terminal.NewWithRunner("/socket", runner)}}
	return service, state, attempt, runner, inputPath
}

func writeRuntimeExecutable(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
