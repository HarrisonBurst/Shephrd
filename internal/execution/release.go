package execution

import (
	"os"
	"slices"
	"strings"

	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/gitcmd"
)

var ErrDirty = fault.New("workspace_dirty", "the workspace has uncommitted changes")

// RemoveWorkspace removes one attempt's workspace after re-verifying it is
// exactly the recorded one. Without force a workspace with uncommitted
// changes is kept and ErrDirty returned; files already sealed as report
// snapshots do not count. Branches are never deleted.
func RemoveWorkspace(repo string, a *coord.Attempt, force bool, sealed []string) error {
	if a.Branch == "" {
		if err := SetReadOnly(a.Workspace, false); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.RemoveAll(a.Workspace)
	}
	if err := VerifyWorkspace(repo, a.Workspace, a.Branch, a.Base); err != nil {
		return err
	}
	if !force {
		status, err := gitcmd.Run(a.Workspace, "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(status, "\n") {
			if line != "" && !slices.Contains(sealed, strings.TrimSpace(line[2:])) {
				return ErrDirty
			}
		}
		force = status != ""
	}
	if err := SetReadOnly(a.Workspace, false); err != nil {
		return err
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	_, err := gitcmd.Run(repo, append(args, a.Workspace)...)
	return err
}
