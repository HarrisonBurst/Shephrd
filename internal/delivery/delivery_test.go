package delivery

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

type fakeRunner struct {
	gh     []byte
	git    []byte
	gitErr error
	calls  []string
}

func (f *fakeRunner) Run(dir, command string, args ...string) ([]byte, []byte, error) {
	call := command + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if command == "gh" {
		return f.gh, nil, nil
	}
	if command == "git" {
		return f.git, nil, f.gitErr
	}
	return nil, nil, errors.New("unexpected command")
}

func TestReportMustUseTaskArtifactPath(t *testing.T) {
	data := t.TempDir()
	verifier := New(data)
	task := model.Task{Title: "Test task", ID: "task-1", Deliverable: "report", ArtifactRef: "report:/etc/hosts"}
	result, err := verifier.Verify(task, model.Repo{}, model.Attempt{}, model.AttemptCheckpoint{})
	if err != nil || result.Landed || !strings.Contains(result.Reason, filepath.Join(data, task.ID, "report.md")) {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	path := filepath.Join(data, task.ID, "report.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	task.ArtifactRef = "report:" + path
	result, err = verifier.Verify(task, model.Repo{}, model.Attempt{}, model.AttemptCheckpoint{})
	if err != nil || !result.Landed {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestReportInputBoundsUTF8AndTampering(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
		want string
	}{
		{name: "exact limit", body: bytes.Repeat([]byte("a"), int(MaxReportInputBytes))},
		{name: "limit plus one", body: bytes.Repeat([]byte("a"), int(MaxReportInputBytes)+1), want: "limited"},
		{name: "invalid UTF-8", body: []byte{0xff}, want: "UTF-8"},
		{name: "NUL", body: []byte("a\x00b"), want: "NUL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := t.TempDir()
			verifier := New(data)
			task := model.Task{Title: "Test task", ID: "task-1", Deliverable: "report"}
			path := filepath.Join(data, task.ID, "report.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, test.body, 0o600); err != nil {
				t.Fatal(err)
			}
			task.ArtifactRef = "report:" + path
			result, err := verifier.Verify(task, model.Repo{}, model.Attempt{}, model.AttemptCheckpoint{})
			if err != nil || result.VerifiedArtifact == nil {
				t.Fatalf("verify result=%+v err=%v", result, err)
			}
			result.VerifiedArtifact.ProducerTaskID = task.ID
			body, err := verifier.ReadVerifiedReport(*result.VerifiedArtifact)
			if test.want == "" {
				if err != nil || !bytes.Equal(body, test.body) {
					t.Fatalf("body size=%d err=%v", len(body), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}

	data := t.TempDir()
	verifier := New(data)
	task := model.Task{Title: "Test task", ID: "task-tamper", Deliverable: "report"}
	path := filepath.Join(data, task.ID, "report.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	task.ArtifactRef = "report:" + path
	result, err := verifier.Verify(task, model.Repo{}, model.Attempt{}, model.AttemptCheckpoint{})
	if err != nil || result.VerifiedArtifact == nil {
		t.Fatalf("verify result=%+v err=%v", result, err)
	}
	if err := os.Chmod(result.VerifiedArtifact.SnapshotPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result.VerifiedArtifact.SnapshotPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifier.ValidateSnapshot(*result.VerifiedArtifact); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("tamper error=%v", err)
	}
}

func TestPushedBranchIsDeliveryNotLanding(t *testing.T) {
	runner := &fakeRunner{git: []byte("worker\trefs/heads/shephrd/task-1\n")}
	verifier := Verifier{Runner: runner, DataDir: t.TempDir()}
	task := model.Task{Title: "Test task", ID: "task-1", RepoPath: "/repo", Deliverable: "code", ArtifactRef: "branch:shephrd/task-1"}
	attempt := model.Attempt{ID: "attempt-1", RunGeneration: 1, Branch: "shephrd/task-1"}
	checkpoint := model.AttemptCheckpoint{AttemptID: attempt.ID, Producer: "worker", RunGeneration: 1, Revision: 2, HeadCommit: "worker"}
	result, err := verifier.Verify(task, model.Repo{DefaultBranch: "main"}, attempt, checkpoint)
	if err != nil || result.Landed || !result.BranchPushed || result.RemoteDelivery.State != "pushed" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if len(runner.calls) != 1 || !strings.Contains(runner.calls[0], "ls-remote") {
		t.Fatalf("calls = %v", runner.calls)
	}
}

func TestPRLandingRequiresMergedExactHeadAndRegisteredBase(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  string
		base   string
		head   string
		landed bool
	}{
		{name: "open", state: "OPEN", base: "main", head: "worker"},
		{name: "closed unmerged", state: "CLOSED", base: "main", head: "worker"},
		{name: "wrong base", state: "MERGED", base: "release", head: "worker"},
		{name: "wrong head", state: "MERGED", base: "main", head: "other"},
		{name: "merged exact", state: "MERGED", base: "main", head: "worker", landed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{git: []byte("worker\trefs/heads/shephrd/task-1\n"), gh: []byte(`{"state":"` + test.state + `","mergedAt":"2026-08-12T00:00:00Z","baseRefName":"` + test.base + `","headRefName":"shephrd/task-1","headRefOid":"` + test.head + `","url":"https://github.test/o/r/pull/1","mergeCommit":{"oid":"merge"}}`)}
			verifier := Verifier{Runner: runner}
			task := model.Task{Title: "Test task", ID: "task-1", RepoPath: "/repo", Deliverable: "code", ArtifactRef: "https://github.test/o/r/pull/1"}
			attempt := model.Attempt{ID: "attempt-1", RunGeneration: 1, Branch: "shephrd/task-1"}
			checkpoint := model.AttemptCheckpoint{AttemptID: attempt.ID, Producer: "worker", RunGeneration: 1, Revision: 2, HeadCommit: "worker"}
			result, err := verifier.Verify(task, model.Repo{DefaultBranch: "main"}, attempt, checkpoint)
			if err != nil || result.Landed != test.landed {
				t.Fatalf("result = %+v, err = %v", result, err)
			}
			if test.landed && (result.Landing.Kind != "github_pr" || result.Landing.SourceCommit != "worker" || result.Landing.TargetCommit != "merge") {
				t.Fatalf("landing = %+v", result.Landing)
			}
		})
	}
}
