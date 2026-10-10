package execution

import (
	"path/filepath"
	"strings"

	"shephrd/internal/fault"
	"shephrd/internal/gitcmd"
)

// InspectRepo resolves a repository's canonical root and default branch
// on the host where it lives.
func InspectRepo(path, defaultBranch string) (string, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", fault.New("not_a_repository", "%s does not exist", path)
	}
	top, err := gitcmd.Run(canonical, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fault.New("not_a_repository", "%s is not a git work tree", canonical)
	}
	if top, err = filepath.EvalSymlinks(top); err != nil || top != canonical {
		return "", "", fault.New("not_a_repository", "%s is not the root of its git work tree", canonical)
	}
	if defaultBranch == "" {
		if remoteHead, err := gitcmd.Run(canonical, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
			defaultBranch = strings.TrimPrefix(remoteHead, "origin/")
		} else if head, err := gitcmd.Run(canonical, "symbolic-ref", "--short", "HEAD"); err == nil {
			defaultBranch = head
		} else {
			return "", "", fault.New("default_branch_unknown", "cannot determine the default branch of %s", canonical).
				WithNext("repo", "add", canonical, "--default-branch", "<branch>")
		}
	}
	if _, err := gitcmd.Run(canonical, "check-ref-format", "--branch", defaultBranch); err != nil {
		return "", "", fault.New("invalid_branch", "%q is not a valid branch name", defaultBranch)
	}
	_, local := gitcmd.Run(canonical, "rev-parse", "--verify", "--quiet", "refs/heads/"+defaultBranch)
	_, remote := gitcmd.Run(canonical, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+defaultBranch)
	if local != nil && remote != nil {
		return "", "", fault.New("invalid_branch", "branch %q does not exist in %s", defaultBranch, canonical)
	}
	return canonical, defaultBranch, nil
}
