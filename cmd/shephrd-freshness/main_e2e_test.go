package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"shephrd/internal/freshness"
)

func TestStandaloneFreshnessE2E(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	copyFreshnessProject(t, projectRoot, checkout)
	runFreshnessSetup(t, checkout, "git", "init", "-b", "main")
	runFreshnessSetup(t, checkout, "git", "add", ".")
	runFreshnessSetup(t, checkout, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	binDir := filepath.Join(checkout, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, "shephrd")
	diagnostic := filepath.Join(root, "shephrd-freshness")
	runFreshnessSetup(t, checkout, "go", "build", "-o", binary, "./cmd/shephrd")
	runFreshnessSetup(t, checkout, "go", "build", "-o", diagnostic, "./cmd/shephrd-freshness")
	head := strings.TrimSpace(runFreshnessSetup(t, checkout, "git", "rev-parse", "HEAD"))
	linkDir := filepath.Join(root, "home", "bin")
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "shephrd")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	canonicalBinary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	canonicalCheckout, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	blockedConfig := filepath.Join(root, "config-is-a-directory")
	if err := os.Mkdir(blockedConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "SHEPHRD_CONFIG="+blockedConfig)
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	current := runFreshnessDiagnostic(t, diagnostic, link, outside, environment, 0)
	if current.Status != freshness.StatusCurrent || current.ExitCode != 0 || current.InvokedPath != link || current.ExecutablePath == "" || current.ResolvedPath != canonicalBinary || current.Location != "repository" {
		t.Fatalf("current report = %+v", current)
	}
	if current.Repository == nil || current.Repository.Path != canonicalCheckout || current.Repository.Head != head || current.Repository.Dirty {
		t.Fatalf("current repository = %+v", current.Repository)
	}
	if !current.Build.ProvenanceAvailable || current.Build.Modified == nil || *current.Build.Modified {
		t.Fatalf("current build = %+v", current.Build)
	}

	readme := filepath.Join(checkout, "README.md")
	appendFreshnessFile(t, readme, "\ndirty\n")
	dirty := runFreshnessDiagnostic(t, diagnostic, binary, outside, environment, 1)
	if dirty.Status != freshness.StatusStale || dirty.ExitCode != 1 || dirty.Repository == nil || !dirty.Repository.Dirty {
		t.Fatalf("dirty report = %+v", dirty)
	}

	runFreshnessSetup(t, checkout, "git", "add", "README.md")
	runFreshnessSetup(t, checkout, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "advance source")
	stale := runFreshnessDiagnostic(t, diagnostic, binary, outside, environment, 1)
	if stale.Status != freshness.StatusStale || stale.ExitCode != 1 || stale.Repository == nil || stale.Repository.Dirty || stale.Build.Revision == stale.Repository.Head {
		t.Fatalf("stale report = %+v", stale)
	}

	appendFreshnessFile(t, readme, "modified source\n")
	modifiedBinary := filepath.Join(binDir, "modified")
	runFreshnessSetup(t, checkout, "go", "build", "-o", modifiedBinary, "./cmd/shephrd")
	modified := runFreshnessDiagnostic(t, diagnostic, modifiedBinary, outside, environment, 2)
	if modified.Status != freshness.StatusModifiedSource || modified.ExitCode != 2 || modified.Build.Modified == nil || !*modified.Build.Modified {
		t.Fatalf("modified report = %+v", modified)
	}

	unversionedBinary := filepath.Join(binDir, "unversioned")
	runFreshnessSetup(t, checkout, "go", "build", "-buildvcs=false", "-ldflags=-s -w", "-o", unversionedBinary, "./cmd/shephrd")
	unversioned := runFreshnessDiagnostic(t, diagnostic, unversionedBinary, outside, environment, 3)
	if unversioned.Status != freshness.StatusUnverifiable || unversioned.ExitCode != 3 || unversioned.Build.ProvenanceAvailable {
		t.Fatalf("unversioned report = %+v", unversioned)
	}

	runFreshnessSetup(t, checkout, "git", "checkout", "--", "README.md")
	strippedBinary := filepath.Join(binDir, "stripped")
	runFreshnessSetup(t, checkout, "go", "build", "-ldflags=-s -w", "-o", strippedBinary, "./cmd/shephrd")
	stripped := runFreshnessDiagnostic(t, diagnostic, strippedBinary, outside, environment, 0)
	if stripped.Status != freshness.StatusCurrent || !stripped.Build.ProvenanceAvailable {
		t.Fatalf("stripped report = %+v", stripped)
	}

	goBin := filepath.Join(root, "gobin")
	goBinEnvironment := append(environment, "GOBIN="+goBin)
	runFreshnessSetupEnv(t, checkout, goBinEnvironment, "go", "install", "./cmd/shephrd")
	installed := runFreshnessDiagnostic(t, diagnostic, filepath.Join(goBin, "shephrd"), outside, environment, 4)
	if installed.Status != freshness.StatusNonRepositoryInstall || installed.ExitCode != 4 || installed.Location != "external" || installed.Repository != nil || !installed.Build.ProvenanceAvailable {
		t.Fatalf("installed report = %+v", installed)
	}

	command := exec.Command("go", "run", "./cmd/shephrd-freshness", "--json", binary)
	command.Dir, command.Env = checkout, environment
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	var exitError *exec.ExitError
	if !strings.Contains(stderr.String(), "exit status 1") || !asExitError(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("go run exit = %v, stderr = %q", err, stderr.String())
	}
	var goRun freshness.Report
	if err := json.Unmarshal(stdout.Bytes(), &goRun); err != nil {
		t.Fatalf("decode go run report %q: %v", stdout.String(), err)
	}
	if goRun.Status != freshness.StatusStale || goRun.ExitCode != 1 || goRun.Repository == nil {
		t.Fatalf("go run report = %+v", goRun)
	}
}

func runFreshnessDiagnostic(t *testing.T, diagnostic, binary, dir string, environment []string, wantExit int) freshness.Report {
	t.Helper()
	command := exec.Command(diagnostic, binary, "--json")
	command.Dir, command.Env = dir, environment
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if wantExit == 0 {
		if err != nil {
			t.Fatalf("%s freshness: %s: %v", binary, stderr.String(), err)
		}
	} else {
		var exitError *exec.ExitError
		if !asExitError(err, &exitError) || exitError.ExitCode() != wantExit {
			t.Fatalf("%s freshness exit = %v, want %d; stderr = %q", binary, err, wantExit, stderr.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("%s freshness stderr = %q", binary, stderr.String())
	}
	var report freshness.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode freshness report %q: %v", stdout.String(), err)
	}
	return report
}

func asExitError(err error, target **exec.ExitError) bool {
	if err == nil {
		return false
	}
	exitError, ok := err.(*exec.ExitError)
	if ok {
		*target = exitError
	}
	return ok
}

func runFreshnessSetup(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	return runFreshnessSetupEnv(t, dir, os.Environ(), name, args...)
}

func runFreshnessSetupEnv(t *testing.T, dir string, environment []string, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir, command.Env = dir, environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %s: %v", name, args, output, err)
	}
	return string(output)
}

func appendFreshnessFile(t *testing.T, path, value string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(value); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func copyFreshnessProject(t *testing.T, source, target string) {
	t.Helper()
	err := filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == ".git" || relative == "bin" || relative == ".lavish" {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		destination := filepath.Join(target, relative)
		if info.IsDir() {
			return os.MkdirAll(destination, info.Mode().Perm())
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		inputCloseErr := input.Close()
		outputCloseErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputCloseErr != nil {
			return inputCloseErr
		}
		return outputCloseErr
	})
	if err != nil {
		t.Fatal(err)
	}
}
