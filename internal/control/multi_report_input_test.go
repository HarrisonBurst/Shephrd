package control

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestMultipleExplicitReportsKeepIdentityDigestAndOrderAcrossRetryAndRelaunch(t *testing.T) {
	fixture := newReportInputFixture(t, "FIRST_REPORT_BODY\n")
	defer fixture.state.Close()
	secondTask, secondAttempt, secondArtifact, secondPath := addVerifiedReportProducer(t, fixture, "second-report", "SECOND_REPORT_BODY\n")
	target, err := fixture.service.CreateTaskWithReports(model.Task{Title: "Test task", DriverID: "driver:attacher", RepoID: fixture.targetRepo.ID, FeatureKey: "two-reports",
		Objective: "implement", AcceptanceCriteria: "checks pass", Deliverable: "code"}, []string{secondTask.ID, fixture.producer.ID})
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := fixture.state.ReportInputs(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0].Position != 1 || inputs[0].ArtifactID != secondArtifact.ID || inputs[1].Position != 2 || inputs[1].ArtifactID != fixture.artifact.ID {
		t.Fatalf("ordered inputs = %+v", inputs)
	}
	attempt1 := configureTargetAttempt(t, fixture, target, "pi")
	initialPath, _, err := fixture.service.renderBrief(target, fixture.targetRepo, attempt1)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "recover", NextSteps: []string{"continue"}}
	if _, err := fixture.state.AddEventForRun(attempt1.ID, attempt1.RunGeneration, model.Event{Type: "checkpoint", Payload: "recover", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.AddEventForRun(attempt1.ID, attempt1.RunGeneration, model.Event{Type: "blocked", Payload: "recover"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	relaunchPath, _, err := fixture.service.renderRecoveryBrief(target, fixture.targetRepo, attempt1, "same-worktree relaunch", "recover")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.state.PrepareRetry(target.ID); err != nil {
		t.Fatal(err)
	}
	target, _ = fixture.state.Task(target.ID)
	attempt2 := configureTargetAttempt(t, fixture, target, "codex")
	retryPath, _, err := fixture.service.renderRecoveryBrief(target, fixture.targetRepo, attempt2, "clean retry", "recover")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.reportPath, []byte("MUTATED_FIRST_SOURCE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("MUTATED_SECOND_SOURCE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{initialPath, relaunchPath, retryPath} {
		brief := readBrief(t, path)
		firstIndex := strings.Index(brief, "SECOND_REPORT_BODY")
		secondIndex := strings.Index(brief, "FIRST_REPORT_BODY")
		if firstIndex < 0 || secondIndex < firstIndex {
			t.Fatalf("brief %s lost explicit report order:\n%s", path, brief)
		}
		for _, want := range []string{secondTask.ID, secondAttempt.ID, secondArtifact.ID, secondArtifact.SHA256,
			fixture.producer.ID, fixture.producerAttempt.ID, fixture.artifact.ID, fixture.artifact.SHA256,
			"Explicit predecessor report 1 of 2", "Explicit predecessor report 2 of 2", "patches, transcripts"} {
			if !strings.Contains(brief, want) {
				t.Fatalf("brief %s missing %q", path, want)
			}
		}
		for _, forbidden := range []string{"MUTATED_FIRST_SOURCE", "MUTATED_SECOND_SOURCE"} {
			if strings.Contains(brief, forbidden) {
				t.Fatalf("brief %s contains mutable source %q", path, forbidden)
			}
		}
	}
}

func addVerifiedReportProducer(t *testing.T, fixture reportInputFixture, feature, body string) (model.Task, model.Attempt, model.VerifiedArtifact, string) {
	t.Helper()
	producer, err := fixture.state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:producer", RepoID: fixture.producerRepo.ID, FeatureKey: feature,
		Objective: feature, Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.state.BeginAttempt(producer.ID, "pi", "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, fixture.state, attempt.ID, "session", fixture.producerRepo.Path, "lease-"+feature, "branch-"+feature); err != nil {
		t.Fatal(err)
	}
	attempt.RunGeneration = prepareReportAttempt(t, fixture.state, attempt)
	path := filepath.Join(fixture.dataDir, producer.ID, "report.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "done", NextSteps: []string{}}
	if _, err := fixture.state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "checkpoint", Payload: "done", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "done", Payload: "done", Artifact: "report:" + path}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	producer, _ = fixture.state.Task(producer.ID)
	result, err := fixture.service.verifyDelivery(producer, attempt)
	if err != nil || result.VerifiedArtifact == nil {
		t.Fatalf("verify %s: result=%+v err=%v", feature, result, err)
	}
	if result.VerifiedArtifact.ProducerAttemptID != attempt.ID {
		t.Fatal(fmt.Sprintf("artifact attempt = %s, want %s", result.VerifiedArtifact.ProducerAttemptID, attempt.ID))
	}
	return producer, attempt, *result.VerifiedArtifact, path
}
