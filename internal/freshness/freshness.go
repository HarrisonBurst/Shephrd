package freshness

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	StatusCurrent              = "current"
	StatusStale                = "stale"
	StatusModifiedSource       = "modified_source"
	StatusUnverifiable         = "unverifiable"
	StatusNonRepositoryInstall = "non_repository_install"
)

type Build struct {
	Available           bool   `json:"available"`
	ProvenanceAvailable bool   `json:"provenance_available"`
	GoVersion           string `json:"go_version,omitempty"`
	Module              string `json:"module,omitempty"`
	Version             string `json:"version,omitempty"`
	VCS                 string `json:"vcs,omitempty"`
	Revision            string `json:"revision,omitempty"`
	Time                string `json:"time,omitempty"`
	Modified            *bool  `json:"modified,omitempty"`
}

type Repository struct {
	Path  string `json:"path"`
	Head  string `json:"head"`
	Dirty bool   `json:"dirty"`
}

type Report struct {
	SchemaVersion  int         `json:"schema_version"`
	Status         string      `json:"status"`
	ExitCode       int         `json:"exit_code"`
	Reason         string      `json:"reason"`
	InvokedPath    string      `json:"invoked_path"`
	ExecutablePath string      `json:"executable_path"`
	ResolvedPath   string      `json:"resolved_path"`
	Location       string      `json:"location"`
	Build          Build       `json:"build"`
	Repository     *Repository `json:"repository"`
}

func Inspect(path string) Report {
	report := Report{SchemaVersion: 1}
	invoked, executable, resolved, err := executablePaths(path)
	report.InvokedPath, report.ExecutablePath, report.ResolvedPath = invoked, executable, resolved
	if err == nil {
		report.Build = readBuild(resolved)
	}
	if err != nil {
		return finish(report, StatusUnverifiable, fmt.Sprintf("resolve invoked executable: %v", err))
	}
	repository, found, err := inspectRepository(resolved)
	if err != nil {
		return finish(report, StatusUnverifiable, fmt.Sprintf("inspect executable repository: %v", err))
	}
	if !found {
		if isGoRunPath(resolved) {
			report.Location = "go_run"
			return finish(report, StatusUnverifiable, "direct go run uses a temporary executable outside its source checkout")
		}
		report.Location = "external"
		return finish(report, StatusNonRepositoryInstall, "executable is installed outside a Git source checkout")
	}
	report.Location = "repository"
	report.Repository = repository
	if !report.Build.ProvenanceAvailable || report.Build.VCS != "git" {
		return finish(report, StatusUnverifiable, "executable has no usable embedded Git build provenance")
	}
	if report.Build.Modified != nil && *report.Build.Modified {
		return finish(report, StatusModifiedSource, "executable was built from modified source")
	}
	var differences []string
	if report.Build.Revision != repository.Head {
		differences = append(differences, fmt.Sprintf("build revision %s does not match repository HEAD %s", report.Build.Revision, repository.Head))
	}
	if repository.Dirty {
		differences = append(differences, "repository has uncommitted changes")
	}
	if len(differences) > 0 {
		return finish(report, StatusStale, strings.Join(differences, "; "))
	}
	return finish(report, StatusCurrent, "build revision matches the clean repository HEAD")
}

func finish(report Report, status, reason string) Report {
	report.Status, report.Reason = status, reason
	switch status {
	case StatusCurrent:
		report.ExitCode = 0
	case StatusStale:
		report.ExitCode = 1
	case StatusModifiedSource:
		report.ExitCode = 2
	case StatusUnverifiable:
		report.ExitCode = 3
	case StatusNonRepositoryInstall:
		report.ExitCode = 4
	}
	return report
}

func readBuild(path string) Build {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return Build{}
	}
	build := Build{Available: true, GoVersion: info.GoVersion, Module: info.Main.Path, Version: info.Main.Version}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs":
			build.VCS = setting.Value
		case "vcs.revision":
			build.Revision = setting.Value
		case "vcs.time":
			build.Time = setting.Value
		case "vcs.modified":
			if modified, err := strconv.ParseBool(setting.Value); err == nil {
				build.Modified = &modified
			}
		}
	}
	build.ProvenanceAvailable = build.VCS != "" && build.Revision != "" && build.Modified != nil
	return build
}

func executablePaths(path string) (string, string, string, error) {
	invoked, err := invokedExecutable(path)
	if err != nil {
		return invoked, invoked, "", err
	}
	resolved, err := filepath.EvalSymlinks(invoked)
	if err != nil {
		return filepath.Clean(invoked), filepath.Clean(invoked), "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return filepath.Clean(invoked), filepath.Clean(invoked), "", err
	}
	return filepath.Clean(invoked), filepath.Clean(invoked), filepath.Clean(resolved), nil
}

func invokedExecutable(argv0 string) (string, error) {
	if strings.TrimSpace(argv0) == "" {
		return "", fmt.Errorf("argv[0] is empty")
	}
	path := argv0
	if !filepath.IsAbs(path) && !strings.ContainsRune(path, filepath.Separator) {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return path, err
		}
		path = resolved
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path, err
	}
	return filepath.Clean(absolute), nil
}

func inspectRepository(executable string) (*Repository, bool, error) {
	found, err := hasGitMarker(filepath.Dir(executable))
	if err != nil || !found {
		return nil, found, err
	}
	rootOutput, err := runGit(filepath.Dir(executable), "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, false, commandError(rootOutput, err)
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(rootOutput))
	if err != nil {
		return nil, false, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, false, err
	}
	headOutput, err := runGit(root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, false, commandError(headOutput, err)
	}
	statusOutput, err := runGit(root, "status", "--porcelain=v1", "--untracked-files=normal", "--ignore-submodules=none")
	if err != nil {
		return nil, false, commandError(statusOutput, err)
	}
	return &Repository{Path: filepath.Clean(root), Head: strings.TrimSpace(headOutput), Dirty: strings.TrimSpace(statusOutput) != ""}, true, nil
}

func hasGitMarker(path string) (bool, error) {
	for {
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false, nil
		}
		path = parent
	}
}

func runGit(dir string, args ...string) (string, error) {
	arguments := append([]string{"-C", dir}, args...)
	command := exec.Command("git", arguments...)
	command.Env = gitEnvironment()
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func gitEnvironment() []string {
	excluded := map[string]bool{
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_CEILING_DIRECTORIES":          true,
		"GIT_COMMON_DIR":                   true,
		"GIT_DIR":                          true,
		"GIT_INDEX_FILE":                   true,
		"GIT_OBJECT_DIRECTORY":             true,
		"GIT_OPTIONAL_LOCKS":               true,
		"GIT_WORK_TREE":                    true,
	}
	environment := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !excluded[strings.ToUpper(key)] {
			environment = append(environment, value)
		}
	}
	return append(environment, "GIT_OPTIONAL_LOCKS=0")
}

func commandError(output string, err error) error {
	if output == "" {
		return err
	}
	return fmt.Errorf("%s: %w", output, err)
}

func isGoRunPath(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for index, part := range parts {
		if strings.HasPrefix(part, "go-build") && index+1 < len(parts) {
			return true
		}
	}
	return false
}
