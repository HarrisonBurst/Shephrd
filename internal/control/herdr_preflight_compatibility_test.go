package control

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/terminal"
)

type absentHerdrEndpointRunner struct{}

func (absentHerdrEndpointRunner) Run(_ string, args ...string) ([]byte, []byte, error) {
	if len(args) >= 2 && args[0] == "pane" && args[1] == "get" {
		return []byte(`{"error":{"code":"pane_not_found","message":"gone"}}`), nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected Herdr call: %s", strings.Join(args, " "))
}

func TestHerdrSendAndRelaunchRetainParentPreflight(t *testing.T) {
	root := t.TempDir()
	selected := writeRuntimeExecutable(t, filepath.Join(root, "bin", "shephrd"), "#!/bin/sh\nexit 0\n")
	t.Setenv("SHEPHRD_EXECUTABLE", selected)
	service, state, attempt, _, _ := runtimeLaunchFixture(t, root, "herdr", "codex")
	defer state.Close()
	if _, err := state.SetTerminalEndpointForRun(attempt.ID, attempt.RunGeneration, model.TerminalEndpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}); err != nil {
		t.Fatal(err)
	}
	attempt, _ = state.Attempt(attempt.ID)
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "waiting", NextSteps: []string{"continue"}}
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "checkpoint", Payload: "waiting", Checkpoint: &checkpoint}, 1, workspaceFacts(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "question", Payload: "continue?"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if err := state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, ""); err != nil {
		t.Fatal(err)
	}
	service.TerminalProviders = []terminal.Provider{herdrClientProvider{Client: terminal.NewWithRunner("/socket", absentHerdrEndpointRunner{})}}
	for _, key := range []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_WORKSPACE_ID", "HERDR_PANE_ID"} {
		t.Setenv(key, "")
	}
	before, _ := state.Attempt(attempt.ID)
	if _, err := service.Send(attempt.TaskID, "continue"); err == nil || !strings.Contains(err.Error(), "not available in the sanitized parent context") {
		t.Fatalf("Send error = %v", err)
	}
	afterSend, _ := state.Attempt(attempt.ID)
	if afterSend.RunGeneration != before.RunGeneration {
		t.Fatalf("Send advanced run generation from %d to %d", before.RunGeneration, afterSend.RunGeneration)
	}
	if _, err := service.Relaunch(attempt.TaskID); err == nil || !strings.Contains(err.Error(), "not available in the sanitized parent context") {
		t.Fatalf("Relaunch error = %v", err)
	}
	afterRelaunch, _ := state.Attempt(attempt.ID)
	if afterRelaunch.RunGeneration != before.RunGeneration {
		t.Fatalf("Relaunch advanced run generation from %d to %d", before.RunGeneration, afterRelaunch.RunGeneration)
	}
}
