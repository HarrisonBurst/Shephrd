package store

import (
	"context"
	"fmt"
	"strings"

	"shephrd/internal/model"
)

func (s *Store) ConfigureNativeWorkspace(id string, workspace model.AttemptWorkspace) error {
	if workspace.Path == "" {
		return fmt.Errorf("workspace path must not be empty")
	}
	if workspace.Backend != model.WorkspaceBackendNative {
		return fmt.Errorf("unsupported retired workspace backend %q", workspace.Backend)
	}
	if workspace.LeaseID != "" || workspace.GitDir == "" || workspace.CommonDir == "" || workspace.Branch == "" {
		return fmt.Errorf("native worktree identity must include Git directories and a branch but no lease")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	result, err := tx.Exec(`UPDATE attempts SET session_id=?, worktree_path=?, lease_id=?, worktree_git_dir=?,
		worktree_common_dir=?, branch=?, status=?, workspace_backend=?, workspace_state=?, workspace_state_changed_at=?, updated_at=?
		WHERE id=? AND released_at IS NULL AND workspace_backend=? AND workspace_state=? AND intended_worktree_path=?`,
		workspace.SessionID, workspace.Path, workspace.LeaseID, workspace.GitDir, workspace.CommonDir, workspace.Branch,
		model.AttemptStatusWorking, workspace.Backend, model.WorkspaceStateHeld, t, t, id,
		model.WorkspaceBackendNative, model.WorkspaceStateAllocating, workspace.Path)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("attempt %s has no matching %s workspace allocation", id, workspace.Backend)
	}
	taskResult, err := tx.Exec(`UPDATE tasks SET status=?, updated_at=? WHERE current_attempt_id=?`, model.TaskStatusWorking, t, id)
	if err != nil {
		return err
	}
	if n, _ := taskResult.RowsAffected(); n == 0 {
		return fmt.Errorf("attempt %q is not current", id)
	}
	return tx.Commit()
}

// BeginNativeAllocation durably records the intent to create a native Git
// worktree at an attempt-unique path before any external side effect runs.
// The intent row is what makes a crash between here and `git worktree add`
// recoverable: recovery can classify the exact intended path instead of
// guessing which directories belong to the attempt.
func (s *Store) BeginNativeAllocation(attemptID, intendedPath string) error {
	intendedPath = strings.TrimSpace(intendedPath)
	if intendedPath == "" {
		return fmt.Errorf("native allocation requires a non-empty intended worktree path")
	}
	timestamp := now()
	result, err := s.db.Exec(`UPDATE attempts SET workspace_backend=?, workspace_state=?, intended_worktree_path=?,
		workspace_state_changed_at=?, updated_at=?
		WHERE id=? AND workspace_backend='' AND workspace_state=? AND worktree_path='' AND lease_id='' AND released_at IS NULL`,
		model.WorkspaceBackendNative, model.WorkspaceStateAllocating, intendedPath, timestamp, timestamp, attemptID, model.WorkspaceStateUnassigned)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("attempt %s already has a workspace backend or workspace; refusing to allocate a native worktree", attemptID)
	}
	return nil
}

// CompleteNativeAllocation finishes identity persistence for an allocation
// whose process crashed after `git worktree add` succeeded but before the
// held identity was stored. It records the exact re-verified worktree as held
// without touching session or runner state.
func (s *Store) CompleteNativeAllocation(id, path, gitDir, commonDir, branch string) error {
	if path == "" || gitDir == "" || commonDir == "" || branch == "" {
		return fmt.Errorf("native worktree identity must be complete before it is persisted")
	}
	t := now()
	result, err := s.db.Exec(`UPDATE attempts SET worktree_path=?, worktree_git_dir=?, worktree_common_dir=?, branch=?,
		workspace_state=?, workspace_state_changed_at=?, updated_at=?
		WHERE id=? AND workspace_backend=? AND workspace_state=? AND intended_worktree_path=? AND released_at IS NULL`,
		path, gitDir, commonDir, branch, model.WorkspaceStateHeld, t, t,
		id, model.WorkspaceBackendNative, model.WorkspaceStateAllocating, path)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("attempt %s has no matching native allocation intent for %s", id, path)
	}
	return nil
}

func (s *Store) NativeWorkspaceAttempts() ([]model.Attempt, error) {
	rows, err := s.db.Query(attemptSelect+` WHERE workspace_backend = ? ORDER BY created_at`, model.WorkspaceBackendNative)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := make([]model.Attempt, 0)
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return attempts, nil
}
