package store

import (
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
)

func TestReportRecoveryAttestationAndVerifiedCompletion(t *testing.T) {
	state, task, attempt, checkpoint := reportRecoveryStoreFixture(t)
	defer state.Close()
	candidate, err := state.ReportRecoveryCandidateFor(task.ID, attempt.ID, task.DriverID, attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil {
		t.Fatal(err)
	}
	evidence := reportRecoveryStoreEvidence(task, attempt)
	attestation, idempotent, err := state.RecordReportRecoveryAttestation(candidate, evidence)
	if err != nil || idempotent {
		t.Fatalf("attestation=%+v idempotent=%t err=%v", attestation, idempotent, err)
	}
	again, idempotent, err := state.RecordReportRecoveryAttestation(candidate, evidence)
	if err != nil || !idempotent || again.ID != attestation.ID {
		t.Fatalf("again=%+v idempotent=%t err=%v", again, idempotent, err)
	}
	if _, err := state.db.Exec(`UPDATE report_recovery_attestations SET reason='changed' WHERE id=?`, attestation.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("mutable attestation error = %v", err)
	}
	artifact := model.VerifiedArtifact{Kind: "report", OriginalRef: "report:" + attestation.CanonicalReportPath,
		SHA256: attestation.SHA256, SizeBytes: attestation.SizeBytes, SnapshotPath: "/data/artifacts/sha256/" + attestation.SHA256 + ".md"}
	stored, err := state.SetVerifiedRecoveredReportDelivery(candidate, attestation, artifact, "driver report recovery verified")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ReportRecoveryID != attestation.ID || stored.DoneMessageID == 0 {
		t.Fatalf("artifact = %+v", stored)
	}
	current := mustTask(t, state, task.ID)
	if current.Status != model.TaskStatusDone || current.ClaimedDone || current.CompletionProvenance != model.CompletionProvenanceReportRecovery || !current.Landed || current.ArtifactRef != artifact.OriginalRef {
		t.Fatalf("task = %+v", current)
	}
	storedAttempt, err := state.Attempt(attempt.ID)
	if err != nil || storedAttempt.Status != model.AttemptStatusDone || !storedAttempt.LandedProven || storedAttempt.LandingKind != "report_artifact" || storedAttempt.LandedCheckpointRevision != checkpoint.Revision {
		t.Fatalf("attempt=%+v err=%v", storedAttempt, err)
	}
	if _, err := state.CompleteLandingProof(attempt.ID); err != nil {
		t.Fatal(err)
	}
	messages, err := state.Messages(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	workerDone, recoveryMessages := 0, 0
	for _, message := range messages {
		workerDone += boolToInt(message.Direction == "worker-to-driver" && message.Type == "done" && !message.Stale)
		recoveryMessages += boolToInt(message.Direction == "system" && message.Type == "report-recovery" && !message.Stale)
	}
	if workerDone != 0 || recoveryMessages != 1 {
		t.Fatalf("worker_done=%d recovery_messages=%d messages=%+v", workerDone, recoveryMessages, messages)
	}
	candidate, err = state.ReportRecoveryCandidateFor(task.ID, attempt.ID, task.DriverID, attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := state.SetVerifiedRecoveredReportDelivery(candidate, attestation, artifact, "driver report recovery verified")
	if err != nil || repeated.ID != stored.ID {
		t.Fatalf("repeated=%+v err=%v", repeated, err)
	}
	eligible, err := state.EligibleVerifiedReport(task.ID)
	if err != nil || eligible.ID != stored.ID || eligible.ReportRecoveryID != attestation.ID {
		t.Fatalf("eligible=%+v err=%v", eligible, err)
	}
	target, err := state.CreateTaskWithReportInputs(model.Task{Title: "Target", DriverID: "driver:target", RepoID: task.RepoID,
		FeatureKey: "target", Objective: "consume recovered report"}, []model.ReportArtifactSelection{{ProducerTaskID: task.ID, ArtifactID: stored.ID}})
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := state.ReportInputs(target.ID)
	if err != nil || len(inputs) != 1 || inputs[0].ReportRecoveryID != attestation.ID {
		t.Fatalf("inputs=%+v err=%v", inputs, err)
	}
	late, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "done", Payload: "late", Artifact: artifact.OriginalRef}, checkpoint.SourceCursor+10, model.WorkspaceFacts{})
	if err != nil || !late.Stale {
		t.Fatalf("late=%+v error=%v", late, err)
	}
	if after := mustTask(t, state, task.ID); after.CompletionProvenance != model.CompletionProvenanceReportRecovery || after.ArtifactRef != artifact.OriginalRef {
		t.Fatalf("late worker changed recovery = %+v", after)
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.BeginAttempt(task.ID, "pi", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CompleteLandingProof(attempt.ID); err != nil {
		t.Fatalf("retry erased recovered attempt proof: %v", err)
	}
	if _, err := state.EligibleVerifiedReport(task.ID); err == nil {
		t.Fatal("superseded recovered report remained current input eligible")
	}
}

func TestReportRecoveryAttestationAllowsOnlyExplicitCleanRetryOfNewAttempt(t *testing.T) {
	state, task, attempt, checkpoint := reportRecoveryStoreFixture(t)
	defer state.Close()
	candidate, err := state.ReportRecoveryCandidateFor(task.ID, attempt.ID, task.DriverID, attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil {
		t.Fatal(err)
	}
	attestation, _, err := state.RecordReportRecoveryAttestation(candidate, reportRecoveryStoreEvidence(task, attempt))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ReserveRunGeneration(attempt.ID); err == nil || attestationErrorKind(err) != "report_recovery_continuation_forbidden" {
		t.Fatalf("same-attempt generation reservation = %v", err)
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	newAttempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := state.ReportRecoveryAttestationForAttempt(attempt.ID)
	if err != nil || stored == nil || stored.ID != attestation.ID || stored.AttemptID != attempt.ID || stored.RunGeneration != attempt.RunGeneration {
		t.Fatalf("old attestation=%+v error=%v", stored, err)
	}
	if current, _ := state.Task(task.ID); current.CurrentAttemptID != newAttempt.ID {
		t.Fatalf("retry silently retargeted current attempt: %+v", current)
	}
	if err := state.GuardReportRecoveryContinuation(task.ID, newAttempt.ID); err != nil {
		t.Fatalf("clean retry attempt was frozen: %v", err)
	}
}

func TestReportRecoveryFencesOwnerGenerationCheckpointConflictsAndConcurrentRetry(t *testing.T) {
	state, task, attempt, checkpoint := reportRecoveryStoreFixture(t)
	defer state.Close()
	for name, test := range map[string]struct {
		owner                string
		generation, revision int
		cursor               int64
		kind                 string
	}{
		"owner":      {"driver:other", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor, "task_owner_mismatch"},
		"generation": {task.DriverID, attempt.RunGeneration + 1, checkpoint.Revision, checkpoint.SourceCursor, "stale_generation"},
		"revision":   {task.DriverID, attempt.RunGeneration, checkpoint.Revision + 1, checkpoint.SourceCursor, "checkpoint_not_current"},
		"cursor":     {task.DriverID, attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor + 1, "checkpoint_not_current"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := state.ReportRecoveryCandidateFor(task.ID, attempt.ID, test.owner, test.generation, test.revision, test.cursor)
			if err == nil || attestationErrorKind(err) != test.kind {
				t.Fatalf("error=%v kind=%q", err, attestationErrorKind(err))
			}
		})
	}
	candidate, err := state.ReportRecoveryCandidateFor(task.ID, attempt.ID, task.DriverID, attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.BeginAttempt(task.ID, "pi", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.RecordReportRecoveryAttestation(candidate, reportRecoveryStoreEvidence(task, attempt)); err == nil || attestationErrorKind(err) != "attempt_selection_changed" {
		t.Fatalf("retry race error = %v", err)
	}
}

func TestReportRecoveryRejectsAcceptedWorkerTerminal(t *testing.T) {
	state, task, attempt, checkpoint := reportRecoveryStoreFixture(t)
	defer state.Close()
	if _, err := state.db.Exec(`INSERT INTO messages(task_id, attempt_id, direction, type, payload, artifact_ref, stale, wake, source_cursor, run_generation, checkpoint_json, created_at)
		VALUES(?, ?, 'worker-to-driver', 'failed', 'accepted failure', '', 0, 1, ?, ?, '', ?)`, task.ID, attempt.ID, checkpoint.SourceCursor+2, attempt.RunGeneration, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ReportRecoveryCandidateFor(task.ID, attempt.ID, task.DriverID, attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor); err == nil || attestationErrorKind(err) != "terminal_event_conflict" {
		t.Fatalf("conflict error = %v", err)
	}
}

func reportRecoveryStoreFixture(t *testing.T) (*Store, model.Task, model.Attempt, model.AttemptCheckpoint) {
	t.Helper()
	state, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Recover report", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "report-recovery", Objective: "recover", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	worktreePath := t.TempDir()
	if err := state.BeginNativeAllocation(attempt.ID, worktreePath); err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureNativeWorkspace(attempt.ID, model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: "session",
		Path: worktreePath, GitDir: worktreePath + "/git-dir", CommonDir: worktreePath + "/common-dir", Branch: "shephrd/" + task.ID}); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"write report"}, model.WorkspaceFacts{HeadCommit: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	checkpointBody := model.Checkpoint{SchemaVersion: 1, Summary: "report complete", Completed: []string{"canonical report written"}, NextSteps: []string{"emit done"}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "report complete", Checkpoint: &checkpointBody}, 1,
		model.WorkspaceFacts{HeadCommit: strings.Repeat("b", 40)}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordControlFailureMessage(attempt.ID, generation, "terminal handoff failed"); err != nil {
		t.Fatal(err)
	}
	task = mustTask(t, state, task.ID)
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := state.LatestCheckpoint(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return state, task, attempt, checkpoint
}

func reportRecoveryStoreEvidence(task model.Task, attempt model.Attempt) model.ReportRecoveryAttestation {
	return model.ReportRecoveryAttestation{CanonicalReportPath: "/data/" + task.ID + "/report.md", FileIdentity: "1:2", FileMode: 0o100600,
		FileModTimeUnixNano: 42, SHA256: strings.Repeat("c", 64), SizeBytes: 7, Reason: "terminal handoff failed",
		ValidationKind: "canonical-regular-stable-sha256+file-identity-v1", EvidenceValidatedAt: time.Now().UTC()}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
