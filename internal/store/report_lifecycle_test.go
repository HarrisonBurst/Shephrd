package store

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
)

func TestReportAcceptancePersistsVersionedHandlerInvocationsAtomically(t *testing.T) {
	state, task, attempt, message := reportLifecycleStoreFixture(t)
	defer state.Close()
	bindings := []model.ReportAcceptedHandlerBinding{
		{Name: "summary", ExtensionID: "fixture.summary", ConfigurationHash: strings.Repeat("a", 64)},
		{Name: "memory", ExtensionID: "fixture.memory", ConfigurationHash: strings.Repeat("b", 64)},
	}
	artifact, err := state.SetVerifiedReportDelivery(task.ID, attempt.ID, message.ID, model.VerifiedArtifact{
		Kind: "report", OriginalRef: message.ArtifactRef, SHA256: strings.Repeat("c", 64), SizeBytes: 12,
		SnapshotPath: filepath.Join(t.TempDir(), "snapshot.md")}, "accepted report", bindings...)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.AcceptedEventID == "" || artifact.AcceptedEventName != "report.accepted" || artifact.AcceptedEventVersion != 1 {
		t.Fatalf("accepted event = %+v", artifact)
	}
	invocations, err := state.ReportLifecycleInvocationsForEvent(artifact.AcceptedEventID)
	if err != nil || len(invocations) != 2 {
		t.Fatalf("invocations=%+v err=%v", invocations, err)
	}
	for index, invocation := range invocations {
		if invocation.EventID != artifact.AcceptedEventID || invocation.EventName != artifact.AcceptedEventName || invocation.EventVersion != artifact.AcceptedEventVersion || invocation.ArtifactID != artifact.ID || invocation.TaskID != task.ID || invocation.AttemptID != attempt.ID || invocation.Position != index+1 || invocation.State != "pending" || invocation.Attempts != 0 {
			t.Fatalf("invocation %d = %+v", index, invocation)
		}
	}
	if _, err := state.db.Exec(`UPDATE verified_artifacts SET accepted_event_id='changed' WHERE id=?`, artifact.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("immutable event update = %v", err)
	}
	if _, err := state.db.Exec(`UPDATE report_lifecycle_invocations SET handler_name='changed' WHERE id=?`, invocations[0].ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("immutable invocation identity update = %v", err)
	}
}

func TestReportLifecycleInvocationCrashRestartAndIdempotentRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	state, task, attempt, message := reportLifecycleStoreFixtureAt(t, path)
	binding := model.ReportAcceptedHandlerBinding{Name: "memory", ExtensionID: "fixture.memory", ConfigurationHash: strings.Repeat("a", 64)}
	artifact, err := state.SetVerifiedReportDelivery(task.ID, attempt.ID, message.ID, model.VerifiedArtifact{
		Kind: "report", OriginalRef: message.ArtifactRef, SHA256: strings.Repeat("b", 64), SizeBytes: 8, SnapshotPath: "/snapshot.md"}, "accepted report", binding)
	if err != nil {
		t.Fatal(err)
	}
	invocations, _ := state.ReportLifecycleInvocationsForEvent(artifact.AcceptedEventID)
	start := time.Date(2026, 8, 17, 1, 0, 0, 0, time.UTC)
	claimed, acquired, err := state.ClaimReportLifecycleInvocation(invocations[0].ID, binding.ConfigurationHash, start, start.Add(time.Minute))
	if err != nil || !acquired || claimed.Attempts != 1 || claimed.InvocationClaimToken == "" {
		t.Fatalf("claim=%+v acquired=%t err=%v", claimed, acquired, err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	inFlight, acquired, err := state.ClaimReportLifecycleInvocation(claimed.ID, binding.ConfigurationHash, start.Add(30*time.Second), start.Add(90*time.Second))
	if err != nil || acquired || inFlight.Attempts != 1 || inFlight.State != "invoking" {
		t.Fatalf("in-flight=%+v acquired=%t err=%v", inFlight, acquired, err)
	}
	retried, acquired, err := state.ClaimReportLifecycleInvocation(claimed.ID, binding.ConfigurationHash, start.Add(61*time.Second), start.Add(2*time.Minute))
	if err != nil || !acquired || retried.ID != claimed.ID || retried.EventID != claimed.EventID || retried.Attempts != 2 || retried.InvocationClaimToken == claimed.InvocationClaimToken {
		t.Fatalf("retry=%+v acquired=%t err=%v", retried, acquired, err)
	}
	completed, err := state.CompleteReportLifecycleInvocation(retried.ID, retried.InvocationClaimToken, "succeeded", "stable summary", "fixture-memory", "memory:"+retried.EventID, "", "", "", start.Add(62*time.Second))
	if err != nil || completed.State != "succeeded" || completed.ReceiptID == "" {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	again, acquired, err := state.ClaimReportLifecycleInvocation(completed.ID, binding.ConfigurationHash, start.Add(3*time.Minute), start.Add(4*time.Minute))
	if err != nil || acquired || !reflect.DeepEqual(again.CompletedAt, completed.CompletedAt) || again.Attempts != 2 {
		t.Fatalf("idempotent claim=%+v acquired=%t err=%v", again, acquired, err)
	}
}

func TestReportDoneNotificationWaitsForLifecycleOutcome(t *testing.T) {
	state, task, attempt, message := reportLifecycleStoreFixture(t)
	defer state.Close()
	binding := model.ReportAcceptedHandlerBinding{Name: "memory", ExtensionID: "fixture.memory", ConfigurationHash: strings.Repeat("a", 64)}
	artifact, err := state.SetVerifiedReportDelivery(task.ID, attempt.ID, message.ID, model.VerifiedArtifact{
		Kind: "report", OriginalRef: message.ArtifactRef, SHA256: strings.Repeat("b", 64), SizeBytes: 8, SnapshotPath: "/snapshot.md"}, "accepted report", binding)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := state.DrainNotifications(task.ID, task.DriverID, "generation:before-handler", 1)
	if err != nil || len(blocked.Notifications) != 0 {
		t.Fatalf("blocked drain=%+v err=%v", blocked, err)
	}
	if _, err := state.EligibleVerifiedReport(task.ID); err == nil {
		t.Fatal("report became selectable before its configured handler outcome")
	}
	invocations, _ := state.ReportLifecycleInvocationsForEvent(artifact.AcceptedEventID)
	now := time.Now().UTC()
	claimed, acquired, err := state.ClaimReportLifecycleInvocation(invocations[0].ID, binding.ConfigurationHash, now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("claim=%+v acquired=%t err=%v", claimed, acquired, err)
	}
	if _, err := state.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "failed", "", "", "", "external_failure", "visible failure", "none", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	presentable, err := state.DrainNotifications(task.ID, task.DriverID, "generation:after-handler", 1)
	if err != nil || len(presentable.Notifications) != 1 || presentable.Notifications[0].Kind != "done" || len(presentable.Notifications[0].ReportLifecycle) != 1 || presentable.Notifications[0].ReportLifecycle[0].FailureKind != "external_failure" || presentable.Notifications[0].ReportLifecycle[0].RetryCommand == "" {
		t.Fatalf("presentable drain=%+v err=%v", presentable, err)
	}
	if selected, err := state.EligibleVerifiedReport(task.ID); err != nil || selected.ID != artifact.ID {
		t.Fatalf("selectable report=%+v err=%v", selected, err)
	}
}

func TestReportVerificationFailurePresentationIsDurableIdempotentAndDoesNotExposeReportBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	state, task, attempt, _ := reportLifecycleStoreFixtureAt(t, path)
	message, err := state.RecordReportVerificationFailure(task.ID, attempt.ID, "report_file_invalid")
	if err != nil {
		t.Fatal(err)
	}
	if message.Direction != "system" || message.Type != model.ReportVerificationFailedMessageType || message.ArtifactRef != "" || !message.Wake || !strings.Contains(message.Payload, "report_file_invalid") || !strings.Contains(message.Payload, "bytes have not been presented") {
		t.Fatalf("failure message = %+v", message)
	}
	if strings.Contains(message.Payload, "secret report bytes") {
		t.Fatalf("failure presentation exposed report bytes: %q", message.Payload)
	}
	again, err := state.RecordReportVerificationFailure(task.ID, attempt.ID, "different_failure")
	if err != nil || again.ID != message.ID || again.Payload != message.Payload {
		t.Fatalf("idempotent failure=%+v err=%v", again, err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	drain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:verification-failure", 2)
	if err != nil || len(drain.Notifications) != 1 || drain.Notifications[0].Kind != model.ReportVerificationFailedMessageType || drain.Notifications[0].Artifact != "" || drain.Notifications[0].Payload != message.Payload {
		t.Fatalf("failure drain=%+v err=%v", drain, err)
	}
}

func TestReportDoneNotificationSurvivesReleaseWhileHandlerOutcomeIsPending(t *testing.T) {
	state, task, attempt, message := reportLifecycleStoreFixture(t)
	defer state.Close()
	binding := model.ReportAcceptedHandlerBinding{Name: "memory", ExtensionID: "fixture.memory", ConfigurationHash: strings.Repeat("a", 64)}
	artifact, err := state.SetVerifiedReportDelivery(task.ID, attempt.ID, message.ID, model.VerifiedArtifact{
		Kind: "report", OriginalRef: message.ArtifactRef, SHA256: strings.Repeat("b", 64), SizeBytes: 8, SnapshotPath: "/snapshot.md"}, "accepted report", binding)
	if err != nil {
		t.Fatal(err)
	}
	claimedRelease, _, err := state.ClaimRelease(attempt.ID)
	if err != nil || !claimedRelease {
		t.Fatalf("release claim=%t err=%v", claimedRelease, err)
	}
	if err := state.MarkReleased(attempt.ID); err != nil {
		t.Fatal(err)
	}
	blocked, err := state.DrainNotifications(task.ID, task.DriverID, "generation:released-pending", 1)
	if err != nil || len(blocked.Notifications) != 0 {
		t.Fatalf("released pending drain=%+v err=%v", blocked, err)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil || len(notifications) != 1 || notifications[0].State != model.NotificationPending {
		t.Fatalf("preserved notifications=%+v err=%v", notifications, err)
	}
	invocations, err := state.ReportLifecycleInvocationsForEvent(artifact.AcceptedEventID)
	if err != nil || len(invocations) != 1 {
		t.Fatalf("invocations=%+v err=%v", invocations, err)
	}
	now := time.Now().UTC()
	claimed, acquired, err := state.ClaimReportLifecycleInvocation(invocations[0].ID, binding.ConfigurationHash, now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("handler claim=%+v acquired=%t err=%v", claimed, acquired, err)
	}
	if _, err := state.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "succeeded", "visible summary", "fixture-memory", "memory:"+claimed.EventID, "", "", "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	presentable, err := state.DrainNotifications(task.ID, task.DriverID, "generation:released-terminal", 1)
	if err != nil || len(presentable.Notifications) != 1 || presentable.Notifications[0].Kind != "done" || len(presentable.Notifications[0].ReportLifecycle) != 1 {
		t.Fatalf("released terminal drain=%+v err=%v", presentable, err)
	}
	notice := presentable.Notifications[0]
	if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: notice.NotificationID, ConsumerID: task.DriverID, DriverGeneration: notice.Claim.DriverGeneration, ClaimToken: notice.Claim.ClaimToken, HandlingID: "handling:released-report"}); err != nil {
		t.Fatalf("ack released report: %v", err)
	}
}

func TestReportLifecycleOutcomeHasNoTaskOrArtifactAuthority(t *testing.T) {
	state, task, attempt, message := reportLifecycleStoreFixture(t)
	defer state.Close()
	binding := model.ReportAcceptedHandlerBinding{Name: "memory", ExtensionID: "fixture.memory", ConfigurationHash: strings.Repeat("a", 64)}
	artifact, err := state.SetVerifiedReportDelivery(task.ID, attempt.ID, message.ID, model.VerifiedArtifact{
		Kind: "report", OriginalRef: message.ArtifactRef, SHA256: strings.Repeat("b", 64), SizeBytes: 8, SnapshotPath: "/snapshot.md"}, "accepted report", binding)
	if err != nil {
		t.Fatal(err)
	}
	beforeTask, _ := state.Task(task.ID)
	beforeArtifact, _ := state.VerifiedArtifactForDoneMessage(message.ID)
	invocations, _ := state.ReportLifecycleInvocationsForEvent(artifact.AcceptedEventID)
	now := time.Now().UTC()
	claimed, acquired, err := state.ClaimReportLifecycleInvocation(invocations[0].ID, binding.ConfigurationHash, now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatal(err)
	}
	if _, err := state.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "failed", "", "", "", "external_failure", "memory write failed", "unknown", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	afterTask, _ := state.Task(task.ID)
	afterArtifact, _ := state.VerifiedArtifactForDoneMessage(message.ID)
	if !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeArtifact, afterArtifact) {
		t.Fatalf("handler outcome mutated authority\ntask before=%+v after=%+v\nartifact before=%+v after=%+v", beforeTask, afterTask, beforeArtifact, afterArtifact)
	}
}

func reportLifecycleStoreFixture(t *testing.T) (*Store, model.Task, model.Attempt, model.Message) {
	t.Helper()
	return reportLifecycleStoreFixtureAt(t, filepath.Join(t.TempDir(), "state.db"))
}

func reportLifecycleStoreFixtureAt(t *testing.T, path string) (*Store, model.Task, model.Attempt, model.Message) {
	t.Helper()
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "report-lifecycle", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Lifecycle report", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "lifecycle", Objective: "write report", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(attempt.ID, "session", t.TempDir(), "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation := prepareAttempt(t, state, attempt)
	recordWorkerCheckpoint(t, state, attempt, generation, 1, []string{})
	message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "report complete", Artifact: "report:/tmp/report.md"}, 2, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return state, task, attempt, message
}
