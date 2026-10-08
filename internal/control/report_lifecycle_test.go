package control

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestReportAcceptedHandlerSummarizesAndReturnsIdempotentMemoryReceipt(t *testing.T) {
	executable := buildReportHandlerFixture(t)
	memoryDir := filepath.Join(t.TempDir(), "memory")
	service, state, task, attempt := reportLifecycleControlFixture(t, executable, memoryDir, "success")
	defer state.Close()
	result, err := service.verifyDelivery(task, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Landed || result.VerifiedArtifact == nil || result.VerifiedArtifact.AcceptedEventName != "report.accepted" || len(result.LifecycleHandlers) != 1 {
		t.Fatalf("result = %+v", result)
	}
	outcome := result.LifecycleHandlers[0]
	if outcome.State != "succeeded" || outcome.Attempts != 1 || !strings.Contains(outcome.Annotation, "Deterministic finding") || outcome.ReceiptSystem != "fixture-memory" || outcome.ReceiptID != "memory:"+result.VerifiedArtifact.AcceptedEventID {
		t.Fatalf("outcome = %+v", outcome)
	}
	entries, err := os.ReadDir(memoryDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("memory entries=%v err=%v", entries, err)
	}
	second, err := service.verifyDelivery(task, attempt)
	if err != nil || len(second.LifecycleHandlers) != 1 || second.LifecycleHandlers[0].Attempts != 1 || second.LifecycleHandlers[0].ID != outcome.ID || second.LifecycleHandlers[0].ReceiptID != outcome.ReceiptID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	entries, err = os.ReadDir(memoryDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("idempotent memory entries=%v err=%v", entries, err)
	}
}

func TestReportAcceptedHandlerFailureIsVisibleWithoutRollingBackAcceptance(t *testing.T) {
	executable := buildReportHandlerFixture(t)
	service, state, task, attempt := reportLifecycleControlFixture(t, executable, filepath.Join(t.TempDir(), "memory"), "fail")
	defer state.Close()
	result, err := service.verifyDelivery(task, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Landed || result.VerifiedArtifact == nil || len(result.LifecycleHandlers) != 1 {
		t.Fatalf("result = %+v", result)
	}
	outcome := result.LifecycleHandlers[0]
	if outcome.State != "failed" || outcome.FailureKind != "fixture_memory_unavailable" || outcome.FailureMessage == "" || outcome.RetryCommand == "" || !outcome.Recoverable {
		t.Fatalf("failure outcome = %+v", outcome)
	}
	current, err := state.Task(task.ID)
	if err != nil || !current.Landed || current.Status != model.TaskStatusDone || current.ArtifactRef == "" {
		t.Fatalf("accepted task=%+v err=%v", current, err)
	}
	artifact, err := state.VerifiedArtifactForDoneMessage(result.VerifiedArtifact.DoneMessageID)
	if err != nil || artifact == nil || artifact.ID != result.VerifiedArtifact.ID {
		t.Fatalf("artifact=%+v err=%v", artifact, err)
	}
	detail, err := state.Detail(task.ID)
	if err != nil || len(detail.ReportLifecycleInvocations) != 1 || detail.ReportLifecycleInvocations[0].FailureKind != outcome.FailureKind {
		t.Fatalf("detail=%+v err=%v", detail.ReportLifecycleInvocations, err)
	}
}

func TestReportVerificationFailureCreatesVisibleSanitizedWakeWhileDoneRemainsHidden(t *testing.T) {
	executable := buildReportHandlerFixture(t)
	service, state, task, attempt := reportLifecycleControlFixture(t, executable, filepath.Join(t.TempDir(), "memory"), "success")
	defer state.Close()
	reportPath := strings.TrimPrefix(task.ArtifactRef, "report:")
	if err := os.WriteFile(reportPath, []byte("secret unverified report bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reportPath); err != nil {
		t.Fatal(err)
	}
	if _, err := service.verifyDelivery(task, attempt); err == nil {
		t.Fatal("invalid report verification succeeded")
	}
	drain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:verification-failure", 2)
	if err != nil || len(drain.Notifications) != 1 {
		t.Fatalf("failure drain=%+v err=%v", drain, err)
	}
	notice := drain.Notifications[0]
	if notice.Kind != model.ReportVerificationFailedMessageType || notice.Artifact != "" || !strings.Contains(notice.Payload, "bytes have not been presented") || strings.Contains(notice.Payload, "secret unverified report bytes") {
		t.Fatalf("failure notice = %+v", notice)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil || len(notifications) != 2 || notifications[0].Kind != "done" || notifications[0].State != model.NotificationPending {
		t.Fatalf("stored notifications=%+v err=%v", notifications, err)
	}
}

func TestRecoveredReportAcceptanceUsesSameLifecycleBoundary(t *testing.T) {
	executable := buildReportHandlerFixture(t)
	memoryDir := filepath.Join(t.TempDir(), "memory")
	service, state, task, attempt, checkpoint, _ := reportRecoveryControlFixture(t)
	defer state.Close()
	digest := sha256.Sum256(mustReadFile(t, executable))
	service.Config.LifecycleHandlers.ReportAccepted = []config.ReportAcceptedHandler{{
		Name: "memory", ExtensionID: "fixture.report-memory", Command: []string{executable, memoryDir, "success"}, SHA256: hex.EncodeToString(digest[:]),
	}}
	if _, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor); err != nil {
		t.Fatal(err)
	}
	result, err := service.VerifyAttempt(task.ID, attempt.ID, task.DriverID)
	if err != nil {
		t.Fatal(err)
	}
	if result.CompletionProvenance != model.CompletionProvenanceReportRecovery || result.VerifiedArtifact == nil || result.VerifiedArtifact.AcceptedEventName != "report.accepted" || len(result.LifecycleHandlers) != 1 || result.LifecycleHandlers[0].State != "succeeded" || result.LifecycleHandlers[0].ReceiptID == "" {
		t.Fatalf("recovered result = %+v", result)
	}
}

func TestReportAcceptedUnknownSideEffectRetriesWithStableIdentity(t *testing.T) {
	executable := buildReportHandlerFixture(t)
	memoryDir := filepath.Join(t.TempDir(), "memory")
	service, state, task, attempt := reportLifecycleControlFixture(t, executable, memoryDir, "unknown")
	defer state.Close()
	first, err := service.verifyDelivery(task, attempt)
	if err != nil || len(first.LifecycleHandlers) != 1 || first.LifecycleHandlers[0].State != "unknown" || first.LifecycleHandlers[0].Effect != "unknown" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := service.verifyDelivery(task, attempt)
	if err != nil || len(second.LifecycleHandlers) != 1 || second.LifecycleHandlers[0].State != "unknown" || second.LifecycleHandlers[0].Attempts != 2 || second.LifecycleHandlers[0].ID != first.LifecycleHandlers[0].ID || second.LifecycleHandlers[0].EventID != first.LifecycleHandlers[0].EventID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	entries, err := os.ReadDir(memoryDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("memory entries=%v err=%v", entries, err)
	}
}

func reportLifecycleControlFixture(t *testing.T, executable, memoryDir, mode string) (Service, *store.Store, model.Task, model.Attempt) {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "report-handler", Path: root, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Report handler", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "handler-" + mode, Objective: "investigate", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", root, "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	attempt.RunGeneration = prepareReportAttempt(t, state, attempt)
	reportPath := filepath.Join(dataDir, task.ID, "report.md")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, []byte("# Deterministic finding\nMemory-worthy detail.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: 1, Summary: "report complete", NextSteps: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "done", Payload: "complete", Artifact: "report:" + reportPath}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(mustReadFile(t, executable))
	cfg := config.Config{DataDir: dataDir, LifecycleHandlers: config.LifecycleHandlers{ReportAccepted: []config.ReportAcceptedHandler{{
		Name: "memory", ExtensionID: "fixture.report-memory", Command: []string{executable, memoryDir, mode}, SHA256: hex.EncodeToString(digest[:]),
	}}}}
	service := New(cfg, state)
	task, err = state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return service, state, task, attempt
}

func buildReportHandlerFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "report-handler")
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	command := exec.Command("go", "build", "-o", executable, "./internal/lifecycle/testdata/reporthandler")
	command.Dir = projectRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build report handler fixture: %s: %v", output, err)
	}
	return executable
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
