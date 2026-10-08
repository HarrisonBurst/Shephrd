package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestNoRemoteFeatureBranchRequiresExplicitDefault(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "demo")
	if output, err := exec.Command("git", "init", "-b", "main", repoPath).CombinedOutput(); err != nil {
		t.Fatalf("init: %s: %v", output, err)
	}
	git := func(args ...string) {
		t.Helper()
		command := append([]string{"-C", repoPath}, args...)
		if output, err := exec.Command("git", command...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base")
	git("switch", "-c", "feature")
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	registry := Registry{Store: state}
	if _, err := registry.Add(repoPath, "", "", "", ""); err == nil {
		t.Fatal("feature branch was silently registered as the default")
	}
	repo, err := registry.Add(repoPath, "", "", "", "main")
	if err != nil || repo.DefaultBranch != "main" {
		t.Fatalf("repo = %+v, err = %v", repo, err)
	}
}

func TestScanAndResolve(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "nested", "demo")
	for _, path := range []string{repoPath, filepath.Join(root, "node_modules", "skipped"), filepath.Join(root, "vendor", "skipped"), filepath.Join(root, ".hidden", "skipped")} {
		if output, err := exec.Command("git", "init", "-b", "main", path).CombinedOutput(); err != nil {
			t.Fatalf("init: %s: %v", output, err)
		}
	}
	if output, err := exec.Command("git", "-C", repoPath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base").CombinedOutput(); err != nil {
		t.Fatalf("commit: %s: %v", output, err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	executable := buildRepositoryScanner(t)
	registry := Registry{Config: config.Config{RepositoryDiscovery: &config.RepositoryDiscoveryConfig{
		Roots: []string{root}, Command: []string{executable}, SHA256: digestFile(t, executable),
	}}, Store: state}
	candidates, err := registry.Scan()
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %+v, err = %v", candidates, err)
	}
	registered, err := state.Repos()
	if err != nil || len(registered) != 0 {
		t.Fatalf("scan mutated registry: %+v, err = %v", registered, err)
	}
	repo, err := registry.Add("demo", "", "", "", "")
	if err != nil || repo.Name != "demo" || repo.DefaultBranch != "main" {
		t.Fatalf("repo = %+v, err = %v", repo, err)
	}
	resolved, err := registry.Resolve(repo.Path)
	if err != nil || resolved.ID != repo.ID {
		t.Fatalf("resolved = %+v, err = %v", resolved, err)
	}
}

func TestScanFailurePreservesRegistry(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	known, err := state.UpsertRepo(model.Repo{Name: "known", Path: "/known", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	registry := Registry{Store: state}
	if _, err := registry.Scan(); err == nil {
		t.Fatal("missing discovery capability was accepted")
	}
	repos, err := state.Repos()
	if err != nil || len(repos) != 1 || repos[0].ID != known.ID {
		t.Fatalf("registry after failed scan = %+v, err = %v", repos, err)
	}
}

func buildRepositoryScanner(t *testing.T) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "shephrd-repository-scanner")
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	command := exec.Command("go", "build", "-o", executable, "./cmd/shephrd-repository-scanner")
	command.Dir = projectRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build repository scanner: %s: %v", output, err)
	}
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	return executable
}

func digestFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
