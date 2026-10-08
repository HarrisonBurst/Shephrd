package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	extensionhost "shephrd/internal/extension"
)

type Client struct {
	host    *extensionhost.Host
	roots   []string
	timeout time.Duration
	gitRoot func(context.Context, string) (string, error)
}

func NewClient(command []string, sha256 string, roots []string) *Client {
	return &Client{
		host: extensionhost.NewHost(extensionhost.HostConfig{
			Command: command, SHA256: sha256, ExpectedID: ExtensionID,
			ExpectedCapabilities: []extensionhost.Capability{Capability()},
		}),
		roots: append([]string(nil), roots...), timeout: 10 * time.Second, gitRoot: resolveGitRoot,
	}
}

func (c *Client) Discover(ctx context.Context) ([]Candidate, error) {
	if c == nil || c.host == nil {
		return nil, fmt.Errorf("repository discovery capability is not configured")
	}
	roots, err := canonicalRoots(c.roots)
	if err != nil {
		return nil, err
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var result Result
	if err := c.host.Invoke(ctx, CapabilityName, CapabilityVersion, DiscoverOperation, Request{Roots: roots}, &result); err != nil {
		return nil, err
	}
	if result.Candidates == nil || len(result.Candidates) > MaxCandidates {
		return nil, fmt.Errorf("repository discovery returned an invalid candidate count")
	}
	candidates := append([]Candidate(nil), result.Candidates...)
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		if seen[candidate.Path] {
			return nil, fmt.Errorf("repository discovery returned duplicate candidate %q", candidate.Path)
		}
		seen[candidate.Path] = true
		if err := c.validateCandidate(ctx, roots, candidate); err != nil {
			return nil, err
		}
	}
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].Path < candidates[right].Path })
	return candidates, nil
}

func (c *Client) validateCandidate(ctx context.Context, roots []string, candidate Candidate) error {
	if !ValidCandidate(candidate) || !utf8.ValidString(candidate.Path) || strings.ContainsRune(candidate.Path, 0) || !filepath.IsAbs(candidate.Path) || filepath.Clean(candidate.Path) != candidate.Path {
		return fmt.Errorf("repository discovery returned malformed candidate")
	}
	canonical, err := filepath.EvalSymlinks(candidate.Path)
	if err != nil || filepath.Clean(canonical) != candidate.Path {
		return fmt.Errorf("repository discovery returned non-canonical candidate %q", candidate.Path)
	}
	if !withinAnyRoot(candidate.Path, roots) {
		return fmt.Errorf("repository discovery candidate %q escapes configured roots", candidate.Path)
	}
	before, err := os.Stat(candidate.Path)
	if err != nil || !before.IsDir() {
		return fmt.Errorf("repository discovery candidate %q is unavailable", candidate.Path)
	}
	gitPath := filepath.Join(candidate.Path, ".git")
	gitEntry, err := os.Stat(gitPath)
	if err != nil || !gitEntry.IsDir() && !gitEntry.Mode().IsRegular() {
		return fmt.Errorf("repository discovery candidate %q is not a Git repository", candidate.Path)
	}
	gitRoot := c.gitRoot
	if gitRoot == nil {
		gitRoot = resolveGitRoot
	}
	root, err := gitRoot(ctx, candidate.Path)
	if err != nil {
		return fmt.Errorf("repository discovery candidate %q is not a Git repository", candidate.Path)
	}
	root, err = filepath.EvalSymlinks(strings.TrimSpace(root))
	if err != nil || filepath.Clean(root) != candidate.Path {
		return fmt.Errorf("repository discovery candidate %q did not identify its Git root", candidate.Path)
	}
	after, err := os.Stat(candidate.Path)
	if err != nil || !os.SameFile(before, after) {
		return fmt.Errorf("repository discovery candidate %q changed identity during validation", candidate.Path)
	}
	gitAfter, err := os.Stat(gitPath)
	if err != nil || !os.SameFile(gitEntry, gitAfter) {
		return fmt.Errorf("repository discovery candidate %q changed identity during validation", candidate.Path)
	}
	canonical, err = filepath.EvalSymlinks(candidate.Path)
	if err != nil || filepath.Clean(canonical) != candidate.Path || !withinAnyRoot(candidate.Path, roots) {
		return fmt.Errorf("repository discovery candidate %q changed identity during validation", candidate.Path)
	}
	return nil
}

func canonicalRoots(roots []string) ([]string, error) {
	if len(roots) == 0 || len(roots) > MaxRoots {
		return nil, fmt.Errorf("repository discovery roots are not configured")
	}
	result := make([]string, len(roots))
	seen := make(map[string]bool, len(roots))
	for index, root := range roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return nil, fmt.Errorf("repository discovery root %q is invalid", root)
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("canonicalize repository discovery root %q: %w", root, err)
			}
			canonical = root
		}
		canonical = filepath.Clean(canonical)
		if seen[canonical] {
			return nil, fmt.Errorf("repository discovery roots resolve to duplicate paths")
		}
		seen[canonical] = true
		result[index] = canonical
	}
	return result, nil
}

func withinAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func resolveGitRoot(ctx context.Context, path string) (string, error) {
	command := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--show-toplevel")
	output, err := command.Output()
	return string(output), err
}
