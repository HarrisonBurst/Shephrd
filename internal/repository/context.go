package repository

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"shephrd/internal/config"
)

type ProjectContext struct {
	RepositoryPath string              `json:"repository_path"`
	OverviewPath   string              `json:"overview_path,omitempty"`
	Memory         config.MemoryConfig `json:"memory"`
}

func InspectProjectContext(path string, memory config.MemoryConfig) (ProjectContext, error) {
	root, err := run(path, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return ProjectContext{}, fmt.Errorf("resolve repository context: %w", err)
	}
	root, err = canonical(strings.TrimSpace(root))
	if err != nil {
		return ProjectContext{}, err
	}
	overview, err := ProjectContextPath(root)
	if err != nil {
		return ProjectContext{}, err
	}
	return ProjectContext{RepositoryPath: root, OverviewPath: overview, Memory: memory}, nil
}

func ProjectContextPath(root string) (string, error) {
	const relativePath = ".shephrd/context.md"
	path := filepath.Join(root, filepath.FromSlash(relativePath))
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect project context: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("project context %s is not a regular file", path)
	}
	resolved, err := canonical(path)
	if err != nil {
		return "", err
	}
	root, err = canonical(root)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("project context %s resolves outside its repository or worktree", path)
	}
	return relativePath, nil
}
