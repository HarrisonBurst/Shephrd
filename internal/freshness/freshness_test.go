package freshness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFinishAssignsStableExitCodes(t *testing.T) {
	for _, test := range []struct {
		status string
		code   int
	}{
		{StatusCurrent, 0},
		{StatusStale, 1},
		{StatusModifiedSource, 2},
		{StatusUnverifiable, 3},
		{StatusNonRepositoryInstall, 4},
	} {
		report := finish(Report{}, test.status, "reason")
		if report.Status != test.status || report.ExitCode != test.code || report.Reason != "reason" {
			t.Fatalf("finish(%q) = %+v", test.status, report)
		}
	}
}

func TestGoRunPathDetectionIsSpecificToGoBuildDirectories(t *testing.T) {
	for _, test := range []struct {
		path string
		want bool
	}{
		{filepath.Join(string(filepath.Separator), "tmp", "go-build123", "b001", "exe", "shephrd"), true},
		{filepath.Join(string(filepath.Separator), "tmp", "gobin", "shephrd"), false},
		{filepath.Join(string(filepath.Separator), "repo", "bin", "shephrd"), false},
	} {
		if got := isGoRunPath(test.path); got != test.want {
			t.Fatalf("isGoRunPath(%q) = %t, want %t", test.path, got, test.want)
		}
	}
}

func TestGitMarkerAndEnvironmentAreIndependentOfCallerGitContext(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "bin", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if found, err := hasGitMarker(nested); err != nil || !found {
		t.Fatalf("hasGitMarker = %t, %v", found, err)
	}
	t.Setenv("GIT_DIR", "/caller/repository")
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	optionalLocksDisabled := false
	for _, value := range gitEnvironment() {
		if strings.HasPrefix(value, "GIT_DIR=") || value == "GIT_OPTIONAL_LOCKS=1" {
			t.Fatalf("git environment retained caller override %q", value)
		}
		optionalLocksDisabled = optionalLocksDisabled || value == "GIT_OPTIONAL_LOCKS=0"
	}
	if !optionalLocksDisabled {
		t.Fatal("git optional locks were not disabled")
	}
}

func TestReadBuildReturnsThisGoBinaryMetadata(t *testing.T) {
	build := readBuild(os.Args[0])
	if !build.Available || build.GoVersion == "" || build.Module == "" {
		t.Fatalf("build = %+v", build)
	}
}
