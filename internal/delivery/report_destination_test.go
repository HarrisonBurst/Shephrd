package delivery

import (
	"os"
	"path/filepath"
	"testing"

	"shephrd/internal/model"
)

func TestReportDestinationVerificationAndRecovery(t *testing.T) {
	data := t.TempDir()
	verifier := New(data)
	attempt := model.Attempt{ID: "attempt_report", TaskID: "task_report", RunGeneration: 3}
	path, _ := model.RunReportPath(data, attempt.TaskID, attempt.ID, attempt.RunGeneration)
	attempt.ReportPath = path
	legacy := filepath.Join(data, attempt.TaskID, "report.md")
	stale, _ := model.RunReportPath(data, attempt.TaskID, attempt.ID, 2)
	foreign, _ := model.RunReportPath(data, attempt.TaskID, "attempt_foreign", 3)
	for _, candidate := range []string{legacy, stale, foreign, path} {
		if err := os.MkdirAll(filepath.Dir(candidate), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(candidate, []byte(candidate), 0o600); err != nil {
			t.Fatal(err)
		}
		task := model.Task{ID: attempt.TaskID, Deliverable: "report", ArtifactRef: "report:" + candidate}
		result, err := verifier.Verify(task, model.Repo{}, attempt, model.AttemptCheckpoint{})
		if err != nil || result.Landed != (candidate == path) {
			t.Fatalf("verify %s: %+v err=%v", candidate, result, err)
		}
	}
	evidence, err := verifier.ValidateReportRecovery(attempt.TaskID, attempt)
	if err != nil || evidence.CanonicalPath != path {
		t.Fatalf("recovery=%+v err=%v", evidence, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.ValidateReportRecovery(attempt.TaskID, attempt); err == nil {
		t.Fatal("recovery fell back to prior report")
	}
	attempt.ReportPath = foreign
	if _, err := verifier.CanonicalReportPath(attempt.TaskID, attempt); err == nil {
		t.Fatal("foreign bound destination accepted")
	}
	attempt.ReportPath = ""
	legacyEvidence, err := verifier.ValidateReportRecovery(attempt.TaskID, attempt)
	if err != nil || legacyEvidence.CanonicalPath != legacy {
		t.Fatalf("legacy recovery=%+v err=%v", legacyEvidence, err)
	}
	result, err := verifier.Verify(model.Task{ID: attempt.TaskID, Deliverable: "report", ArtifactRef: "report:" + legacy}, model.Repo{}, attempt, model.AttemptCheckpoint{})
	if err != nil || !result.Landed {
		t.Fatalf("legacy verification=%+v err=%v", result, err)
	}
}
