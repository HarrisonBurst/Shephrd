package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"

	"shephrd/internal/config"
	"shephrd/internal/gitcmd"
)

type PackageStatus struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Dir    string `json:"dir"`
	Rev    string `json:"rev,omitempty"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

func packagesDir(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "packages")
}

func packageRoot(cfg *config.Config, name string) (string, error) {
	pkg := cfg.Packages[name]
	dir := pkg.Path
	if pkg.Git != "" {
		dir = filepath.Join(packagesDir(cfg), name)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if pkg.Git != "" {
			return "", fmt.Errorf("package %s is not fetched; run shephrd plugin sync", name)
		}
		return "", fmt.Errorf("package %s: %w", name, err)
	}
	if pkg.Git == "" {
		return root, nil
	}
	head, err := gitcmd.Run(root, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("package %s: %w", name, err)
	}
	if head != pkg.Rev {
		return "", fmt.Errorf("package %s is at %s, not its pinned %s; run shephrd plugin sync", name, head, pkg.Rev)
	}
	status, err := gitcmd.Run(root, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("package %s: %w", name, err)
	}
	if status != "" {
		return "", fmt.Errorf("package %s has local changes; run shephrd plugin sync", name)
	}
	return root, nil
}

func discover(root string) ([]string, error) {
	var listing struct {
		Plugins []string `toml:"plugins"`
	}
	_, err := toml.DecodeFile(filepath.Join(root, "shephrd-package.toml"), &listing)
	switch {
	case err == nil:
		var dirs []string
		for _, pattern := range listing.Plugins {
			matches, err := filepath.Glob(filepath.Join(root, pattern))
			if err != nil {
				return nil, fmt.Errorf("shephrd-package.toml: %w", err)
			}
			dirs = append(dirs, matches...)
		}
		return dirs, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("shephrd-package.toml: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "plugin.toml")); err == nil {
		return []string{root}, nil
	}
	matches, err := filepath.Glob(filepath.Join(root, "plugins", "*", "plugin.toml"))
	if err != nil {
		return nil, err
	}
	dirs := make([]string, len(matches))
	for i, match := range matches {
		dirs[i] = filepath.Dir(match)
	}
	return dirs, nil
}

func Sync(cfg *config.Config) ([]PackageStatus, error) {
	base := packagesDir(cfg)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cfg.Packages))
	for name := range cfg.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	statuses := []PackageStatus{}
	for _, name := range names {
		pkg := cfg.Packages[name]
		if pkg.Git == "" {
			status := PackageStatus{Name: name, Source: "path", Dir: pkg.Path, State: "ready"}
			if _, err := packageRoot(cfg, name); err != nil {
				status.State, status.Detail = "unavailable", err.Error()
			}
			statuses = append(statuses, status)
			continue
		}
		dir := filepath.Join(base, name)
		status := PackageStatus{Name: name, Source: "git", Dir: dir, Rev: pkg.Rev, State: "synced"}
		if _, err := packageRoot(cfg, name); err == nil {
			status.State = "ready"
		} else if err := fetchPackage(dir, pkg); err != nil {
			status.State, status.Detail = "failed", err.Error()
		}
		statuses = append(statuses, status)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if _, declared := cfg.Packages[entry.Name()]; declared && cfg.Packages[entry.Name()].Git != "" {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		status := PackageStatus{Name: entry.Name(), Source: "git", Dir: dir, State: "removed"}
		if err := os.RemoveAll(dir); err != nil {
			status.State, status.Detail = "failed", err.Error()
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func fetchPackage(dir string, pkg config.Package) error {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		tmp, err := os.MkdirTemp(filepath.Dir(dir), ".fetch-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		if _, err := gitcmd.Run(tmp, "clone", "--quiet", "--no-checkout", pkg.Git, "checkout"); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(tmp, "checkout"), dir); err != nil {
			return err
		}
	}
	if _, err := gitcmd.Run(dir, "cat-file", "-e", pkg.Rev+"^{commit}"); err != nil {
		if _, err := gitcmd.Run(dir, "fetch", "--quiet", "origin", pkg.Rev); err != nil {
			return err
		}
	}
	if _, err := gitcmd.Run(dir, "checkout", "--quiet", "--force", "--detach", pkg.Rev); err != nil {
		return err
	}
	_, err := gitcmd.Run(dir, "clean", "--quiet", "-fdx")
	return err
}
