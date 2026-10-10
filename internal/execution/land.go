package execution

import (
	"os"
	"path/filepath"

	"shephrd/internal/fault"
	"shephrd/internal/gitcmd"
)

type Landed struct {
	Tip      string   `json:"tip"`
	Proof    string   `json:"proof"`
	Source   string   `json:"source"`
	Warnings []string `json:"warnings,omitempty"`
}

func hasRemote(repo string) bool {
	_, err := gitcmd.Run(repo, "remote", "get-url", "origin")
	return err == nil
}

// branchTip returns the default branch's tip: on origin, freshly fetched,
// when there is a remote, otherwise locally.
func branchTip(repo, branch string) (string, error) {
	if hasRemote(repo) {
		if _, err := gitcmd.Run(repo, "fetch", "--quiet", "origin", "refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
			return "", fault.New("landing_failed", "fetch %s: %v", branch, err)
		}
		return gitcmd.Run(repo, "rev-parse", "refs/remotes/origin/"+branch)
	}
	return gitcmd.Run(repo, "rev-parse", "refs/heads/"+branch)
}

func isAncestor(repo, ancestor, descendant string) bool {
	_, err := gitcmd.Run(repo, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

type LandRequest struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Commit  string `json:"commit"`
	Method  string `json:"method"`
	Message string `json:"message"`
}

// LandDirect merges a sealed commit into the default branch without
// touching the registered checkout: fast-forward only, or with a merge
// commit made in a temporary worktree. It then proves the result.
func LandDirect(req LandRequest, scratch string) (*Landed, error) {
	repo, branch, commit, method, message := req.Repo, req.Branch, req.Commit, req.Method, req.Message
	tip, err := branchTip(repo, branch)
	if err != nil {
		return nil, err
	}
	next := tip
	switch {
	case isAncestor(repo, commit, tip):
	case method == "fast-forward" && isAncestor(repo, tip, commit):
		next = commit
	case method == "fast-forward":
		return nil, fault.New("landing_failed", "%s has moved past the result's base; it cannot fast-forward. Ask the worker to rebase on %s", branch, branch)
	default:
		if next, err = mergeCommit(repo, tip, commit, message, scratch); err != nil {
			return nil, err
		}
	}
	landed := &Landed{Tip: next}
	if next != tip {
		if hasRemote(repo) {
			if _, err := gitcmd.Run(repo, "push", "--quiet", "origin", next+":refs/heads/"+branch); err != nil {
				return nil, fault.New("landing_failed", "push to %s was rejected: %v", branch, err)
			}
		} else {
			if _, err := gitcmd.Run(repo, "update-ref", "refs/heads/"+branch, next, tip); err != nil {
				return nil, fault.New("landing_failed", "%s moved while landing: %v", branch, err)
			}
			if head, err := gitcmd.Run(repo, "symbolic-ref", "--short", "HEAD"); err == nil && head == branch {
				landed.Warnings = append(landed.Warnings, "the registered checkout has "+branch+" checked out; its working tree is now behind the branch")
			}
		}
	}
	proof, err := ProveAncestry(repo, branch, commit)
	if err != nil {
		return nil, err
	}
	if !proof {
		return nil, fault.New("landing_failed", "%s does not contain %s after landing", branch, commit)
	}
	landed.Proof, landed.Source = "ancestry", "git"
	return landed, nil
}

func mergeCommit(repo, tip, commit, message, scratch string) (string, error) {
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(scratch, "land-")
	if err != nil {
		return "", err
	}
	worktree := filepath.Join(dir, "merge")
	defer func() {
		gitcmd.Run(repo, "worktree", "remove", "--force", worktree)
		os.RemoveAll(dir)
	}()
	if _, err := gitcmd.Run(repo, "worktree", "add", "--quiet", "--detach", worktree, tip); err != nil {
		return "", fault.New("landing_failed", "prepare merge: %v", err)
	}
	identity := []string{}
	if _, err := gitcmd.Run(worktree, "config", "user.email"); err != nil {
		identity = []string{"-c", "user.name=Shephrd", "-c", "user.email=shephrd@localhost"}
	}
	if _, err := gitcmd.Run(worktree, append(identity, "merge", "--quiet", "--no-ff", "-m", message, commit)...); err != nil {
		gitcmd.Run(worktree, "merge", "--abort")
		return "", fault.New("landing_failed", "merge conflict; ask the worker to rebase: %v", err)
	}
	return gitcmd.Run(worktree, "rev-parse", "HEAD")
}

// ProveAncestry checks that a commit is reachable from the default branch,
// on origin when there is a remote. It never changes history.
func ProveAncestry(repo, branch, commit string) (bool, error) {
	tip, err := branchTip(repo, branch)
	if err != nil {
		return false, err
	}
	return isAncestor(repo, commit, tip), nil
}
