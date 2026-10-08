package control

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/process"
	"shephrd/internal/store"
	"shephrd/internal/worktree"
)

type nativeFixture struct {
	service      Service
	state        *store.Store
	repo         model.Repo
	databasePath string
	worktreeRoot string
}

func newNativeFixture(t *testing.T) *nativeFixture {
	t.Helper()
	installFakePi(t)
	base := t.TempDir()
	repoPath := filepath.Join(base, "repo")
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repoPath, "init", "-b", "main")
	runLocalGit(t, repoPath, "config", "user.name", "Test")
	runLocalGit(t, repoPath, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repoPath, "add", "README.md")
	runLocalGit(t, repoPath, "commit", "-m", "base")
	databasePath := filepath.Join(base, "state.db")
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: repoPath, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	worktreeRoot := filepath.Join(base, "worktrees")
	cfg := config.Config{DefaultHarness: "pi", DataDir: filepath.Join(base, "data"), WorktreeRoot: worktreeRoot}
	service := New(cfg, state)
	executable := filepath.Join(base, "fake-shephrd")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_EXECUTABLE", executable)
	return &nativeFixture{service: service, state: state, repo: repo, databasePath: databasePath, worktreeRoot: worktreeRoot}
}

func (f *nativeFixture) createTask(t *testing.T, feature string) model.Task {
	t.Helper()
	task, err := f.state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: f.repo.ID, FeatureKey: feature, Objective: feature, Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *nativeFixture) spawn(t *testing.T, taskID string) model.Attempt {
	t.Helper()
	result, err := f.service.SpawnWithModelSelection(taskID, "pi", "", false)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := f.state.Attempt(result.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	reapRunner(t, attempt.RunnerPID)
	return attempt
}

// reapRunner waits for the detached fake runner (a direct child of the test
// process) so its PID stops reading as alive.
func reapRunner(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		return
	}
	waitForChildProcess(pid)
	deadline := time.Now().Add(5 * time.Second)
	for process.Alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("fake runner pid %d did not exit", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reachWaiting records a worker checkpoint and question so the attempt is
// resumable exactly like a live worker that asked for input.
func (f *nativeFixture) reachWaiting(t *testing.T, attempt model.Attempt, facts model.WorkspaceFacts) {
	t.Helper()
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "progress", NextSteps: []string{"continue"}}
	if _, err := f.state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "checkpoint", Payload: "progress", Checkpoint: &checkpoint}, attempt.Cursor+1, facts); err != nil {
		t.Fatal(err)
	}
	if _, err := f.state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "question", Payload: "which"}, attempt.Cursor+2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSpawnCreatesHeldAttemptOwnedWorkspace(t *testing.T) {
	fixture := newNativeFixture(t)
	head := runLocalGit(t, fixture.repo.Path, "rev-parse", "HEAD")
	first := fixture.createTask(t, "one")
	second := fixture.createTask(t, "two")
	attemptA := fixture.spawn(t, first.ID)
	attemptB := fixture.spawn(t, second.ID)
	for _, attempt := range []model.Attempt{attemptA, attemptB} {
		if attempt.WorkspaceBackend != model.WorkspaceBackendNative || attempt.WorkspaceState != model.WorkspaceStateHeld {
			t.Fatalf("attempt workspace = %q/%q", attempt.WorkspaceBackend, attempt.WorkspaceState)
		}
		wantPath := filepath.Join(fixture.worktreeRoot, fixture.repo.ID, attempt.ID, "demo")
		if attempt.WorktreePath != wantPath || attempt.IntendedWorktreePath != wantPath {
			t.Fatalf("attempt path = %q, want %q", attempt.WorktreePath, wantPath)
		}
		if attempt.WorktreeGitDir == "" || attempt.WorktreeCommonDir == "" || attempt.LeaseID != "" {
			t.Fatalf("attempt identity = %+v", attempt)
		}
		if attempt.BaseCommit != head {
			t.Fatalf("attempt base = %q, want registered head %q", attempt.BaseCommit, head)
		}
		if got := runLocalGit(t, attempt.WorktreePath, "rev-parse", "HEAD"); got != head {
			t.Fatalf("worktree HEAD = %q, want %q", got, head)
		}
	}
	if attemptA.WorktreePath == attemptB.WorktreePath || attemptA.Branch == attemptB.Branch {
		t.Fatal("concurrent tasks shared a workspace or branch")
	}
	brief, err := os.ReadFile(filepath.Join(fixture.service.Config.DataDir, first.ID, "brief.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(brief), "Workspace backend: native_git_worktree") || !strings.Contains(string(brief), "Base commit: "+head) {
		t.Fatalf("brief run identity is missing native workspace facts")
	}
}

func TestNativeSpawnUsesFreshRemoteTipWithoutMutatingRoot(t *testing.T) {
	fixture := newNativeFixture(t)
	originPath := filepath.Join(t.TempDir(), "origin.git")
	runLocalGit(t, filepath.Dir(originPath), "clone", "--bare", fixture.repo.Path, originPath)
	runLocalGit(t, fixture.repo.Path, "remote", "add", "origin", originPath)
	rootHead := runLocalGit(t, fixture.repo.Path, "rev-parse", "HEAD")
	seed := filepath.Join(t.TempDir(), "seed")
	runLocalGit(t, filepath.Dir(seed), "clone", originPath, seed)
	runLocalGit(t, seed, "config", "user.name", "Test")
	runLocalGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "remote.txt"), []byte("remote\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, seed, "add", "remote.txt")
	runLocalGit(t, seed, "commit", "-m", "remote advance")
	runLocalGit(t, seed, "push", "origin", "main")
	remoteHead := runLocalGit(t, seed, "rev-parse", "HEAD")
	task := fixture.createTask(t, "remote-fresh")
	attempt := fixture.spawn(t, task.ID)
	if attempt.BaseCommit != remoteHead || runLocalGit(t, attempt.WorktreePath, "rev-parse", "HEAD") != remoteHead {
		t.Fatalf("spawn base = %s, want remote %s", attempt.BaseCommit, remoteHead)
	}
	if runLocalGit(t, fixture.repo.Path, "rev-parse", "HEAD") != rootHead || runLocalGit(t, fixture.repo.Path, "status", "--porcelain") != "" {
		t.Fatal("remote refresh checked out or dirtied the registered root")
	}
}

func TestNativeExplicitStackedTaskBasePreservesRelation(t *testing.T) {
	fixture := newNativeFixture(t)
	producer := fixture.createTask(t, "producer")
	producerAttempt := fixture.spawn(t, producer.ID)
	if err := os.WriteFile(filepath.Join(producerAttempt.WorktreePath, "producer.txt"), []byte("producer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, producerAttempt.WorktreePath, "add", "producer.txt")
	runLocalGit(t, producerAttempt.WorktreePath, "commit", "-m", "producer")
	producerHead := runLocalGit(t, producerAttempt.WorktreePath, "rev-parse", "HEAD")
	if branchHead := runLocalGit(t, fixture.repo.Path, "rev-parse", "refs/heads/"+producerAttempt.Branch); branchHead != producerHead {
		t.Fatalf("producer branch = %s, worktree HEAD = %s", branchHead, producerHead)
	}
	if direct, err := fixture.service.Native.ResolveBranchBase(fixture.repo, producerAttempt.Branch); err != nil || direct != producerHead {
		t.Fatalf("direct producer branch resolution = %s, err = %v", direct, err)
	}
	target := fixture.createTask(t, "stacked")
	result, err := fixture.service.SpawnWithBaseSelection(target.ID, "pi", "", false, model.BaseSelection{Strategy: model.BaseStrategyTask, Ref: producer.ID})
	if err != nil {
		t.Fatal(err)
	}
	reapRunner(t, result.Attempt.RunnerPID)
	if result.Attempt.BaseStrategy != model.BaseStrategyTask || result.Attempt.BaseRef != producer.ID || result.Attempt.BaseCommit != producerHead {
		t.Fatalf("stacked attempt = %+v, worktree head = %s, want task %s at %s", result.Attempt, runLocalGit(t, result.Attempt.WorktreePath, "rev-parse", "HEAD"), producer.ID, producerHead)
	}
	if got := runLocalGit(t, result.Attempt.WorktreePath, "rev-parse", "HEAD"); got != producerHead {
		t.Fatalf("stacked worktree HEAD = %s, want %s", got, producerHead)
	}
}

func TestNativeCleanRetryReResolvesDeclaredStackedBase(t *testing.T) {
	fixture := newNativeFixture(t)
	producer := fixture.createTask(t, "retry-producer")
	producerAttempt := fixture.spawn(t, producer.ID)
	if err := os.WriteFile(filepath.Join(producerAttempt.WorktreePath, "first.txt"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, producerAttempt.WorktreePath, "add", "first.txt")
	runLocalGit(t, producerAttempt.WorktreePath, "commit", "-m", "first producer commit")
	target := fixture.createTask(t, "retry-stacked")
	first, err := fixture.service.SpawnWithBaseSelection(target.ID, "pi", "", false, model.BaseSelection{Strategy: model.BaseStrategyTask, Ref: producer.ID})
	if err != nil {
		t.Fatal(err)
	}
	reapRunner(t, first.Attempt.RunnerPID)
	if err := fixture.state.Stop(target.ID, "stopped for retry"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(producerAttempt.WorktreePath, "second.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, producerAttempt.WorktreePath, "add", "second.txt")
	runLocalGit(t, producerAttempt.WorktreePath, "commit", "-m", "second producer commit")
	latest := runLocalGit(t, producerAttempt.WorktreePath, "rev-parse", "HEAD")
	retry, err := fixture.service.RetryWithModelSelection(target.ID, "pi", "", false)
	if err != nil {
		t.Fatal(err)
	}
	reapRunner(t, retry.Attempt.RunnerPID)
	if retry.Attempt.BaseStrategy != model.BaseStrategyTask || retry.Attempt.BaseRef != producer.ID || retry.Attempt.BaseCommit != latest {
		t.Fatalf("stacked retry = %+v, want task %s at %s", retry.Attempt, producer.ID, latest)
	}
}

func TestNativeCleanRetryReResolvesDeclaredDefaultBase(t *testing.T) {
	fixture := newNativeFixture(t)
	task := fixture.createTask(t, "retry-fresh")
	first := fixture.spawn(t, task.ID)
	if err := fixture.state.Stop(task.ID, "stopped for retry"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.repo.Path, "advanced.txt"), []byte("advanced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, fixture.repo.Path, "add", "advanced.txt")
	runLocalGit(t, fixture.repo.Path, "commit", "-m", "default advance")
	latest := runLocalGit(t, fixture.repo.Path, "rev-parse", "HEAD")
	retry, err := fixture.service.RetryWithModelSelection(task.ID, "pi", "", false)
	if err != nil {
		t.Fatal(err)
	}
	reapRunner(t, retry.Attempt.RunnerPID)
	if first.BaseCommit == latest || retry.Attempt.BaseCommit != latest || retry.Attempt.BaseStrategy != model.BaseStrategyDefaultBranch {
		t.Fatalf("retry bases = first %s retry %+v, want latest %s", first.BaseCommit, retry.Attempt, latest)
	}
}

func TestNativeSpawnFailuresBeforeAddBecomeNoWorkspace(t *testing.T) {
	fixture := newNativeFixture(t)
	branchTask := fixture.createTask(t, "collision")
	runLocalGit(t, fixture.repo.Path, "branch", "shephrd/"+branchTask.ID)
	if _, err := fixture.service.SpawnWithModelSelection(branchTask.ID, "pi", "", false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("branch collision spawn error = %v", err)
	}
	task, _ := fixture.state.Task(branchTask.ID)
	attempt, err := fixture.state.Attempt(task.CurrentAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.WorkspaceState != model.WorkspaceStateNoWorkspace || attempt.ReleaseState != "no_workspace" {
		t.Fatalf("collision attempt = %q/%q", attempt.WorkspaceState, attempt.ReleaseState)
	}
	runLocalGit(t, fixture.repo.Path, "rev-parse", "--verify", "refs/heads/shephrd/"+branchTask.ID)

	runLocalGit(t, fixture.repo.Path, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))
	fetchTask := fixture.createTask(t, "fetch")
	if _, err := fixture.service.SpawnWithModelSelection(fetchTask.ID, "pi", "", false); err == nil || !strings.Contains(err.Error(), "fetch registered default branch") {
		t.Fatalf("fetch failure spawn error = %v", err)
	}
	task, _ = fixture.state.Task(fetchTask.ID)
	attempt, _ = fixture.state.Attempt(task.CurrentAttemptID)
	if attempt.WorkspaceState != model.WorkspaceStateNoWorkspace {
		t.Fatalf("fetch failure attempt = %q", attempt.WorkspaceState)
	}
	runLocalGit(t, fixture.repo.Path, "remote", "remove", "origin")

	if err := os.WriteFile(filepath.Join(fixture.repo.Path, ".gitmodules"), []byte("[submodule \"dep\"]\n\tpath = dep\n\turl = ./dep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, fixture.repo.Path, "add", ".gitmodules")
	runLocalGit(t, fixture.repo.Path, "commit", "-m", "submodule")
	submoduleTask := fixture.createTask(t, "submodule")
	if _, err := fixture.service.SpawnWithModelSelection(submoduleTask.ID, "pi", "", false); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("submodule spawn error = %v", err)
	}
	task, _ = fixture.state.Task(submoduleTask.ID)
	attempt, _ = fixture.state.Attempt(task.CurrentAttemptID)
	if attempt.WorkspaceState != model.WorkspaceStateNoWorkspace {
		t.Fatalf("submodule attempt = %q", attempt.WorkspaceState)
	}
	if entries, err := os.ReadDir(fixture.worktreeRoot); err == nil {
		for _, entry := range entries {
			t.Fatalf("failed spawns left content under the worktree root: %s", entry.Name())
		}
	}
}

func TestNativeSetupHookFailureLeavesExactWorktreeHeld(t *testing.T) {
	fixture := newNativeFixture(t)
	if _, err := fixture.state.UpsertRepo(model.Repo{Name: "demo", Path: fixture.repo.Path, DefaultBranch: "main", SetupHook: "exit 7"}); err != nil {
		t.Fatal(err)
	}
	task := fixture.createTask(t, "hook")
	if _, err := fixture.service.SpawnWithModelSelection(task.ID, "pi", "", false); err == nil || !strings.Contains(err.Error(), "setup hook failed") {
		t.Fatalf("setup hook spawn error = %v", err)
	}
	current, _ := fixture.state.Task(task.ID)
	attempt, err := fixture.state.Attempt(current.CurrentAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.WorkspaceState != model.WorkspaceStateHeld {
		t.Fatalf("attempt after hook failure = %q", attempt.WorkspaceState)
	}
	if _, err := os.Stat(attempt.WorktreePath); err != nil {
		t.Fatal("setup hook failure removed the worktree")
	}
	// Explicit discard is the disposition path and must remove exactly this
	// worktree while keeping the branch ref.
	if err := fixture.service.Release(task.ID, attempt.ID, true); err != nil {
		t.Fatal(err)
	}
	released, _ := fixture.state.Attempt(attempt.ID)
	if released.WorkspaceState != model.WorkspaceStateReleased || released.ReleasedAt == nil {
		t.Fatalf("discarded attempt = %+v", released)
	}
	if _, err := os.Stat(attempt.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("discard did not remove the worktree")
	}
	runLocalGit(t, fixture.repo.Path, "rev-parse", "--verify", "refs/heads/"+attempt.Branch)
	// Repeated release of a released attempt is idempotent.
	if err := fixture.service.Release(task.ID, attempt.ID, false); err != nil {
		t.Fatalf("repeated release = %v", err)
	}
}

func TestNativeRelaunchPreservesExactWorktreeAndRetryGetsFreshOne(t *testing.T) {
	fixture := newNativeFixture(t)
	task := fixture.createTask(t, "resume")
	attempt := fixture.spawn(t, task.ID)
	fixture.reachWaiting(t, attempt, model.WorkspaceFacts{HeadCommit: attempt.BaseCommit})
	dirtyFile := filepath.Join(attempt.WorktreePath, "in-progress.txt")
	if err := os.WriteFile(dirtyFile, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.repo.Path, "main-advance.txt"), []byte("main advance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, fixture.repo.Path, "add", "main-advance.txt")
	runLocalGit(t, fixture.repo.Path, "commit", "-m", "main advance")
	latest := runLocalGit(t, fixture.repo.Path, "rev-parse", "HEAD")
	result, err := fixture.service.Relaunch(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reapRunner(t, result.Attempt.RunnerPID)
	if result.Attempt.ID != attempt.ID || result.Attempt.WorktreePath != attempt.WorktreePath || result.Attempt.RunGeneration != attempt.RunGeneration+1 || result.Attempt.BaseCommit != attempt.BaseCommit {
		t.Fatalf("relaunch identity = %+v", result.Attempt)
	}
	if _, err := os.Stat(dirtyFile); err != nil {
		t.Fatal("relaunch destroyed dirty worker state")
	}

	// A retry supersedes the attempt but must leave its workspace held and
	// give the new attempt a distinct fresh worktree and branch.
	if err := fixture.state.Stop(task.ID, "stopped for retry"); err != nil {
		t.Fatal(err)
	}
	retry, err := fixture.service.RetryWithModelSelection(task.ID, "pi", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Attempt.ID == attempt.ID || retry.Attempt.WorktreePath == attempt.WorktreePath {
		t.Fatalf("retry reused the old workspace: %+v", retry.Attempt)
	}
	if retry.Attempt.Branch != "shephrd/"+task.ID+"-attempt-2" {
		t.Fatalf("retry branch = %q", retry.Attempt.Branch)
	}
	if retry.Attempt.BaseCommit != latest {
		t.Fatalf("retry base = %q, want latest default tip %q", retry.Attempt.BaseCommit, latest)
	}
	if _, err := os.Stat(filepath.Join(retry.Attempt.WorktreePath, "in-progress.txt")); !os.IsNotExist(err) {
		t.Fatal("retry transferred dirty files from the superseded attempt")
	}
	old, _ := fixture.state.Attempt(attempt.ID)
	if old.Status != "superseded" || old.WorkspaceState != model.WorkspaceStateHeld {
		t.Fatalf("superseded attempt = %q/%q", old.Status, old.WorkspaceState)
	}
	if _, err := os.Stat(dirtyFile); err != nil {
		t.Fatal("retry destroyed the superseded workspace")
	}
}

func TestNativeRelaunchFailsClosedOnIdentityDrift(t *testing.T) {
	fixture := newNativeFixture(t)
	task := fixture.createTask(t, "drift")
	attempt := fixture.spawn(t, task.ID)
	fixture.reachWaiting(t, attempt, model.WorkspaceFacts{HeadCommit: attempt.BaseCommit})
	moved := attempt.WorktreePath + "-moved"
	if err := os.Rename(attempt.WorktreePath, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Relaunch(task.ID); err == nil || !strings.Contains(err.Error(), "identity cannot be verified") {
		t.Fatalf("relaunch after drift = %v", err)
	}
	if err := os.Rename(moved, attempt.WorktreePath); err != nil {
		t.Fatal(err)
	}
	restored, err := fixture.service.Relaunch(task.ID)
	if err != nil {
		t.Fatalf("relaunch after restore = %v", err)
	}
	reapRunner(t, restored.Attempt.RunnerPID)
}

func TestNativeReleaseRequiresCleanWorkspaceUnlessDiscarded(t *testing.T) {
	fixture := newNativeFixture(t)
	task := fixture.createTask(t, "dirt")
	attempt := fixture.spawn(t, task.ID)
	if err := os.WriteFile(filepath.Join(attempt.WorktreePath, "residual.txt"), []byte("residual\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if claimed, _, err := fixture.state.ClaimRelease(attempt.ID); err != nil || !claimed {
		t.Fatalf("claim = %t, err = %v", claimed, err)
	}
	_, err := fixture.service.workspaceBackend().Release(fixture.repo, attempt, false)
	if err == nil || !strings.Contains(err.Error(), "residual workspace changes") {
		t.Fatalf("dirty release error = %v", err)
	}
	stored, _ := fixture.state.Attempt(attempt.ID)
	if stored.WorkspaceState != model.WorkspaceStateHeld || stored.ReleaseState != "held" {
		t.Fatalf("attempt after refused release = %q/%q", stored.WorkspaceState, stored.ReleaseState)
	}
	if _, err := os.Stat(attempt.WorktreePath); err != nil {
		t.Fatal("refused release removed the worktree")
	}
	if err := fixture.service.Release(task.ID, attempt.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(attempt.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("discard did not remove the dirty worktree")
	}
}

func TestNativeLocalLandingReleasesWorktreeOnce(t *testing.T) {
	fixture := newNativeFixture(t)
	task := fixture.createTask(t, "landing")
	attempt := fixture.spawn(t, task.ID)
	if err := os.WriteFile(filepath.Join(attempt.WorktreePath, "work.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, attempt.WorktreePath, "add", "work.txt")
	runLocalGit(t, attempt.WorktreePath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "worker")
	sealed := runLocalGit(t, attempt.WorktreePath, "rev-parse", "HEAD")
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "sealed", NextSteps: []string{"deliver"}}
	if _, err := fixture.state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "checkpoint", Payload: "sealed", Checkpoint: &checkpoint}, attempt.Cursor+1, model.WorkspaceFacts{HeadCommit: sealed}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "done", Payload: "done", Artifact: "branch:" + attempt.Branch}, attempt.Cursor+2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, fixture.repo.Path, "merge", "--ff-only", attempt.Branch)
	result, err := fixture.service.VerifyLocal(task.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Landed || result.Landing.Kind != "local_default_branch" || result.Landing.SourceCommit != sealed {
		t.Fatalf("local landing result = %+v", result)
	}
	released, _ := fixture.state.Attempt(attempt.ID)
	if released.WorkspaceState != model.WorkspaceStateReleased || released.ReleasedAt == nil {
		t.Fatalf("landed attempt = %q released_at=%v", released.WorkspaceState, released.ReleasedAt)
	}
	if _, err := os.Stat(attempt.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("landed release did not remove the worktree")
	}
	runLocalGit(t, fixture.repo.Path, "rev-parse", "--verify", "refs/heads/"+attempt.Branch)
	// Verification after release is idempotent through the immutable proof.
	again, err := fixture.service.VerifyLocal(task.ID, "", "")
	if err != nil || !again.Landed || !again.Worktree.Released {
		t.Fatalf("repeated verification = %+v, err = %v", again, err)
	}
}

func TestNativeReconcileRecoversInterruptedAllocations(t *testing.T) {
	fixture := newNativeFixture(t)

	// Crash after intent, before git worktree add: no_workspace.
	intentTask := fixture.createTask(t, "intent")
	intentAttempt, err := fixture.state.BeginAttempt(intentTask.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	intentPath, err := fixture.service.Native.AllocatePath(fixture.repo, intentAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.state.BeginNativeAllocation(intentAttempt.ID, intentPath); err != nil {
		t.Fatal(err)
	}

	// Crash after git worktree add, before identity persistence: recovered
	// as held with the exact re-verified identity.
	addTask := fixture.createTask(t, "added")
	addAttempt, err := fixture.state.BeginAttempt(addTask.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	addPath, err := fixture.service.Native.AllocatePath(fixture.repo, addAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.state.BeginNativeAllocation(addAttempt.ID, addPath); err != nil {
		t.Fatal(err)
	}
	base, err := fixture.service.Native.ResolveBase(fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.state.SetAttemptBaseCommit(addAttempt.ID, base); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Native.Add(fixture.repo, addPath, worktree.BranchName(addAttempt), base); err != nil {
		t.Fatal(err)
	}

	// Partial evidence (path exists but Git never registered it): unknown.
	partialTask := fixture.createTask(t, "partial")
	partialAttempt, err := fixture.state.BeginAttempt(partialTask.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	partialPath, err := fixture.service.Native.AllocatePath(fixture.repo, partialAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.state.BeginNativeAllocation(partialAttempt.ID, partialPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(partialPath, 0o700); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.service.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	classifications := map[string]string{}
	for _, entry := range result.Classifications {
		classifications[entry.AttemptID] = entry.Classification
	}
	if classifications[intentAttempt.ID] != "no_workspace" {
		t.Fatalf("intent classification = %q", classifications[intentAttempt.ID])
	}
	if classifications[addAttempt.ID] != "recovered-held" {
		t.Fatalf("added classification = %q", classifications[addAttempt.ID])
	}
	if classifications[partialAttempt.ID] != "unknown" {
		t.Fatalf("partial classification = %q", classifications[partialAttempt.ID])
	}
	intentStored, _ := fixture.state.Attempt(intentAttempt.ID)
	if intentStored.WorkspaceState != model.WorkspaceStateNoWorkspace {
		t.Fatalf("intent attempt = %q", intentStored.WorkspaceState)
	}
	addStored, _ := fixture.state.Attempt(addAttempt.ID)
	if addStored.WorkspaceState != model.WorkspaceStateHeld || addStored.WorktreePath != addPath || addStored.WorktreeGitDir == "" {
		t.Fatalf("added attempt = %+v", addStored)
	}
	partialStored, _ := fixture.state.Attempt(partialAttempt.ID)
	if partialStored.WorkspaceState != model.WorkspaceStateUnknown || partialStored.Status != "unknown" {
		t.Fatalf("partial attempt = %q/%q", partialStored.WorkspaceState, partialStored.Status)
	}
	if _, err := os.Stat(partialPath); err != nil {
		t.Fatal("reconcile deleted ambiguous external evidence")
	}
}

func TestNativeReconcileMarksDriftedHeldWorkspaceUnknown(t *testing.T) {
	fixture := newNativeFixture(t)
	task := fixture.createTask(t, "drifted")
	attempt := fixture.spawn(t, task.ID)
	if err := os.Rename(attempt.WorktreePath, attempt.WorktreePath+"-moved"); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range result.Classifications {
		if entry.AttemptID == attempt.ID {
			found = true
			if entry.Classification != "unknown" || entry.Backend != model.WorkspaceBackendNative {
				t.Fatalf("drifted entry = %+v", entry)
			}
		}
	}
	if !found {
		t.Fatal("drifted attempt missing from reconcile classifications")
	}
	stored, _ := fixture.state.Attempt(attempt.ID)
	if stored.WorkspaceState != model.WorkspaceStateUnknown || stored.Status != "unknown" {
		t.Fatalf("drifted attempt = %q/%q", stored.WorkspaceState, stored.Status)
	}
	if err := fixture.service.Release(task.ID, attempt.ID, true); err == nil || !strings.Contains(err.Error(), "reconcile identity") {
		t.Fatalf("release of unknown workspace = %v", err)
	}
}

func TestNativeReconcileCompletesInterruptedRelease(t *testing.T) {
	fixture := newNativeFixture(t)

	// Dead release owner with the worktree still present: revalidate and
	// complete the exact removal.
	presentTask := fixture.createTask(t, "present")
	presentAttempt := fixture.spawn(t, presentTask.ID)
	if err := fixture.state.AuthorizeDiscard(presentTask.ID, presentAttempt.ID); err != nil {
		t.Fatal(err)
	}
	claimDead(t, fixture.databasePath, presentAttempt.ID)

	// Dead release owner with worktree and registration both gone: record
	// the recovered release.
	absentTask := fixture.createTask(t, "absent")
	absentAttempt := fixture.spawn(t, absentTask.ID)
	if err := fixture.state.AuthorizeDiscard(absentTask.ID, absentAttempt.ID); err != nil {
		t.Fatal(err)
	}
	claimDead(t, fixture.databasePath, absentAttempt.ID)
	if err := fixture.service.Native.Remove(fixture.repo, absentAttempt.WorktreePath, true); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.service.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	classifications := map[string]ReconcileEntry{}
	for _, entry := range result.Classifications {
		classifications[entry.AttemptID] = entry
	}
	if entry := classifications[presentAttempt.ID]; entry.Classification != "released" || !strings.Contains(entry.Reason, "completed exact removal") {
		t.Fatalf("present release entry = %+v", entry)
	}
	if entry := classifications[absentAttempt.ID]; entry.Classification != "released" || !strings.Contains(entry.Reason, "recovered release") {
		t.Fatalf("absent release entry = %+v", entry)
	}
	for _, attemptID := range []string{presentAttempt.ID, absentAttempt.ID} {
		stored, _ := fixture.state.Attempt(attemptID)
		if stored.WorkspaceState != model.WorkspaceStateReleased || stored.ReleasedAt == nil {
			t.Fatalf("attempt %s = %q released_at=%v", attemptID, stored.WorkspaceState, stored.ReleasedAt)
		}
	}
	if _, err := os.Stat(presentAttempt.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("interrupted release recovery did not remove the worktree")
	}
}

// claimDead claims the release in a separate process that immediately exits,
// leaving a releasing state owned by a dead PID.
func claimDead(t *testing.T, databasePath, attemptID string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestReleaseClaimHelper$")
	command.Env = append(os.Environ(), "SHEPHRD_RELEASE_HELPER=1", "SHEPHRD_RELEASE_DB="+databasePath, "SHEPHRD_RELEASE_ATTEMPT="+attemptID)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("release claim helper: %s: %v", output, err)
	}
}
