package store

import (
	"path/filepath"
	"reflect"
	"testing"

	"shephrd/internal/model"
)

func TestTerminalAttemptRuntimeStateIsDurable(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "runtime", Objective: "visible"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetRuntimeBackend(attempt.ID, "herdr"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetRuntimeExecutableForRun(attempt.ID, attempt.RunGeneration, "/configured/shephrd"); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetTerminalEndpointForRun(attempt.ID, attempt.RunGeneration, model.TerminalEndpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}); err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RuntimeBackend != "herdr" || stored.RuntimeGeneration != 1 || stored.RuntimeExecutable != "/configured/shephrd" || stored.TerminalEndpoint == nil || stored.TerminalEndpoint.Backend != "herdr" || stored.TerminalEndpoint.SocketPath != "/socket" || stored.TerminalEndpoint.WorkspaceID != "w7" || stored.TerminalEndpoint.TabID != "w7:t2" || stored.TerminalEndpoint.PaneID != "w7:p2" {
		t.Fatalf("attempt = %+v", stored)
	}
	if _, err := state.SetTerminalEndpointForRun(attempt.ID, attempt.RunGeneration, model.TerminalEndpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t3", PaneID: "w7:p3"}); err != nil {
		t.Fatal(err)
	}
	stored, _ = state.Attempt(attempt.ID)
	if stored.RuntimeGeneration != 2 || stored.TerminalEndpoint == nil || stored.TerminalEndpoint.TabID != "w7:t3" || stored.TerminalEndpoint.PaneID != "w7:p3" {
		t.Fatalf("replacement endpoint = %+v", stored)
	}

	cmuxTask, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "cmux-runtime", Objective: "visible cmux"})
	if err != nil {
		t.Fatal(err)
	}
	cmuxAttempt, err := state.BeginAttempt(cmuxTask.ID, "codex", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetRuntimeBackend(cmuxAttempt.ID, "cmux"); err != nil {
		t.Fatal(err)
	}
	cmuxEndpoint := model.TerminalEndpoint{Backend: "cmux", SocketPath: "/cmux.sock", WindowID: "window-uuid", WorkspaceID: "workspace-uuid", PaneID: "pane-uuid", SurfaceID: "surface-uuid", ProviderVersion: "0.64.22", ProtocolVersion: "2", Capabilities: []string{"surface.read_text", "workspace.close"}}
	if _, err := state.SetTerminalEndpointForRun(cmuxAttempt.ID, cmuxAttempt.RunGeneration, cmuxEndpoint); err != nil {
		t.Fatal(err)
	}
	stored, err = state.Attempt(cmuxAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RuntimeBackend != "cmux" || stored.RuntimeGeneration != 1 || stored.TerminalEndpoint == nil || !reflect.DeepEqual(*stored.TerminalEndpoint, cmuxEndpoint) {
		t.Fatalf("cmux attempt = %+v", stored)
	}
}

func TestTerminalCreateIntentIsDurableAndFenced(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "intent", Objective: "durable intent"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetRuntimeBackend(attempt.ID, "herdr"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent := model.TerminalCreateIntent{Backend: "herdr", Source: "shephrd:attempt:1", WorkspaceID: "w7", CWD: filepath.Join(t.TempDir(), "worktree"), Label: "label", Generation: 1}
	if err := state.SetTerminalCreateIntentForRun(attempt.ID, generation, intent); err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalCreateIntent == nil || stored.TerminalCreateIntent.State != model.TerminalCreateIntentPending || stored.TerminalCreateIntent.RunGeneration != generation || stored.TerminalCreateIntent.Source != intent.Source || stored.TerminalCreateIntent.WorkspaceID != "w7" || stored.TerminalCreateIntent.Generation != 1 || stored.TerminalCreateIntent.UpdatedAt == "" {
		t.Fatalf("intent = %+v", stored.TerminalCreateIntent)
	}
	if err := state.ResolveTerminalCreateIntentForRun(attempt.ID, generation, model.TerminalCreateIntentCommitted); err != nil {
		t.Fatal(err)
	}
	stored, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalCreateIntent == nil || stored.TerminalCreateIntent.State != model.TerminalCreateIntentCommitted {
		t.Fatalf("intent = %+v", stored.TerminalCreateIntent)
	}
	if err := state.ResolveTerminalCreateIntentForRun(attempt.ID, generation, model.TerminalCreateIntentAborted); err == nil {
		t.Fatal("resolved intent resolved again")
	}
	if err := state.SetTerminalCreateIntentForRun(attempt.ID, generation, intent); err != nil {
		t.Fatal(err)
	}
	if err := state.ResolveTerminalCreateIntentForRun(attempt.ID, generation+1, model.TerminalCreateIntentAborted); err == nil {
		t.Fatal("stale run generation resolved the intent")
	}
	if err := state.ResolveTerminalCreateIntentForRun(attempt.ID, generation, "pending"); err == nil {
		t.Fatal("invalid resolution state accepted")
	}
	for _, invalid := range []model.TerminalCreateIntent{
		{Backend: "headless", Source: "s", CWD: "/abs", Label: "l", Generation: 1},
		{Backend: "herdr", Source: "bad source", CWD: "/abs", Label: "l", Generation: 1},
		{Backend: "herdr", Source: "s", CWD: "relative", Label: "l", Generation: 1},
		{Backend: "herdr", Source: "s", CWD: "/abs", Label: "l"},
	} {
		if err := state.SetTerminalCreateIntentForRun(attempt.ID, generation, invalid); err == nil {
			t.Fatalf("invalid intent accepted: %+v", invalid)
		}
	}
	stored, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TerminalCreateIntent == nil || stored.TerminalCreateIntent.State != model.TerminalCreateIntentPending {
		t.Fatalf("intent changed by invalid sets: %+v", stored.TerminalCreateIntent)
	}
}
