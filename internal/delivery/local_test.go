package delivery

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

type recordingExec struct {
	calls []string
}

func (r *recordingExec) Run(dir, command string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, command+" "+strings.Join(args, " "))
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func TestOfflineLocalLandingUsesExactAncestryAndObservesDirtyRoot(t *testing.T) {
	root, worker, base, source := localRepo(t)
	runner := &recordingExec{}
	verifier := Verifier{Runner: runner}
	task := model.Task{Title: "Test task", ID: "task-1", Deliverable: "code"}
	repo := model.Repo{Path: root, DefaultBranch: "main"}
	attempt := model.Attempt{ID: "attempt-1", RunGeneration: 1, SessionID: "session", WorktreePath: worker,
		Branch: "shephrd/task-1", BaseCommit: base, ReleaseState: "held"}
	checkpoint := model.AttemptCheckpoint{AttemptID: attempt.ID, Producer: "worker", RunGeneration: 1, Revision: 2,
		SessionID: attempt.SessionID, Branch: attempt.Branch, HeadCommit: source}
	result, err := verifier.VerifyLocal(task, repo, attempt, checkpoint, "branch:"+attempt.Branch)
	if err != nil || result.Landed || !strings.Contains(result.Reason, "not contained") {
		t.Fatalf("before merge result = %+v, err = %v", result, err)
	}
	git(t, root, "merge", "--no-ff", "-m", "merge worker", attempt.Branch)
	dirtyPath := filepath.Join(root, "unrelated.tmp")
	before := []byte("unchanged")
	if err := os.WriteFile(dirtyPath, before, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = verifier.VerifyLocal(task, repo, attempt, checkpoint, "branch:"+attempt.Branch)
	if err != nil || !result.Landed || result.Landing.Kind != "local_default_branch" || !result.Worktree.RootDirty {
		t.Fatalf("after merge result = %+v, err = %v", result, err)
	}
	after, err := os.ReadFile(dirtyPath)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("dirty root changed: %q, %v", after, err)
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "gh ") || strings.Contains(call, "ls-remote") {
			t.Fatalf("offline verifier made network-capable call %q", call)
		}
	}
}

func TestLocalLandingRejectsSquashAndMovedOrDirtyWorker(t *testing.T) {
	t.Run("squash", func(t *testing.T) {
		root, worker, base, source := localRepo(t)
		git(t, root, "merge", "--squash", "shephrd/task-1")
		git(t, root, "commit", "-m", "squash worker")
		result, err := localResult(root, worker, base, source)
		if err != nil || result.Landed {
			t.Fatalf("result = %+v, err = %v", result, err)
		}
	})
	t.Run("moved branch", func(t *testing.T) {
		root, worker, base, source := localRepo(t)
		git(t, worker, "commit", "--allow-empty", "-m", "moved")
		_, err := localResult(root, worker, base, source)
		if err == nil || !strings.Contains(err.Error(), "moved") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("dirty worker", func(t *testing.T) {
		root, worker, base, source := localRepo(t)
		if err := os.WriteFile(filepath.Join(worker, "dirty"), []byte("dirty"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := localResult(root, worker, base, source)
		if err == nil || !strings.Contains(err.Error(), "currently dirty") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestLocalLandingRejectsNoChangeAttempt(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "commit", "--allow-empty", "-m", "base")
	base := git(t, root, "rev-parse", "HEAD")
	attempt := model.Attempt{ID: "attempt-1", RunGeneration: 1, SessionID: "session", WorktreePath: root,
		Branch: "main", BaseCommit: base, ReleaseState: "held"}
	checkpoint := model.AttemptCheckpoint{AttemptID: attempt.ID, Producer: "worker", RunGeneration: 1, Revision: 2,
		SessionID: attempt.SessionID, Branch: attempt.Branch, HeadCommit: base}
	result, err := (Verifier{Runner: &recordingExec{}}).VerifyLocal(model.Task{Title: "Test task", ID: "task-1", Deliverable: "code"},
		model.Repo{Path: root, DefaultBranch: "main"}, attempt, checkpoint, "branch:main")
	if err != nil || result.Landed || !strings.Contains(result.Reason, "no code delta") {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func localRepo(t *testing.T) (string, string, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	worker := filepath.Join(t.TempDir(), "worker")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "commit", "--allow-empty", "-m", "base")
	base := git(t, root, "rev-parse", "HEAD")
	git(t, root, "worktree", "add", "-b", "shephrd/task-1", worker, "main")
	if err := os.WriteFile(filepath.Join(worker, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, worker, "add", "work.txt")
	git(t, worker, "commit", "-m", "worker")
	return root, worker, base, git(t, worker, "rev-parse", "HEAD")
}

func localResult(root, worker, base, source string) (Result, error) {
	attempt := model.Attempt{ID: "attempt-1", RunGeneration: 1, SessionID: "session", WorktreePath: worker,
		Branch: "shephrd/task-1", BaseCommit: base, ReleaseState: "held"}
	checkpoint := model.AttemptCheckpoint{AttemptID: attempt.ID, Producer: "worker", RunGeneration: 1, Revision: 2,
		SessionID: attempt.SessionID, Branch: attempt.Branch, HeadCommit: source}
	return (Verifier{Runner: &recordingExec{}}).VerifyLocal(model.Task{Title: "Test task", ID: "task-1", Deliverable: "code"},
		model.Repo{Path: root, DefaultBranch: "main"}, attempt, checkpoint, "branch:"+attempt.Branch)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), output, err)
	}
	return strings.TrimSpace(string(output))
}
