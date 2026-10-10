package execution

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"shephrd/internal/fault"
	"shephrd/internal/gitcmd"
)

type WorkspaceSpec struct {
	Repo          string `json:"repo"`
	DefaultBranch string `json:"default_branch"`
	Path          string `json:"path"`
	Branch        string `json:"branch"`
	Base          string `json:"base,omitempty"`
}

// WorkspaceError carries the workspace state an allocation failure left:
// none when nothing was created, unknown when something partial may exist.
type WorkspaceError struct {
	State string
	Err   *fault.Error
}

func (e *WorkspaceError) Error() string { return e.Err.Error() }

func refused(kind, format string, args ...any) error {
	return &WorkspaceError{State: "none", Err: fault.New(kind, format, args...)}
}

// PrepareWorkspace creates the attempt's worktree from a verified base and
// returns the base commit. The registered checkout is only fetched into.
func PrepareWorkspace(spec WorkspaceSpec) (string, error) {
	if spec.Repo == "" {
		if err := os.MkdirAll(spec.Path, 0o700); err != nil {
			return "", &WorkspaceError{State: "unknown", Err: fault.New("workspace_failed", "create scratch directory: %v", err)}
		}
		return "", nil
	}
	if _, err := os.Lstat(spec.Path); err == nil {
		return "", &WorkspaceError{State: "unknown", Err: fault.New("workspace_exists", "%s already exists", spec.Path)}
	}
	base := spec.Base
	if base == "" {
		var err error
		if base, err = resolveBase(spec.Repo, spec.DefaultBranch); err != nil {
			return "", err
		}
	} else if _, err := gitcmd.Run(spec.Repo, "cat-file", "-e", base+"^{commit}"); err != nil {
		return "", refused("base_unresolved", "stacked base %s is not in %s", base, spec.Repo)
	}
	if _, err := gitcmd.Run(spec.Repo, "cat-file", "-e", base+":.gitmodules"); err == nil {
		return "", refused("unsupported_repository", "repositories with submodules are not supported yet")
	}
	if attrs, err := gitcmd.Run(spec.Repo, "show", base+":.gitattributes"); err == nil && strings.Contains(attrs, "filter=lfs") {
		return "", refused("unsupported_repository", "repositories using Git LFS are not supported yet")
	}
	if _, err := gitcmd.Run(spec.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+spec.Branch); err == nil {
		return "", &WorkspaceError{State: "unknown", Err: fault.New("workspace_exists", "branch %s already exists", spec.Branch)}
	}
	if err := os.MkdirAll(filepath.Dir(spec.Path), 0o700); err != nil {
		return "", refused("workspace_failed", "%v", err)
	}
	if _, err := gitcmd.Run(spec.Repo, "worktree", "add", "--quiet", "-b", spec.Branch, spec.Path, base); err != nil {
		_, statErr := os.Lstat(spec.Path)
		_, branchErr := gitcmd.Run(spec.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+spec.Branch)
		state := "unknown"
		if errors.Is(statErr, fs.ErrNotExist) && branchErr != nil {
			state = "none"
		}
		return "", &WorkspaceError{State: state, Err: fault.New("workspace_failed", "%v", err)}
	}
	if err := VerifyWorkspace(spec.Repo, spec.Path, spec.Branch, base); err != nil {
		return "", &WorkspaceError{State: "unknown", Err: fault.As(err)}
	}
	return base, nil
}

func resolveBase(repo, branch string) (string, error) {
	local, localErr := gitcmd.Run(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if _, err := gitcmd.Run(repo, "remote", "get-url", "origin"); err != nil {
		if localErr != nil {
			return "", refused("base_unresolved", "branch %s does not exist in %s", branch, repo)
		}
		return local, nil
	}
	if _, err := gitcmd.Run(repo, "fetch", "--quiet", "origin", "refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		return "", refused("fetch_failed", "fetch %s from origin: %v", branch, err)
	}
	remote, err := gitcmd.Run(repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch+"^{commit}")
	switch {
	case err != nil && localErr != nil:
		return "", refused("base_unresolved", "branch %s exists neither locally nor on origin", branch)
	case err != nil:
		return local, nil
	case localErr != nil || local == remote:
		return remote, nil
	}
	if _, err := gitcmd.Run(repo, "merge-base", "--is-ancestor", local, remote); err == nil {
		return remote, nil
	}
	if _, err := gitcmd.Run(repo, "merge-base", "--is-ancestor", remote, local); err == nil {
		return local, nil
	}
	return "", refused("base_diverged", "local %s and origin/%s have diverged; reconcile them before starting work", branch, branch)
}

// VerifyWorkspace proves a worktree is exactly the one recorded.
func VerifyWorkspace(repo, path, branch, base string) error {
	mismatch := func(format string, args ...any) error {
		return fault.New("workspace_mismatch", "workspace %s: "+format, append([]any{path}, args...)...)
	}
	top, err := gitcmd.Run(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return mismatch("not a git work tree")
	}
	if resolved, err := filepath.EvalSymlinks(top); err != nil || resolved != path {
		return mismatch("its top level is %s", top)
	}
	head, err := gitcmd.Run(path, "symbolic-ref", "--short", "HEAD")
	if err != nil || head != branch {
		return mismatch("is on %q, not %s", head, branch)
	}
	if base != "" {
		if _, err := gitcmd.Run(path, "merge-base", "--is-ancestor", base, "HEAD"); err != nil {
			return mismatch("does not contain its base %s", base)
		}
	}
	common, err := gitcmd.Run(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return mismatch("has no common git directory")
	}
	want, err := gitcmd.Run(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || filepath.Clean(common) != filepath.Clean(want) {
		return mismatch("belongs to %s, not %s", common, repo)
	}
	return nil
}

func RunSetup(path, command, logPath string, env []string) error {
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = path, env, log, log
	if err := cmd.Run(); err != nil {
		return fault.New("setup_failed", "setup command failed: %v; see %s", err, logPath)
	}
	return nil
}

// SetReadOnly removes or restores write permission on everything in a
// workspace except git's own metadata.
func SetReadOnly(path string, readOnly bool) error {
	return filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && p != path {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if readOnly {
			mode &^= 0o222
		} else {
			mode |= 0o200
		}
		return os.Chmod(p, mode)
	})
}
