package repository

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/repository/discovery"
	"shephrd/internal/store"
)

type Registry struct {
	Config config.Config
	Store  *store.Store
}

func (r Registry) Scan() ([]discovery.Candidate, error) {
	configured := r.Config.RepositoryDiscovery
	if configured == nil {
		return nil, fmt.Errorf("repository discovery capability is not configured")
	}
	client := discovery.NewClient(configured.Command, configured.SHA256, configured.Roots)
	return client.Discover(context.Background())
}

func (r Registry) Add(ref, name, contextFile, setupHook, defaultBranch string) (model.Repo, error) {
	path, err := r.resolveUnregistered(ref)
	if err != nil {
		return model.Repo{}, err
	}
	if name == "" {
		name = filepath.Base(path)
	}
	repo, err := inspect(path, name, contextFile, setupHook, defaultBranch)
	if err != nil {
		return model.Repo{}, err
	}
	return r.Store.UpsertRepo(repo)
}

func (r Registry) Resolve(ref string) (model.Repo, error) {
	if abs, err := filepath.Abs(ref); err == nil {
		if canonical, err := canonical(abs); err == nil {
			if repo, err := r.Store.RepoByPath(canonical); err == nil {
				return repo, nil
			}
		}
	}
	return r.Store.Repo(ref)
}

func (r Registry) resolveUnregistered(ref string) (string, error) {
	candidates := []string{ref}
	if r.Config.RepositoryDiscovery != nil {
		for _, root := range r.Config.RepositoryDiscovery.Roots {
			candidates = append(candidates, filepath.Join(root, ref))
		}
	}
	var found []string
	for _, candidate := range candidates {
		path, err := canonical(candidate)
		if err == nil && isGitRepo(path) {
			found = append(found, path)
		}
	}
	found = unique(found)
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) > 1 {
		return "", fmt.Errorf("repo ref %q is ambiguous; use an absolute path", ref)
	}
	if r.Config.RepositoryDiscovery != nil {
		candidates, err := r.Scan()
		if err != nil {
			return "", err
		}
		for _, candidate := range candidates {
			if filepath.Base(candidate.Path) == ref {
				found = append(found, candidate.Path)
			}
		}
	}
	found = unique(found)
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) > 1 {
		return "", fmt.Errorf("repo name %q matches multiple paths; use an absolute path", ref)
	}
	return "", fmt.Errorf("%q is not a git repository under a configured repository discovery root", ref)
}

func inspect(path, name, contextFile, setupHook, explicitDefaultBranch string) (model.Repo, error) {
	if err := config.Require("git"); err != nil {
		return model.Repo{}, err
	}
	path, err := canonical(path)
	if err != nil {
		return model.Repo{}, fmt.Errorf("normalize repo path: %w", err)
	}
	root, err := run(path, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return model.Repo{}, fmt.Errorf("%s is not a git repository", path)
	}
	path, err = canonical(strings.TrimSpace(root))
	if err != nil {
		return model.Repo{}, err
	}
	branch, err := defaultBranch(path, explicitDefaultBranch)
	if err != nil {
		return model.Repo{}, err
	}
	if contextFile == "" {
		for _, file := range []string{"AGENTS.md", "CLAUDE.md"} {
			if _, err := os.Stat(filepath.Join(path, file)); err == nil {
				contextFile = file
				break
			}
		}
	} else if !filepath.IsAbs(contextFile) {
		contextFile = filepath.Join(path, contextFile)
	}
	return model.Repo{Name: name, Path: path, DefaultBranch: branch, ContextFile: contextFile, SetupHook: setupHook}, nil
}

func defaultBranch(path, explicit string) (string, error) {
	branch := strings.TrimSpace(explicit)
	if branch == "" {
		if output, err := run(path, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
			_, branch, _ = strings.Cut(strings.TrimSpace(output), "/")
		}
	}
	if branch == "" {
		if output, err := run(path, "git", "symbolic-ref", "--short", "HEAD"); err == nil {
			candidate := strings.TrimSpace(output)
			if candidate == "main" || candidate == "master" {
				branch = candidate
			}
		}
	}
	if branch == "" {
		return "", fmt.Errorf("default branch is ambiguous without origin/HEAD; pass --default-branch with an existing local branch")
	}
	if _, err := run(path, "git", "check-ref-format", "--branch", branch); err != nil {
		return "", fmt.Errorf("invalid default branch %q", branch)
	}
	ref := "refs/heads/" + branch + "^{commit}"
	if _, err := run(path, "git", "rev-parse", "--verify", ref); err != nil {
		return "", fmt.Errorf("default branch ref refs/heads/%s does not resolve to a commit", branch)
	}
	return branch, nil
}

func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func isGitRepo(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}

func run(dir, command string, args ...string) (string, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func unique(values []string) []string {
	seen := make(map[string]bool)
	out := values[:0]
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
