package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func installFakeHarnesses(t *testing.T, executables ...string) {
	t.Helper()
	binDir := t.TempDir()
	for _, executable := range append(executables, "shephrd") {
		if err := os.WriteFile(filepath.Join(binDir, executable), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHEPHRD_EXECUTABLE", filepath.Join(binDir, "shephrd"))
}

func installFakePi(t *testing.T) {
	t.Helper()
	installFakeHarnesses(t, "pi")
}

func TestInitialWorkerSelectionUsesCurrentDriverContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		defaultHarness string
		harness        string
		model          string
		modelProvided  bool
		driverHarness  string
		driverModel    string
		wantHarness    string
		wantModel      string
		wantError      string
	}{
		{name: "current Claude Code", defaultHarness: "current", driverHarness: "claude-code", driverModel: "claude-exact", wantHarness: "claude-code", wantModel: "claude-exact"},
		{name: "current Pi", defaultHarness: "current", driverHarness: "pi", driverModel: "openai-codex/gpt-5.6-sol", wantHarness: "pi", wantModel: "openai-codex/gpt-5.6-sol"},
		{name: "current Codex", defaultHarness: "current", driverHarness: "codex", driverModel: "gpt-5.6-codex", wantHarness: "codex", wantModel: "gpt-5.6-codex"},
		{name: "current missing context", defaultHarness: "current", wantError: "SHEPHRD_DRIVER_HARNESS is required"},
		{name: "current invalid context", defaultHarness: "current", driverHarness: "cursor", wantError: "SHEPHRD_DRIVER_HARNESS must be"},
		{name: "static default remains", defaultHarness: "claude-code", wantHarness: "claude-code"},
		{name: "explicit harness wins without context", defaultHarness: "current", harness: "codex", wantHarness: "codex"},
		{name: "same harness forwards driver model", defaultHarness: "claude-code", harness: "pi", driverHarness: "pi", driverModel: "pi-exact", wantHarness: "pi", wantModel: "pi-exact"},
		{name: "cross harness omits driver model", defaultHarness: "claude-code", harness: "codex", driverHarness: "pi", driverModel: "pi-exact", wantHarness: "codex"},
		{name: "explicit model wins across harnesses", defaultHarness: "current", harness: "codex", model: "codex-exact", modelProvided: true, driverHarness: "pi", driverModel: "pi-exact", wantHarness: "codex", wantModel: "codex-exact"},
		{name: "explicit empty model wins", defaultHarness: "current", harness: "pi", modelProvided: true, driverHarness: "pi", driverModel: "pi-exact", wantHarness: "pi"},
		{name: "invalid static context is ignored", defaultHarness: "pi", driverHarness: "cursor", driverModel: "untrusted", wantHarness: "pi"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := Service{Config: config.Config{DefaultHarness: test.defaultHarness}, environment: driverEnvironment(test.driverHarness, test.driverModel)}
			harness, model, err := service.resolveInitialWorkerSelection("", test.harness, test.model, test.modelProvided)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if harness != test.wantHarness || model != test.wantModel {
				t.Fatalf("selection = %q/%q, want %q/%q", harness, model, test.wantHarness, test.wantModel)
			}
		})
	}
}

func TestConfiguredInitialSelectionPrecedence(t *testing.T) {
	service := Service{Config: config.Config{DefaultHarness: "pi", DefaultModel: "openai-codex/gpt-6-sol:xhigh", RepositoryModels: map[string]config.ModelSelection{
		"repo_glide": {Harness: "claude-code", Model: "claude-opus-5-5"},
	}}, environment: driverEnvironment("pi", "caller-model")}
	for _, test := range []struct {
		name, repo, harness, model, wantHarness, wantModel string
		provided                                           bool
	}{
		{"global", "repo_other", "", "", "pi", "openai-codex/gpt-6-sol:xhigh", false},
		{"general", "", "", "", "pi", "openai-codex/gpt-6-sol:xhigh", false},
		{"repo", "repo_glide", "", "", "claude-code", "claude-opus-5-5", false},
		{"explicit harness", "repo_glide", "pi", "", "pi", "openai-codex/gpt-6-sol:xhigh", false},
		{"other harness", "repo_glide", "codex", "", "codex", "", false},
		{"explicit model", "repo_glide", "", "custom", "claude-code", "custom", true},
		{"native default", "repo_glide", "", "", "claude-code", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness, modelID, err := service.resolveInitialWorkerSelection(test.repo, test.harness, test.model, test.provided)
			if err != nil || harness != test.wantHarness || modelID != test.wantModel {
				t.Fatalf("selection = %q/%q, err %v; want %q/%q", harness, modelID, err, test.wantHarness, test.wantModel)
			}
		})
	}
	attempt := model.Attempt{Harness: "pi", Model: "stored"}
	if harness, modelID := service.resolveRetryWorkerSelection(attempt, "", "", false); harness != "pi" || modelID != "stored" {
		t.Fatalf("retry = %q/%q", harness, modelID)
	}
}

func TestRetryWorkerSelectionPreservesOnlyCompatibleAttemptValues(t *testing.T) {
	t.Parallel()
	attempt := model.Attempt{Harness: "pi", Model: "old-pi-model"}
	for _, test := range []struct {
		name          string
		harness       string
		model         string
		provided      bool
		driverHarness string
		driverModel   string
		wantHarness   string
		wantModel     string
	}{
		{name: "same harness inherits", wantHarness: "pi", wantModel: "old-pi-model"},
		{name: "same harness explicit override", model: "new-pi-model", provided: true, wantHarness: "pi", wantModel: "new-pi-model"},
		{name: "same harness explicit native default", provided: true, wantHarness: "pi"},
		{name: "cross harness uses matching driver model", harness: "claude-code", driverHarness: "claude-code", driverModel: "claude-exact", wantHarness: "claude-code", wantModel: "claude-exact"},
		{name: "cross harness omits old and mismatched driver models", harness: "codex", driverHarness: "pi", driverModel: "pi-exact", wantHarness: "codex"},
		{name: "cross harness explicit model wins", harness: "codex", model: "codex-exact", provided: true, driverHarness: "pi", driverModel: "pi-exact", wantHarness: "codex", wantModel: "codex-exact"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := Service{environment: driverEnvironment(test.driverHarness, test.driverModel)}
			harness, model := service.resolveRetryWorkerSelection(attempt, test.harness, test.model, test.provided)
			if harness != test.wantHarness || model != test.wantModel {
				t.Fatalf("selection = %q/%q, want %q/%q", harness, model, test.wantHarness, test.wantModel)
			}
		})
	}
}

func driverEnvironment(harness, model string) environmentCapability {
	values := map[string]string{"SHEPHRD_DRIVER_HARNESS": harness, "SHEPHRD_DRIVER_MODEL": model}
	return environmentCapability{get: func(name string) string { return values[name] }}
}

func newModelRetryService(t *testing.T) (*store.Store, model.Task, model.Attempt, Service) {
	t.Helper()
	root := t.TempDir()
	if _, err := gitFact(root, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitFact(root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "model", Objective: "model"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "Luna/Sol")
	if err != nil {
		t.Fatal(err)
	}
	worktreeRoot := filepath.Join(root, "worktrees")
	workerPath := filepath.Join(worktreeRoot, repo.ID, attempt.ID, "demo")
	if err := os.MkdirAll(filepath.Dir(workerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := gitFact(root, "worktree", "add", "-b", "branch", workerPath, "main"); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", workerPath, "", "branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "choose", NextSteps: []string{"get answer"}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "choose", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "choose"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	service := New(config.Config{DefaultHarness: "pi", DataDir: filepath.Join(root, "data"), WorktreeRoot: worktreeRoot}, state)
	return state, task, attempt, service
}

func TestInitialSpawnPersistsResolvedDriverSelection(t *testing.T) {
	installFakePi(t)
	root := t.TempDir()
	if _, err := gitFact(root, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitFact(root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "current", Objective: "model", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	service := New(config.Config{DefaultHarness: "current", DataDir: filepath.Join(root, "data"), WorktreeRoot: filepath.Join(root, "worktrees")}, state)
	t.Setenv("SHEPHRD_DRIVER_HARNESS", "pi")
	t.Setenv("SHEPHRD_DRIVER_MODEL", "provider/exact-model")
	result, err := service.SpawnWithModelSelection(task.ID, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempt.Harness != "pi" || result.Attempt.Model != "provider/exact-model" {
		t.Fatalf("attempt selection = %q/%q", result.Attempt.Harness, result.Attempt.Model)
	}
	stored, err := state.Attempt(result.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Harness != result.Attempt.Harness || stored.Model != result.Attempt.Model {
		t.Fatalf("stored selection = %q/%q", stored.Harness, stored.Model)
	}
	if stored.RuntimeExecutable != os.Getenv("SHEPHRD_EXECUTABLE") {
		t.Fatalf("runtime executable = %q", stored.RuntimeExecutable)
	}
	reapRunner(t, result.Attempt.RunnerPID)
}

func TestRetryInheritsAndOverridesAttemptModel(t *testing.T) {
	for _, test := range []struct {
		name     string
		model    string
		provided bool
		want     string
	}{
		{name: "inherits", want: "Luna/Sol"},
		{name: "overrides", model: "Sol", provided: true, want: "Sol"},
		{name: "empty override uses default", model: "", provided: true, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			installFakePi(t)
			state, task, _, service := newModelRetryService(t)
			defer state.Close()
			result, err := service.RetryWithModelSelection(task.ID, "", test.model, test.provided)
			if err != nil {
				t.Fatal(err)
			}
			if result.Attempt.Model != test.want {
				t.Fatalf("model = %q, want %q", result.Attempt.Model, test.want)
			}
			reapRunner(t, result.Attempt.RunnerPID)
		})
	}
}

func TestCrossHarnessRetryDoesNotInheritOpaqueModel(t *testing.T) {
	installFakeHarnesses(t, "pi", "claude")
	state, task, _, service := newModelRetryService(t)
	defer state.Close()
	t.Setenv("SHEPHRD_DRIVER_HARNESS", "pi")
	t.Setenv("SHEPHRD_DRIVER_MODEL", "current-pi-model")
	result, err := service.RetryWithModelSelection(task.ID, "claude-code", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempt.Harness != "claude-code" || result.Attempt.Model != "" {
		t.Fatalf("retry selection = %q/%q", result.Attempt.Harness, result.Attempt.Model)
	}
	reapRunner(t, result.Attempt.RunnerPID)
}

func TestRelaunchKeepsAttemptModel(t *testing.T) {
	installFakePi(t)
	state, task, _, service := newModelRetryService(t)
	defer state.Close()
	if err := state.Stop(task.ID, "prepare relaunch"); err != nil {
		t.Fatal(err)
	}
	result, err := service.Relaunch(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempt.Model != "Luna/Sol" {
		t.Fatalf("model = %q", result.Attempt.Model)
	}
	brief, err := os.ReadFile(result.BriefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(brief), "Luna/Sol") {
		t.Fatalf("brief = %q", brief)
	}
	reapRunner(t, result.Attempt.RunnerPID)
}
