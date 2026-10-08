package cli

import (
	"path/filepath"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/worktree"
)

func configureAttempt(t testing.TB, state *store.Store, id, sessionID, worktreePath, leaseID, branch string) error {
	t.Helper()
	if err := state.BeginNativeAllocation(id, worktreePath); err != nil {
		return err
	}
	gitDir := filepath.Join(worktreePath, ".git-admin-"+id)
	commonDir := filepath.Join(worktreePath, ".git-common")
	attempt, attemptErr := state.Attempt(id)
	if attemptErr == nil {
		if task, taskErr := state.Task(attempt.TaskID); taskErr == nil {
			if repo, repoErr := state.Repo(task.RepoID); repoErr == nil {
				if identity, inspectErr := worktree.NewNative(filepath.Dir(worktreePath)).Inspect(repo, worktreePath, branch); inspectErr == nil {
					gitDir, commonDir = identity.GitDir, identity.CommonDir
				}
			}
		}
	}
	workspace := model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: sessionID, Path: worktreePath,
		GitDir: gitDir, CommonDir: commonDir, Branch: branch}
	return state.ConfigureNativeWorkspace(id, workspace)
}
