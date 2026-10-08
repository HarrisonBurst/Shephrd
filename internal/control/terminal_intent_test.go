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

type intentFaultProvider struct {
	state interface {
		Attempt(string) (model.Attempt, error)
	}
	attemptID  string
	executable string
	endpoint   terminal.Endpoint
	createErr  error
	abortErr   error
	aborts     int
	started    bool
	closed     bool
}

func (p *intentFaultProvider) Backend() string                          { return "fixture" }
func (p *intentFaultProvider) WithSocket(string) terminal.RuntimeClient { return p }
func (p *intentFaultProvider) Validate() error                          { return nil }
func (p *intentFaultProvider) Diagnostics() (terminal.Diagnostics, error) {
	return terminal.Diagnostics{ProviderVersion: "fixture-1", ProtocolVersion: "1", Capabilities: []string{"exact-close", "processes"}}, nil
}
func (p *intentFaultProvider) CreateWorkspace(terminal.WorkspaceSpec) (terminal.Endpoint, error) {
	p.closed = false
	return p.endpoint, p.createErr
}
func (p *intentFaultProvider) Start(endpoint terminal.Endpoint, _ string) error {
	attempt, err := p.state.Attempt(p.attemptID)
	if err != nil {
		return err
	}
	if attempt.TerminalEndpoint == nil || attempt.TerminalEndpoint.WorkspaceID != endpoint.WorkspaceID {
		return fmt.Errorf("endpoint was not durable before Start")
	}
	p.started = true
	return nil
}
func (p *intentFaultProvider) Inspect(endpoint terminal.Endpoint) (terminal.EndpointState, error) {
	if p.closed {
		return terminal.EndpointState{}, terminal.ErrNotFound
	}
	return terminal.EndpointState{Endpoint: endpoint, Label: "fixture", PaneCount: 1, SurfaceCount: 1}, nil
}
func (p *intentFaultProvider) ProcessInfo(endpoint terminal.Endpoint) (terminal.ProcessInfo, error) {
	processes := []terminal.Process{}
	if p.started {
		processes = append(processes, terminal.Process{PID: 8801, PGID: 8801, Name: "shephrd", Path: p.executable})
	}
	return terminal.ProcessInfo{PaneID: endpoint.PaneID, ForegroundProcesses: processes}, nil
}
func (p *intentFaultProvider) Read(terminal.Endpoint, int) (string, error) {
	return "fixture output", nil
}
func (p *intentFaultProvider) Focus(terminal.Endpoint) error { return nil }
func (p *intentFaultProvider) Close(terminal.Endpoint) error {
	p.closed = true
	p.started = false
	return nil
}
func (p *intentFaultProvider) ReportAgent(terminal.Endpoint, terminal.AgentReport) error { return nil }
func (p *intentFaultProvider) ReleaseAgent(terminal.Endpoint, string, string, uint64) error {
	return nil
}
func (p *intentFaultProvider) ReportMetadata(terminal.Endpoint, string, string, string, string) error {
	return nil
}
func (p *intentFaultProvider) ContextKeys() []string { return []string{"FIXTURE_TERMINAL_PARENT"} }
func (p *intentFaultProvider) Detect(context terminal.ParentContext) terminal.Detection {
	if context.Value("FIXTURE_TERMINAL_PARENT") == "" {
		return terminal.Detection{Provider: p.Backend(), State: terminal.DetectionAbsent}
	}
	diagnostics, _ := p.Diagnostics()
	return terminal.Detection{Provider: p.Backend(), State: terminal.DetectionMatched, Parent: terminal.Parent{Backend: p.Backend(), SocketPath: p.endpoint.SocketPath, WorkspaceID: "parent", TabID: "parent-tab", PaneID: "parent-pane"}, Diagnostics: diagnostics}
}
func (p *intentFaultProvider) OwnedCleanup() bool { return false }
func (p *intentFaultProvider) ValidateEndpoint(endpoint terminal.Endpoint) error {
	if endpoint.Backend != p.Backend() || endpoint.SocketPath == "" || endpoint.WorkspaceID == "" || endpoint.TabID == "" || endpoint.PaneID == "" {
		return fmt.Errorf("fixture endpoint is incomplete")
	}
	return nil
}

type intentPreparedWorkspace struct {
	provider *intentFaultProvider
	endpoint terminal.Endpoint
}

func (w *intentPreparedWorkspace) Endpoint() terminal.Endpoint { return w.endpoint }
func (w *intentPreparedWorkspace) Commit() error               { return nil }
func (w *intentPreparedWorkspace) Abort() error {
	w.provider.aborts++
	w.provider.closed = true
	return w.provider.abortErr
}

func (p *intentFaultProvider) PrepareWorkspace(spec terminal.WorkspaceSpec) (terminal.PreparedWorkspace, error) {
	if p.createErr != nil {
		return nil, p.createErr
	}
	return &intentPreparedWorkspace{provider: p, endpoint: p.endpoint}, nil
}

func intentLaunchFixture(t *testing.T, root, backend string, provider terminal.Provider) (Service, *store.Store, model.Attempt, string) {
	t.Helper()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Intent task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "intent", Objective: "durable intent"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "codex", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetRuntimeBackend(attempt.ID, backend); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", root, "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt.RunGeneration = generation
	inputPath := filepath.Join(root, "data", task.ID, "brief.md")
	if err := os.MkdirAll(filepath.Dir(inputPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(config.Config{DataDir: filepath.Join(root, "data"), WorktreeRoot: filepath.Join(root, "worktrees")}, state)
	service.TerminalProviders = []terminal.Provider{provider}
	return service, state, attempt, inputPath
}

func TestTerminalLaunchPersistsDurableCreateIntent(t *testing.T) {
	root := t.TempDir()
	selected := writeRuntimeExecutable(t, filepath.Join(root, "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	t.Setenv("SHEPHRD_EXECUTABLE", selected)
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("FIXTURE_TERMINAL_PARENT", "1")
	endpoint := terminal.Endpoint{Backend: "fixture", SocketPath: "/fixture.sock", WorkspaceID: "fixture-workspace", TabID: "fixture-tab", PaneID: "fixture-pane"}
	provider := &intentFaultProvider{executable: selected, endpoint: endpoint}
	service, state, attempt, inputPath := intentLaunchFixture(t, root, "fixture", provider)
	defer state.Close()
	provider.state = state
	provider.attemptID = attempt.ID
	service.processes = processCapability{alive: func(pid int) bool { return pid == 8801 && provider.started }, stop: func(int) error { provider.started = false; return nil }}
	if err := service.launch(attempt, inputPath, false); err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalCreateIntent == nil || stored.TerminalCreateIntent.State != model.TerminalCreateIntentCommitted || stored.TerminalCreateIntent.RunGeneration != attempt.RunGeneration || stored.TerminalCreateIntent.Source != fmt.Sprintf("shephrd:%s:1", attempt.ID) || stored.TerminalCreateIntent.WorkspaceID == "" || stored.TerminalCreateIntent.Label == "" {
		t.Fatalf("intent = %+v", stored.TerminalCreateIntent)
	}
}

func TestTerminalLaunchKeepsAttemptHeldWhenCreateSideEffectIsUncertain(t *testing.T) {
	root := t.TempDir()
	selected := writeRuntimeExecutable(t, filepath.Join(root, "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	t.Setenv("SHEPHRD_EXECUTABLE", selected)
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("FIXTURE_TERMINAL_PARENT", "1")
	endpoint := terminal.Endpoint{Backend: "fixture", SocketPath: "/fixture.sock", WorkspaceID: "fixture-workspace", TabID: "fixture-tab", PaneID: "fixture-pane"}
	provider := &intentFaultProvider{executable: selected, endpoint: endpoint, createErr: &terminal.CreateSideEffect{Effect: terminal.CreateEffectUnknown, Cause: fmt.Errorf("create failed with uncertain side effect")}}
	service, state, attempt, inputPath := intentLaunchFixture(t, root, "fixture", provider)
	defer state.Close()
	provider.state = state
	provider.attemptID = attempt.ID
	service.processes = processCapability{alive: func(int) bool { return false }}
	if err := service.launch(attempt, inputPath, false); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("launch error = %v", err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalCreateIntent == nil || stored.TerminalCreateIntent.State != model.TerminalCreateIntentPending {
		t.Fatalf("intent = %+v", stored.TerminalCreateIntent)
	}
	if stored.TerminalEndpoint != nil {
		t.Fatalf("endpoint was persisted for uncertain create: %+v", stored.TerminalEndpoint)
	}
	if err := service.launch(stored, inputPath, false); err == nil || !strings.Contains(err.Error(), "unresolved terminal create intent") {
		t.Fatalf("second launch error = %v", err)
	}
	if _, err := service.ClearTerminalCreateIntent(stored.TaskID); err != nil {
		t.Fatal(err)
	}
	stored, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalCreateIntent == nil || stored.TerminalCreateIntent.State != model.TerminalCreateIntentAbandoned {
		t.Fatalf("intent after clear = %+v", stored.TerminalCreateIntent)
	}
}

func TestTerminalLaunchResolvesIntentWhenCreateSideEffectIsProvenAbsent(t *testing.T) {
	root := t.TempDir()
	selected := writeRuntimeExecutable(t, filepath.Join(root, "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	t.Setenv("SHEPHRD_EXECUTABLE", selected)
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("FIXTURE_TERMINAL_PARENT", "1")
	endpoint := terminal.Endpoint{Backend: "fixture", SocketPath: "/fixture.sock", WorkspaceID: "fixture-workspace", TabID: "fixture-tab", PaneID: "fixture-pane"}
	provider := &intentFaultProvider{executable: selected, endpoint: endpoint, createErr: fmt.Errorf("create rejected before provider call")}
	service, state, attempt, inputPath := intentLaunchFixture(t, root, "fixture", provider)
	defer state.Close()
	provider.state = state
	provider.attemptID = attempt.ID
	service.processes = processCapability{alive: func(int) bool { return false }}
	if err := service.launch(attempt, inputPath, false); err == nil {
		t.Fatal("launch should fail")
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalCreateIntent == nil || stored.TerminalCreateIntent.State != model.TerminalCreateIntentAborted {
		t.Fatalf("intent = %+v", stored.TerminalCreateIntent)
	}
	provider.createErr = nil
	if err := service.launch(stored, inputPath, false); err != nil {
		t.Fatalf("relaunch after proven absent intent: %v", err)
	}
}

func TestTerminalLaunchResolvesIntentAbortedWhenPreparedAbortIsProven(t *testing.T) {
	root := t.TempDir()
	selected := writeRuntimeExecutable(t, filepath.Join(root, "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	t.Setenv("SHEPHRD_EXECUTABLE", selected)
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("FIXTURE_TERMINAL_PARENT", "1")
	provider := &intentFaultProvider{executable: selected, endpoint: terminal.Endpoint{Backend: "fixture", SocketPath: "/fixture.sock", WorkspaceID: "other-workspace"}}
	service, state, attempt, inputPath := intentLaunchFixture(t, root, "fixture", provider)
	defer state.Close()
	provider.state = state
	provider.attemptID = attempt.ID
	service.processes = processCapability{alive: func(int) bool { return false }}
	if err := service.launch(attempt, inputPath, false); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("launch error = %v", err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.aborts != 1 {
		t.Fatalf("aborts = %d", provider.aborts)
	}
	if stored.TerminalCreateIntent == nil || stored.TerminalCreateIntent.State != model.TerminalCreateIntentAborted {
		t.Fatalf("intent = %+v", stored.TerminalCreateIntent)
	}
}

func TestTerminalLaunchKeepsIntentPendingWhenPreparedAbortIsUncertain(t *testing.T) {
	root := t.TempDir()
	selected := writeRuntimeExecutable(t, filepath.Join(root, "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	t.Setenv("SHEPHRD_EXECUTABLE", selected)
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("FIXTURE_TERMINAL_PARENT", "1")
	provider := &intentFaultProvider{executable: selected, endpoint: terminal.Endpoint{Backend: "fixture", SocketPath: "/fixture.sock", WorkspaceID: "other-workspace"}, abortErr: &terminal.CreateSideEffect{Effect: terminal.CreateEffectUnknown, Cause: fmt.Errorf("extension abort acknowledgement was lost")}}
	service, state, attempt, inputPath := intentLaunchFixture(t, root, "fixture", provider)
	defer state.Close()
	provider.state = state
	provider.attemptID = attempt.ID
	service.processes = processCapability{alive: func(int) bool { return false }}
	if err := service.launch(attempt, inputPath, false); err == nil || !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("launch error = %v", err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.aborts != 1 {
		t.Fatalf("aborts = %d", provider.aborts)
	}
	if stored.TerminalCreateIntent == nil || stored.TerminalCreateIntent.State != model.TerminalCreateIntentPending {
		t.Fatalf("intent = %+v", stored.TerminalCreateIntent)
	}
	if stored.TerminalEndpoint != nil {
		t.Fatalf("endpoint was persisted for uncertain prepared abort: %+v", stored.TerminalEndpoint)
	}
	if err := service.launch(stored, inputPath, false); err == nil || !strings.Contains(err.Error(), "unresolved terminal create intent") {
		t.Fatalf("second launch error = %v", err)
	}
	if _, err := service.ClearTerminalCreateIntent(stored.TaskID); err != nil {
		t.Fatal(err)
	}
	stored, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalCreateIntent == nil || stored.TerminalCreateIntent.State != model.TerminalCreateIntentAbandoned {
		t.Fatalf("intent after clear = %+v", stored.TerminalCreateIntent)
	}
}
