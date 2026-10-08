package store

import (
	"path/filepath"
	"testing"

	"shephrd/internal/model"
)

func (s *Store) ConfigureAttempt(id, sessionID, worktreePath, leaseID, branch string) error {
	if err := s.BeginNativeAllocation(id, worktreePath); err != nil {
		return err
	}
	return s.ConfigureNativeWorkspace(id, model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: sessionID,
		Path: worktreePath, GitDir: filepath.Join(worktreePath, ".git-admin-"+id), CommonDir: filepath.Join(worktreePath, ".git-common"), Branch: branch})
}

func (s *Store) MarkReleased(attemptID string) error {
	return s.MarkReleasedWithReason(attemptID, "native test workspace released")
}

func prepareAttempt(t *testing.T, state *Store, attempt model.Attempt) int {
	t.Helper()
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	return generation
}

func recordWorkerCheckpoint(t *testing.T, state *Store, attempt model.Attempt, generation int, cursor int64, nextSteps []string) {
	t.Helper()
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "checkpoint", NextSteps: nextSteps}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, cursor, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
}
