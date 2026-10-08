package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/brief"
	"shephrd/internal/model"
)

func TestRunnerDisableSupersedesStoredEnabledMemoryGuidance(t *testing.T) {
	root := t.TempDir()
	argsPath := filepath.Join(root, "args")
	streamPath := filepath.Join(root, "stream.jsonl")
	checkpoint := headlessClaudeEnvelope(t, model.Event{Type: "checkpoint", Payload: "waiting", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "waiting", NextSteps: []string{"Await the decision"}}})
	question := headlessClaudeEnvelope(t, model.Event{Type: "question", Payload: "Need a decision"})
	writeHeadlessClaudeFile(t, streamPath, claudeStream(t, "claude-session", checkpoint+question))
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" > \"$SHEPHRD_TEST_ARGS\"\ncat \"$SHEPHRD_TEST_STREAM\"\n"
	service, state, task, attempt, inputPath := headlessClaudeControlFixture(t, root, filepath.Join(root, "data"), script)
	defer state.Close()
	service.Config.Memory.Enabled = false
	stored := brief.ProjectGuidance(".shephrd/context.md", true)
	writeHeadlessClaudeFile(t, inputPath, []byte(stored))
	t.Setenv("SHEPHRD_TEST_ARGS", argsPath)
	t.Setenv("SHEPHRD_TEST_STREAM", streamPath)
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(args)
	if !strings.Contains(body, stored) || !strings.Contains(body, "## Current memory setting") || strings.Index(body, brief.MemoryDisabledInstruction) < strings.Index(body, stored)+len(stored) {
		t.Fatalf("runner did not supersede stored enabled guidance: %s", body)
	}
	current, err := state.Task(task.ID)
	if err != nil || current.Status != model.TaskStatusWaiting {
		t.Fatalf("task = %+v, err = %v", current, err)
	}
}
