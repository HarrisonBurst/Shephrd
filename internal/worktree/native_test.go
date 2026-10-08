package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %s: %v", args, dir, output, err)
	}
	return strings.TrimSpace(string(output))
}

func nativeFixture(t *testing.T) (NativeManager, model.Repo) {
	t.Helper()
	base := t.TempDir()
	repoPath := filepath.Join(base, "demo")
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoPath, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, ".gitignore"), []byte("ignored.tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoPath, "add", ".")
	runGit(t, repoPath, "commit", "-m", "base")
	root := filepath.Join(base, "worktrees")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return NewNative(root), model.Repo{ID: "repo_native", Name: "demo", Path: repoPath, DefaultBranch: "main"}
}

func allocate(t *testing.T, manager NativeManager, repo model.Repo, attempt model.Attempt) (string, NativeIdentity) {
	t.Helper()
	path, err := manager.AllocatePath(repo, attempt)
	if err != nil {
		t.Fatal(err)
	}
	branch := BranchName(attempt)
	base, err := manager.ResolveBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Add(repo, path, branch, base); err != nil {
		t.Fatal(err)
	}
	identity, err := manager.Inspect(repo, path, branch)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Head != base {
		t.Fatalf("worktree HEAD %s != resolved base %s", identity.Head, base)
	}
	return path, identity
}

func TestNativeConcurrentAttemptsGetDistinctIsolatedWorktrees(t *testing.T) {
	manager, repo := nativeFixture(t)
	// Registered-root dirt: a modified tracked file and an untracked file
	// must never appear in an attempt checkout.
	if err := os.WriteFile(filepath.Join(repo.Path, "README.md"), []byte("dirty edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Path, "untracked.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := model.Attempt{ID: "attempt_a", TaskID: "task_1", Number: 1}
	second := model.Attempt{ID: "attempt_b", TaskID: "task_2", Number: 1}
	pathA, identityA := allocate(t, manager, repo, first)
	// Ignored state created by attempt A must not transfer to attempt B.
	if err := os.WriteFile(filepath.Join(pathA, "ignored.tmp"), []byte("cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pathB, identityB := allocate(t, manager, repo, second)
	if pathA == pathB || identityA.GitDir == identityB.GitDir || identityA.Branch == identityB.Branch {
		t.Fatalf("attempt identities are not distinct: %+v vs %+v", identityA, identityB)
	}
	if identityA.Head != identityB.Head {
		t.Fatalf("attempts started from different baselines: %s vs %s", identityA.Head, identityB.Head)
	}
	if identityA.CommonDir != identityB.CommonDir {
		t.Fatalf("attempts do not share the registered common Git directory")
	}
	for _, path := range []string{pathA, pathB} {
		if body, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(body) != "demo\n" {
			t.Fatalf("registered-root dirt leaked into %s: %q, %v", path, body, err)
		}
		if _, err := os.Stat(filepath.Join(path, "untracked.txt")); !os.IsNotExist(err) {
			t.Fatalf("untracked root file leaked into %s", path)
		}
	}
	if _, err := os.Stat(filepath.Join(pathB, "ignored.tmp")); !os.IsNotExist(err) {
		t.Fatal("ignored state from attempt A leaked into attempt B")
	}
	if body, err := os.ReadFile(filepath.Join(repo.Path, "README.md")); err != nil || string(body) != "dirty edit\n" {
		t.Fatalf("registered root was modified by allocation: %q, %v", body, err)
	}
	if dirty, err := manager.Dirty(pathA); err != nil || dirty {
		t.Fatalf("fresh worktree reports dirty=%t, err=%v (ignored files must not count)", dirty, err)
	}
}

func TestNativeBranchCollisionFailsBeforeAddWithoutDeletingBranch(t *testing.T) {
	manager, repo := nativeFixture(t)
	runGit(t, repo.Path, "branch", "shephrd/task_1")
	attempt := model.Attempt{ID: "attempt_a", TaskID: "task_1", Number: 1}
	path, err := manager.AllocatePath(repo, attempt)
	if err != nil {
		t.Fatal(err)
	}
	base, err := manager.ResolveBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Add(repo, path, BranchName(attempt), base); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("branch collision error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed allocation left a worktree path behind")
	}
	runGit(t, repo.Path, "rev-parse", "--verify", "refs/heads/shephrd/task_1")
}

func TestNativeUnsupportedRepositoriesFailWithDiagnostics(t *testing.T) {
	manager, repo := nativeFixture(t)
	base, err := manager.ResolveBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CheckSupported(repo, base); err != nil {
		t.Fatalf("plain repository reported unsupported: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo.Path, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo.Path, "add", ".gitattributes")
	runGit(t, repo.Path, "commit", "-m", "lfs")
	lfsBase, err := manager.ResolveBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CheckSupported(repo, lfsBase); err == nil || !strings.Contains(err.Error(), "Git LFS") {
		t.Fatalf("LFS diagnostic = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo.Path, ".gitmodules"), []byte("[submodule \"dep\"]\n\tpath = dep\n\turl = ./dep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo.Path, "add", ".gitmodules")
	runGit(t, repo.Path, "commit", "-m", "submodule")
	submoduleBase, err := manager.ResolveBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CheckSupported(repo, submoduleBase); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("submodule diagnostic = %v", err)
	}
}

func TestNativeResolveBaseFetchesOriginAndNeverFallsBack(t *testing.T) {
	manager, repo := nativeFixture(t)
	originPath := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, filepath.Dir(originPath), "clone", "--bare", repo.Path, originPath)
	runGit(t, repo.Path, "remote", "add", "origin", originPath)
	// Advance origin past the local registered head; the freshly fetched
	// remote-tracking commit must win over stale local state.
	seed := filepath.Join(t.TempDir(), "seed")
	runGit(t, filepath.Dir(seed), "clone", originPath, seed)
	if err := os.WriteFile(filepath.Join(seed, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "new.txt")
	runGit(t, seed, "commit", "-m", "advance")
	runGit(t, seed, "push", "origin", "main")
	remoteHead := runGit(t, seed, "rev-parse", "HEAD")
	localHead := runGit(t, repo.Path, "rev-parse", "refs/heads/main")
	base, err := manager.ResolveBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	if base != remoteHead || base == localHead {
		t.Fatalf("base = %s, want freshly fetched %s (stale local %s)", base, remoteHead, localHead)
	}
	// A failed fetch must fail the allocation instead of silently reusing
	// stale local state.
	runGit(t, repo.Path, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	if _, err := manager.ResolveBase(repo); err == nil || !strings.Contains(err.Error(), "fetch registered default branch") {
		t.Fatalf("fetch failure error = %v", err)
	}
}

func TestNativeDefaultBaseUsesLatestSafeTip(t *testing.T) {
	for _, test := range []struct {
		name       string
		remoteName string
		advance    string
	}{
		{name: "local-only", advance: "local"},
		{name: "remote", remoteName: "origin", advance: "remote"},
		{name: "sole-upstream", remoteName: "upstream", advance: "remote"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, repo := nativeFixture(t)
			originPath := filepath.Join(t.TempDir(), "origin.git")
			runGit(t, filepath.Dir(originPath), "clone", "--bare", repo.Path, originPath)
			if test.remoteName != "" {
				runGit(t, repo.Path, "remote", "add", test.remoteName, originPath)
			}
			if test.advance == "local" {
				if err := os.WriteFile(filepath.Join(repo.Path, "local.txt"), []byte("local\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, repo.Path, "add", "local.txt")
				runGit(t, repo.Path, "commit", "-m", "local advance")
			} else {
				seed := filepath.Join(t.TempDir(), "seed")
				runGit(t, filepath.Dir(seed), "clone", originPath, seed)
				if err := os.WriteFile(filepath.Join(seed, "remote.txt"), []byte("remote\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, seed, "add", "remote.txt")
				runGit(t, seed, "commit", "-m", "remote advance")
				runGit(t, seed, "push", "origin", "main")
			}
			want := runGit(t, repo.Path, "rev-parse", "refs/heads/main")
			if test.advance == "remote" {
				remoteTip := runGit(t, repo.Path, "ls-remote", originPath, "refs/heads/main")
				want = strings.Fields(remoteTip)[0]
			}
			rootHead := runGit(t, repo.Path, "rev-parse", "HEAD")
			base, err := manager.ResolveDefaultBase(repo)
			if err != nil {
				t.Fatal(err)
			}
			if test.advance == "local" && base != want {
				t.Fatalf("base = %s, want local tip %s", base, want)
			}
			if test.advance == "remote" && base != want {
				t.Fatalf("base = %s, want remote tip %s", base, want)
			}
			if runGit(t, repo.Path, "rev-parse", "HEAD") != rootHead {
				t.Fatal("default resolution changed the registered root HEAD")
			}
			if runGit(t, repo.Path, "status", "--porcelain") != "" {
				t.Fatal("default resolution dirtied the registered root")
			}
		})
	}
}

func TestNativeDefaultBaseRejectsUncertainRemoteState(t *testing.T) {
	manager, repo := nativeFixture(t)
	originPath := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, filepath.Dir(originPath), "clone", "--bare", repo.Path, originPath)
	runGit(t, repo.Path, "remote", "add", "origin", originPath)
	seed := filepath.Join(t.TempDir(), "seed")
	runGit(t, filepath.Dir(seed), "clone", originPath, seed)
	if err := os.WriteFile(filepath.Join(seed, "remote.txt"), []byte("remote\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "remote.txt")
	runGit(t, seed, "commit", "-m", "remote advance")
	runGit(t, seed, "push", "origin", "main")
	if err := os.WriteFile(filepath.Join(repo.Path, "local.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo.Path, "add", "local.txt")
	runGit(t, repo.Path, "commit", "-m", "local advance")
	before := runGit(t, repo.Path, "rev-parse", "HEAD")
	if _, err := manager.ResolveDefaultBase(repo); err == nil || !strings.Contains(err.Error(), "diverges") {
		t.Fatalf("divergence resolution = %v", err)
	}
	if runGit(t, repo.Path, "rev-parse", "HEAD") != before || runGit(t, repo.Path, "status", "--porcelain") != "" {
		t.Fatal("divergence resolution changed the registered root")
	}
	for _, remote := range []string{filepath.Join(t.TempDir(), "missing.git"), filepath.Join(t.TempDir(), "empty.git")} {
		if strings.HasSuffix(remote, "empty.git") {
			runGit(t, filepath.Dir(remote), "init", "--bare", remote)
		}
		runGit(t, repo.Path, "remote", "set-url", "origin", remote)
		if _, err := manager.ResolveDefaultBase(repo); err == nil || !strings.Contains(err.Error(), "fetch registered default branch") {
			t.Fatalf("uncertain remote %s resolution = %v", remote, err)
		}
	}

	manager, repo = nativeFixture(t)
	first := filepath.Join(t.TempDir(), "first.git")
	second := filepath.Join(t.TempDir(), "second.git")
	runGit(t, filepath.Dir(first), "init", "--bare", first)
	runGit(t, filepath.Dir(second), "init", "--bare", second)
	runGit(t, repo.Path, "remote", "add", "first", first)
	runGit(t, repo.Path, "remote", "add", "second", second)
	if _, err := manager.ResolveDefaultBase(repo); err == nil || !strings.Contains(err.Error(), "unambiguous") {
		t.Fatalf("ambiguous remote resolution = %v", err)
	}
}

func TestNativeExplicitBaseSelectionsAreExact(t *testing.T) {
	manager, repo := nativeFixture(t)
	branch := "stack-base"
	runGit(t, repo.Path, "branch", branch)
	branchBase := runGit(t, repo.Path, "rev-parse", "refs/heads/"+branch)
	if got, err := manager.ResolveBranchBase(repo, branch); err != nil || got != branchBase {
		t.Fatalf("explicit branch base = %s, err = %v, want %s", got, err, branchBase)
	}
	if got, err := manager.ResolveCommitBase(repo, branchBase); err != nil || got != branchBase {
		t.Fatalf("explicit commit base = %s, err = %v, want %s", got, err, branchBase)
	}
	if _, err := manager.ResolveCommitBase(repo, branchBase[:12]); err == nil {
		t.Fatal("abbreviated explicit commit was accepted")
	}
	if _, err := manager.ResolveBranchBase(repo, "missing"); err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("missing explicit branch = %v", err)
	}
}

func TestNativeRemoveModesAndBranchRetention(t *testing.T) {
	manager, repo := nativeFixture(t)
	attempt := model.Attempt{ID: "attempt_a", TaskID: "task_1", Number: 1}
	path, _ := allocate(t, manager, repo, attempt)
	if err := os.WriteFile(filepath.Join(path, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if dirty, err := manager.Dirty(path); err != nil || !dirty {
		t.Fatalf("dirty = %t, err = %v", dirty, err)
	}
	if err := manager.Remove(repo, path, false); err == nil {
		t.Fatal("non-forced removal succeeded on a dirty worktree")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("failed non-forced removal deleted the worktree")
	}
	if err := manager.Remove(repo, path, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("forced removal left the worktree")
	}
	if _, err := os.Stat(filepath.Join(manager.Root, repo.ID)); !os.IsNotExist(err) {
		t.Fatal("empty attempt containers were not cleaned up")
	}
	// The branch ref is artifact history and must survive removal.
	runGit(t, repo.Path, "rev-parse", "--verify", "refs/heads/shephrd/task_1")

	clean := model.Attempt{ID: "attempt_b", TaskID: "task_2", Number: 1}
	cleanPath, _ := allocate(t, manager, repo, clean)
	if err := manager.Remove(repo, cleanPath, false); err != nil {
		t.Fatalf("non-forced removal of a clean worktree failed: %v", err)
	}
	runGit(t, repo.Path, "rev-parse", "--verify", "refs/heads/shephrd/task_2")
}

func TestNativeContainmentDefenses(t *testing.T) {
	manager, repo := nativeFixture(t)
	outside := filepath.Join(t.TempDir(), "escape")
	if err := manager.Add(repo, outside, "shephrd/escape", "HEAD"); err == nil || !strings.Contains(err.Error(), "outside the configured worktree root") {
		t.Fatalf("outside-root error = %v", err)
	}
	if err := manager.Remove(repo, filepath.Join(manager.Root, ".."), false); err == nil {
		t.Fatal("parent traversal was not rejected")
	}
	if err := manager.Remove(repo, manager.Root, false); err == nil {
		t.Fatal("removing the root itself was not rejected")
	}
	// A symlinked component under the root must be rejected before any
	// creation or removal touches the target.
	victim := t.TempDir()
	if err := os.MkdirAll(filepath.Join(manager.Root, repo.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(manager.Root, repo.ID, "attempt_evil")
	if err := os.Symlink(victim, linked); err != nil {
		t.Fatal(err)
	}
	attackPath := filepath.Join(linked, repo.Name)
	if err := manager.Add(repo, attackPath, "shephrd/evil", "HEAD"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink creation error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(victim, repo.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(repo, attackPath, true); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink removal error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(victim, repo.Name)); err != nil {
		t.Fatal("containment attack removed content outside the root")
	}
}

func TestNativeRegistrationStateClassifiesCrashEvidence(t *testing.T) {
	manager, repo := nativeFixture(t)
	attempt := model.Attempt{ID: "attempt_a", TaskID: "task_1", Number: 1}
	path, err := manager.AllocatePath(repo, attempt)
	if err != nil {
		t.Fatal(err)
	}
	pathExists, registered, err := manager.RegistrationState(repo, path)
	if err != nil || pathExists || registered {
		t.Fatalf("pre-add state = %t/%t, err = %v", pathExists, registered, err)
	}
	path, _ = allocate(t, manager, repo, attempt)
	pathExists, registered, err = manager.RegistrationState(repo, path)
	if err != nil || !pathExists || !registered {
		t.Fatalf("post-add state = %t/%t, err = %v", pathExists, registered, err)
	}
	// Crash between directory removal and registration cleanup.
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	pathExists, registered, err = manager.RegistrationState(repo, path)
	if err != nil || pathExists || !registered {
		t.Fatalf("half-removed state = %t/%t, err = %v", pathExists, registered, err)
	}
	if err := manager.RemoveAbsentRegistration(repo, path); err != nil {
		t.Fatal(err)
	}
	pathExists, registered, err = manager.RegistrationState(repo, path)
	if err != nil || pathExists || registered {
		t.Fatalf("post-cleanup state = %t/%t, err = %v", pathExists, registered, err)
	}
}

func TestNativeVerifyHeldDetectsIdentityDrift(t *testing.T) {
	manager, repo := nativeFixture(t)
	attempt := model.Attempt{ID: "attempt_a", TaskID: "task_1", Number: 1}
	path, identity := allocate(t, manager, repo, attempt)
	held := model.Attempt{ID: attempt.ID, TaskID: attempt.TaskID, Number: 1, WorktreePath: path,
		WorktreeGitDir: identity.GitDir, WorktreeCommonDir: identity.CommonDir, Branch: identity.Branch}
	if _, err := manager.VerifyHeld(repo, held); err != nil {
		t.Fatalf("exact identity failed verification: %v", err)
	}
	drifted := held
	drifted.WorktreeGitDir = filepath.Join(identity.CommonDir, "worktrees", "other")
	if _, err := manager.VerifyHeld(repo, drifted); err == nil {
		t.Fatal("gitdir drift was not detected")
	}
	runGit(t, path, "checkout", "--detach")
	if _, err := manager.VerifyHeld(repo, held); err == nil || !strings.Contains(err.Error(), "not checked out") {
		t.Fatalf("branch drift error = %v", err)
	}
	runGit(t, path, "checkout", identity.Branch)
	if _, err := manager.VerifyHeld(repo, held); err != nil {
		t.Fatalf("restored identity failed verification: %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyHeld(repo, held); err == nil {
		t.Fatal("missing worktree passed held verification")
	}
}
