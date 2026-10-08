package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"shephrd/internal/model"
)

const MaxReportRecoveryReasonBytes = 1024

func (s *Store) GuardReportRecoveryContinuation(taskID, attemptID string) error {
	var driverID string
	err := s.db.QueryRow(`SELECT t.driver_id FROM report_recovery_attestations r JOIN tasks t ON t.id=r.task_id WHERE r.task_id=? AND r.attempt_id=?`, taskID, attemptID).Scan(&driverID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return model.ReportRecoveryContinuationFailure(taskID, attemptID, driverID)
}

func guardReportRecoveryContinuationTx(tx *sql.Tx, attemptID string) error {
	var taskID, driverID string
	err := tx.QueryRow(`SELECT r.task_id, t.driver_id FROM report_recovery_attestations r JOIN tasks t ON t.id=r.task_id WHERE r.attempt_id=?`, attemptID).Scan(&taskID, &driverID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return model.ReportRecoveryContinuationFailure(taskID, attemptID, driverID)
}

func (s *Store) ReportRecoveryCandidateFor(taskID, attemptID, driverID string, runGeneration, checkpointRevision int, checkpointCursor int64) (model.ReportRecoveryCandidate, error) {
	task, err := s.Task(taskID)
	if err != nil {
		return model.ReportRecoveryCandidate{}, err
	}
	if strings.TrimSpace(driverID) == "" {
		return model.ReportRecoveryCandidate{}, model.Failure("driver_context_required", "driver identity is required for report recovery")
	}
	if task.DriverID != driverID {
		return model.ReportRecoveryCandidate{}, model.Failure("task_owner_mismatch", "task %s is owned by %s, not %s; adopt it explicitly before recording report recovery", task.ID, task.DriverID, driverID)
	}
	if task.Deliverable != "report" {
		return model.ReportRecoveryCandidate{}, model.Failure("attempt_not_eligible", "task %s deliverable is %s, not report", task.ID, task.Deliverable)
	}
	if strings.TrimSpace(attemptID) == "" {
		return model.ReportRecoveryCandidate{}, model.Failure("attempt_not_eligible", "--attempt must explicitly name the current report attempt")
	}
	attempt, err := s.Attempt(attemptID)
	if err != nil {
		return model.ReportRecoveryCandidate{}, err
	}
	if attempt.TaskID != task.ID || task.CurrentAttemptID != attempt.ID {
		return model.ReportRecoveryCandidate{}, model.Failure("attempt_selection_changed", "attempt %s is not the exact current attempt for task %s", attempt.ID, task.ID)
	}
	checkpoint, err := s.LatestCheckpoint(attempt.ID)
	if err != nil {
		return model.ReportRecoveryCandidate{}, model.Failure("checkpoint_not_current", "attempt %s has no current final checkpoint", attempt.ID)
	}
	command := reportRecoveryCommand(task, attempt, checkpoint)
	failure := func(kind, format string, args ...any) error {
		return model.EvidenceFailure(kind, map[string]string{"recovery_command": command, "inspect_command": "shephrd task inspect " + task.ID + " --json"}, format, args...)
	}
	if runGeneration != attempt.RunGeneration || checkpoint.RunGeneration != attempt.RunGeneration {
		return model.ReportRecoveryCandidate{}, failure("stale_generation", "attempt %s run generation is %d, not explicitly confirmed generation %d", attempt.ID, attempt.RunGeneration, runGeneration)
	}
	if checkpointRevision != checkpoint.Revision || checkpointCursor != checkpoint.SourceCursor {
		return model.ReportRecoveryCandidate{}, failure("checkpoint_not_current", "attempt %s final checkpoint is revision %d at cursor %d", attempt.ID, checkpoint.Revision, checkpoint.SourceCursor)
	}
	existing, err := s.ReportRecoveryAttestationForAttempt(attempt.ID)
	if err != nil {
		return model.ReportRecoveryCandidate{}, err
	}
	if existing != nil && (existing.TaskID != task.ID || existing.AttemptID != attempt.ID || existing.RepoID != task.RepoID ||
		existing.RunGeneration != attempt.RunGeneration || existing.CheckpointRevision != checkpoint.Revision ||
		existing.CheckpointSourceCursor != checkpoint.SourceCursor || existing.CheckpointSessionID != checkpoint.SessionID ||
		existing.CheckpointBranch != checkpoint.Branch || existing.CheckpointHeadCommit != checkpoint.HeadCommit ||
		existing.CheckpointWorktreeDirty != checkpoint.WorktreeDirty || existing.CheckpointWorkspaceFactsError != checkpoint.WorkspaceFactsError || existing.WorkspaceBackend != attempt.WorkspaceBackend ||
		existing.WorktreePath != attempt.WorktreePath || existing.WorktreeGitDir != attempt.WorktreeGitDir ||
		existing.WorktreeCommonDir != attempt.WorktreeCommonDir) {
		return model.ReportRecoveryCandidate{}, failure("attestation_conflict", "current task, attempt, workspace, or checkpoint identity no longer matches immutable report recovery evidence")
	}
	recovered := existing != nil && task.Status == model.TaskStatusDone && task.CompletionProvenance == model.CompletionProvenanceReportRecovery && attempt.Status == model.AttemptStatusDone
	if !recovered && (task.Status != model.TaskStatusBlocked && task.Status != model.TaskStatusFailed || attempt.Status != model.AttemptStatusBlocked && attempt.Status != model.AttemptStatusFailed) {
		return model.ReportRecoveryCandidate{}, failure("attempt_not_eligible", "task %s and attempt %s must retain their blocked or failed protocol result", task.ID, attempt.ID)
	}
	if attempt.RunnerPID != 0 || task.ProcessAlive {
		return model.ReportRecoveryCandidate{}, failure("worker_process_alive", "attempt %s still records a live worker process", attempt.ID)
	}
	if attempt.WorkspaceBackend != model.WorkspaceBackendNative || attempt.WorkspaceState != model.WorkspaceStateHeld || attempt.ReleaseState != model.WorkspaceStateHeld || attempt.ReleasedAt != nil || attempt.WorktreePath == "" || attempt.WorktreeGitDir == "" || attempt.WorktreeCommonDir == "" || attempt.Branch == "" {
		if !recovered || attempt.ReleaseState != model.WorkspaceStateReleased {
			return model.ReportRecoveryCandidate{}, failure("workspace_identity_mismatch", "attempt %s does not retain the exact held native worktree identity", attempt.ID)
		}
	}
	if checkpoint.Producer != "worker" || checkpoint.AttemptID != attempt.ID || checkpoint.SessionID != attempt.SessionID || checkpoint.Branch != attempt.Branch || checkpoint.HeadCommit == "" || checkpoint.WorkspaceFactsError != "" {
		return model.ReportRecoveryCandidate{}, failure("checkpoint_not_current", "attempt %s final checkpoint does not bind the current session, branch, and workspace facts", attempt.ID)
	}
	var workerTerminal, systemRecovery, verifiedArtifact int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND run_generation=? AND direction='worker-to-driver' AND type IN ('question','done','blocked','failed') AND stale=0`, attempt.ID, attempt.RunGeneration).Scan(&workerTerminal); err != nil {
		return model.ReportRecoveryCandidate{}, err
	}
	if workerTerminal != 0 {
		return model.ReportRecoveryCandidate{}, failure("terminal_event_conflict", "attempt %s has an accepted worker terminal event", attempt.ID)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND run_generation=? AND direction='system' AND type='report-recovery' AND stale=0`, attempt.ID, attempt.RunGeneration).Scan(&systemRecovery); err != nil {
		return model.ReportRecoveryCandidate{}, err
	}
	if (!recovered && systemRecovery != 0) || (recovered && systemRecovery != 1) {
		return model.ReportRecoveryCandidate{}, failure("artifact_conflict", "attempt %s has incompatible report recovery completion provenance", attempt.ID)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM verified_artifacts WHERE producer_attempt_id=?`, attempt.ID).Scan(&verifiedArtifact); err != nil {
		return model.ReportRecoveryCandidate{}, err
	}
	if !recovered && (task.ClaimedDone || task.ArtifactRef != "" || task.Landed || task.CompletionProvenance != "" || attempt.LandedProven || verifiedArtifact != 0) {
		return model.ReportRecoveryCandidate{}, failure("artifact_conflict", "task %s or attempt %s already has conflicting terminal, artifact, or landing evidence", task.ID, attempt.ID)
	}
	if recovered && (task.ClaimedDone || task.ArtifactRef != "report:"+existing.CanonicalReportPath || !attempt.LandedProven || verifiedArtifact != 1) {
		return model.ReportRecoveryCandidate{}, failure("artifact_conflict", "task %s recovered report projection is incomplete or conflicting", task.ID)
	}
	if external, err := s.ExternalDeliveryAttestationForAttempt(attempt.ID); err != nil {
		return model.ReportRecoveryCandidate{}, err
	} else if external != nil {
		return model.ReportRecoveryCandidate{}, failure("attestation_conflict", "attempt %s already has external delivery evidence", attempt.ID)
	}
	if local, err := s.LocalDeliveryRecoveryForAttempt(attempt.ID); err != nil {
		return model.ReportRecoveryCandidate{}, err
	} else if local != nil {
		return model.ReportRecoveryCandidate{}, failure("attestation_conflict", "attempt %s already has local code recovery evidence", attempt.ID)
	}
	repo, err := s.Repo(task.RepoID)
	if err != nil {
		return model.ReportRecoveryCandidate{}, err
	}
	return model.ReportRecoveryCandidate{Task: task, Repo: repo, Attempt: attempt, Checkpoint: checkpoint, Existing: existing, RecoveryCommand: command}, nil
}

func (s *Store) ReportRecoveryCommand(taskID, driverID string) (string, error) {
	task, err := s.Task(taskID)
	if err != nil {
		return "", err
	}
	if task.CurrentAttemptID == "" {
		return "", model.Failure("attempt_not_eligible", "task %s has no current report attempt", task.ID)
	}
	attempt, err := s.Attempt(task.CurrentAttemptID)
	if err != nil {
		return "", err
	}
	checkpoint, err := s.LatestCheckpoint(attempt.ID)
	if err != nil {
		return "", err
	}
	candidate, err := s.ReportRecoveryCandidateFor(task.ID, attempt.ID, driverID, attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil {
		return "", err
	}
	return candidate.RecoveryCommand, nil
}

func reportRecoveryCommand(task model.Task, attempt model.Attempt, checkpoint model.AttemptCheckpoint) string {
	return fmt.Sprintf("shephrd task attest-report-recovery %s --attempt %s --run-generation %d --checkpoint-revision %d --checkpoint-cursor %d --reason %s --driver-id %s --json", task.ID, attempt.ID, attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor, model.ShellQuote("terminal report handoff failed after canonical report write"), model.ShellQuote(task.DriverID))
}

func ValidateReportRecoveryReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return model.Failure("recovery_reason_invalid", "--reason is required for report recovery")
	}
	if !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) || len([]byte(reason)) > MaxReportRecoveryReasonBytes {
		return model.Failure("recovery_reason_invalid", "--reason must be valid UTF-8 without NUL and at most %d bytes", MaxReportRecoveryReasonBytes)
	}
	return nil
}

func (s *Store) ReportRecoveryAttestationForAttempt(attemptID string) (*model.ReportRecoveryAttestation, error) {
	attestation, err := scanReportRecoveryAttestation(s.db.QueryRow(reportRecoveryAttestationSelect+` WHERE attempt_id=?`, attemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attestation, nil
}

func (s *Store) ReportRecoveryAttestations(taskID string) ([]model.ReportRecoveryAttestation, error) {
	rows, err := s.db.Query(reportRecoveryAttestationSelect+` WHERE task_id=? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.ReportRecoveryAttestation, 0)
	for rows.Next() {
		attestation, err := scanReportRecoveryAttestation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, attestation)
	}
	return result, rows.Err()
}

func (s *Store) RecordReportRecoveryAttestation(candidate model.ReportRecoveryCandidate, evidence model.ReportRecoveryAttestation) (model.ReportRecoveryAttestation, bool, error) {
	if err := ValidateReportRecoveryReason(evidence.Reason); err != nil {
		return model.ReportRecoveryAttestation{}, false, err
	}
	evidence.Reason = strings.TrimSpace(evidence.Reason)
	if evidence.CanonicalReportPath == "" || evidence.FileIdentity == "" || evidence.FileMode == 0 || len(evidence.SHA256) != 64 || evidence.SizeBytes < 0 || evidence.ValidationKind == "" {
		return model.ReportRecoveryAttestation{}, false, model.Failure("report_evidence_incomplete", "validated report recovery evidence is incomplete")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.ReportRecoveryAttestation{}, false, err
	}
	defer tx.Rollback()
	recovered := candidate.Existing != nil && candidate.Task.CompletionProvenance == model.CompletionProvenanceReportRecovery
	if err := revalidateReportRecoveryTx(tx, candidate, recovered); err != nil {
		return model.ReportRecoveryAttestation{}, false, err
	}
	evidence.TaskID = candidate.Task.ID
	evidence.AttemptID = candidate.Attempt.ID
	evidence.RepoID = candidate.Repo.ID
	evidence.RunGeneration = candidate.Attempt.RunGeneration
	evidence.CheckpointRevision = candidate.Checkpoint.Revision
	evidence.CheckpointSourceCursor = candidate.Checkpoint.SourceCursor
	evidence.CheckpointSessionID = candidate.Checkpoint.SessionID
	evidence.CheckpointBranch = candidate.Checkpoint.Branch
	evidence.CheckpointHeadCommit = candidate.Checkpoint.HeadCommit
	evidence.CheckpointWorktreeDirty = candidate.Checkpoint.WorktreeDirty
	evidence.CheckpointWorkspaceFactsError = candidate.Checkpoint.WorkspaceFactsError
	evidence.WorkspaceBackend = candidate.Attempt.WorkspaceBackend
	evidence.WorkspaceState = candidate.Attempt.WorkspaceState
	evidence.WorktreePath = candidate.Attempt.WorktreePath
	evidence.WorktreeGitDir = candidate.Attempt.WorktreeGitDir
	evidence.WorktreeCommonDir = candidate.Attempt.WorktreeCommonDir
	evidence.AttestedByDriverID = candidate.Task.DriverID
	if recovered {
		evidence.WorkspaceState = candidate.Existing.WorkspaceState
		evidence.AttestedByDriverID = candidate.Existing.AttestedByDriverID
	}
	if existing, existingErr := scanReportRecoveryAttestation(tx.QueryRow(reportRecoveryAttestationSelect+` WHERE attempt_id=?`, candidate.Attempt.ID)); existingErr == nil {
		if !sameReportRecoveryAttestation(existing, evidence) {
			return model.ReportRecoveryAttestation{}, false, model.Failure("attestation_conflict", "attempt %s already has incompatible immutable report recovery evidence", candidate.Attempt.ID)
		}
		if err := tx.Commit(); err != nil {
			return model.ReportRecoveryAttestation{}, false, err
		}
		return existing, true, nil
	} else if !errors.Is(existingErr, sql.ErrNoRows) {
		return model.ReportRecoveryAttestation{}, false, existingErr
	}
	if evidence.ID == "" {
		evidence.ID = NewID("report_recovery")
	}
	evidence.SchemaVersion = 1
	if evidence.EvidenceValidatedAt.IsZero() {
		evidence.EvidenceValidatedAt = time.Now().UTC()
	}
	if evidence.CreatedAt.IsZero() {
		evidence.CreatedAt = time.Now().UTC()
	}
	_, err = tx.Exec(`INSERT INTO report_recovery_attestations(id, schema_version, task_id, attempt_id, repo_id, run_generation,
		checkpoint_revision, checkpoint_source_cursor, checkpoint_session_id, checkpoint_branch, checkpoint_head_commit,
		checkpoint_worktree_dirty, checkpoint_workspace_facts_error, workspace_backend, workspace_state, worktree_path, worktree_git_dir, worktree_common_dir,
		canonical_report_path, file_identity, file_mode, file_mod_time_unix_nano, sha256, size_bytes, reason, validation_kind,
		attested_by_driver_id, evidence_validated_at, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		evidence.ID, evidence.SchemaVersion, evidence.TaskID, evidence.AttemptID, evidence.RepoID, evidence.RunGeneration,
		evidence.CheckpointRevision, evidence.CheckpointSourceCursor, evidence.CheckpointSessionID, evidence.CheckpointBranch,
		evidence.CheckpointHeadCommit, evidence.CheckpointWorktreeDirty, evidence.CheckpointWorkspaceFactsError, evidence.WorkspaceBackend, evidence.WorkspaceState,
		evidence.WorktreePath, evidence.WorktreeGitDir, evidence.WorktreeCommonDir, evidence.CanonicalReportPath, evidence.FileIdentity,
		evidence.FileMode, evidence.FileModTimeUnixNano, evidence.SHA256, evidence.SizeBytes, evidence.Reason,
		evidence.ValidationKind, evidence.AttestedByDriverID, stamp(evidence.EvidenceValidatedAt), stamp(evidence.CreatedAt))
	if err != nil {
		return model.ReportRecoveryAttestation{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.ReportRecoveryAttestation{}, false, err
	}
	return evidence, false, nil
}

func revalidateReportRecoveryTx(tx *sql.Tx, candidate model.ReportRecoveryCandidate, recovered bool) error {
	var current, owner, deliverable, taskStatus, taskArtifact, completion string
	var claimed, landed, processAlive int
	if err := tx.QueryRow(`SELECT COALESCE(current_attempt_id,''), driver_id, deliverable, status, artifact_ref, claimed_done, completion_provenance, process_alive,
		(SELECT landed FROM task_landing_projections WHERE task_id=tasks.id) FROM tasks WHERE id=?`, candidate.Task.ID).
		Scan(&current, &owner, &deliverable, &taskStatus, &taskArtifact, &claimed, &completion, &processAlive, &landed); err != nil {
		return err
	}
	if current != candidate.Attempt.ID || owner != candidate.Task.DriverID || deliverable != "report" || processAlive != 0 {
		return model.Failure("attempt_selection_changed", "task %s owner, attempt, deliverable, or process identity changed during report recovery", candidate.Task.ID)
	}
	if recovered {
		if taskStatus != model.TaskStatusDone || claimed != 0 || completion != model.CompletionProvenanceReportRecovery || taskArtifact == "" || landed == 0 {
			return model.Failure("attempt_selection_changed", "task %s recovered report projection changed during verification", candidate.Task.ID)
		}
	} else if taskStatus != model.TaskStatusBlocked && taskStatus != model.TaskStatusFailed || claimed != 0 || completion != "" || taskArtifact != "" || landed != 0 {
		return model.Failure("attempt_selection_changed", "task %s terminal or artifact state changed during report recovery", candidate.Task.ID)
	}
	var taskID, status, session, branch, workspaceBackend, workspaceState, worktreePath, gitDir, commonDir, releaseState string
	var generation, runnerPID, attemptLanded int
	var released sql.NullString
	if err := tx.QueryRow(`SELECT task_id, status, session_id, branch, run_generation, runner_pid, workspace_backend, workspace_state,
		worktree_path, worktree_git_dir, worktree_common_dir, release_state, released_at, landed_proven FROM attempts WHERE id=?`, candidate.Attempt.ID).
		Scan(&taskID, &status, &session, &branch, &generation, &runnerPID, &workspaceBackend, &workspaceState, &worktreePath,
			&gitDir, &commonDir, &releaseState, &released, &attemptLanded); err != nil {
		return err
	}
	if taskID != candidate.Task.ID || generation != candidate.Attempt.RunGeneration || runnerPID != 0 || session != candidate.Attempt.SessionID || branch != candidate.Attempt.Branch ||
		workspaceBackend != candidate.Attempt.WorkspaceBackend || worktreePath != candidate.Attempt.WorktreePath || gitDir != candidate.Attempt.WorktreeGitDir || commonDir != candidate.Attempt.WorktreeCommonDir {
		return model.Failure("attempt_selection_changed", "attempt %s identity changed during report recovery", candidate.Attempt.ID)
	}
	if recovered {
		if status != model.AttemptStatusDone || attemptLanded == 0 {
			return model.Failure("attempt_selection_changed", "attempt %s recovered completion changed during verification", candidate.Attempt.ID)
		}
	} else if status != model.AttemptStatusBlocked && status != model.AttemptStatusFailed || workspaceBackend != model.WorkspaceBackendNative || workspaceState != model.WorkspaceStateHeld || releaseState != model.WorkspaceStateHeld || released.Valid || attemptLanded != 0 || worktreePath != candidate.Attempt.WorktreePath || gitDir != candidate.Attempt.WorktreeGitDir || commonDir != candidate.Attempt.WorktreeCommonDir {
		return model.Failure("attempt_selection_changed", "attempt %s lifecycle or held workspace identity changed during report recovery", candidate.Attempt.ID)
	}
	if err := revalidateCheckpointIdentityTx(tx, candidate.Attempt, candidate.Checkpoint, "report recovery"); err != nil {
		return err
	}
	var terminals, recoveries int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND run_generation=? AND direction='worker-to-driver' AND type IN ('question','done','blocked','failed') AND stale=0`, candidate.Attempt.ID, generation).Scan(&terminals); err != nil {
		return err
	}
	if terminals != 0 {
		return model.Failure("terminal_event_conflict", "attempt %s acquired an accepted worker terminal event during report recovery", candidate.Attempt.ID)
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND run_generation=? AND direction='system' AND type='report-recovery' AND stale=0`, candidate.Attempt.ID, generation).Scan(&recoveries); err != nil {
		return err
	}
	if (!recovered && recoveries != 0) || (recovered && recoveries != 1) {
		return model.Failure("artifact_conflict", "attempt %s report recovery completion provenance changed", candidate.Attempt.ID)
	}
	return nil
}

func (s *Store) SetVerifiedRecoveredReportDelivery(candidate model.ReportRecoveryCandidate, attestation model.ReportRecoveryAttestation, artifact model.VerifiedArtifact, reason string, handlers ...model.ReportAcceptedHandlerBinding) (model.VerifiedArtifact, error) {
	if artifact.Kind != "report" || artifact.OriginalRef != "report:"+attestation.CanonicalReportPath || artifact.SHA256 != attestation.SHA256 || artifact.SizeBytes != attestation.SizeBytes || artifact.SnapshotPath == "" {
		return model.VerifiedArtifact{}, model.Failure("attestation_conflict", "report snapshot does not match immutable recovery %s", attestation.ID)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.VerifiedArtifact{}, err
	}
	defer tx.Rollback()
	var stored model.ReportRecoveryAttestation
	stored, err = scanReportRecoveryAttestation(tx.QueryRow(reportRecoveryAttestationSelect+` WHERE id=? AND attempt_id=?`, attestation.ID, candidate.Attempt.ID))
	if err != nil || !sameReportRecoveryAttestation(stored, attestation) {
		return model.VerifiedArtifact{}, model.Failure("attestation_conflict", "immutable report recovery %s changed or disappeared", attestation.ID)
	}
	var existing model.VerifiedArtifact
	existing, err = scanVerifiedArtifact(tx.QueryRow(s.verifiedArtifactSelect()+` WHERE report_recovery_id=?`, attestation.ID))
	if err == nil {
		if existing.ProducerTaskID != candidate.Task.ID || existing.ProducerAttemptID != candidate.Attempt.ID || existing.Kind != artifact.Kind || existing.OriginalRef != artifact.OriginalRef || existing.SHA256 != artifact.SHA256 || existing.SizeBytes != artifact.SizeBytes || existing.SnapshotPath != artifact.SnapshotPath {
			return model.VerifiedArtifact{}, model.Failure("artifact_conflict", "recovered report snapshot metadata conflicts with immutable artifact %s", existing.ID)
		}
		if err := revalidateReportRecoveryTx(tx, candidate, true); err != nil {
			return model.VerifiedArtifact{}, err
		}
		if err := tx.Commit(); err != nil {
			return model.VerifiedArtifact{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.VerifiedArtifact{}, err
	}
	if err := revalidateReportRecoveryTx(tx, candidate, false); err != nil {
		return model.VerifiedArtifact{}, err
	}
	message, err := insertMessage(tx, candidate.Task.ID, candidate.Attempt.ID, "system", "report-recovery",
		"driver-attested report recovery verified: "+attestation.ID, artifact.OriginalRef, false, false, 0, candidate.Attempt.RunGeneration, "")
	if err != nil {
		return model.VerifiedArtifact{}, err
	}
	if artifact.ID == "" {
		artifact.ID = NewID("artifact")
	}
	artifact.ProducerTaskID = candidate.Task.ID
	artifact.ProducerAttemptID = candidate.Attempt.ID
	artifact.DoneMessageID = message.ID
	artifact.ReportRecoveryID = attestation.ID
	if artifact.VerifiedAt.IsZero() {
		artifact.VerifiedAt = time.Now().UTC()
	}
	prepareReportAcceptedArtifact(&artifact)
	if _, err := tx.Exec(`INSERT INTO verified_artifacts(id, producer_task_id, producer_attempt_id, done_message_id, report_recovery_id,
		kind, original_ref, sha256, size_bytes, snapshot_path, verified_at, accepted_event_id, accepted_event_name, accepted_event_version)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, artifact.ID, artifact.ProducerTaskID, artifact.ProducerAttemptID,
		artifact.DoneMessageID, artifact.ReportRecoveryID, artifact.Kind, artifact.OriginalRef, artifact.SHA256, artifact.SizeBytes,
		artifact.SnapshotPath, stamp(artifact.VerifiedAt), artifact.AcceptedEventID, artifact.AcceptedEventName, artifact.AcceptedEventVersion); err != nil {
		return model.VerifiedArtifact{}, err
	}
	if err := insertReportLifecycleInvocationsTx(tx, artifact, handlers); err != nil {
		return model.VerifiedArtifact{}, err
	}
	if _, err := tx.Exec(`UPDATE attempts SET status=?, landed_proven=1, landing_kind='report_artifact', landed_checkpoint_revision=?,
		landed_verified_at=?, landing_reason=?, updated_at=? WHERE id=? AND task_id=? AND run_generation=?`, model.AttemptStatusDone,
		attestation.CheckpointRevision, stamp(artifact.VerifiedAt), reason, now(), candidate.Attempt.ID, candidate.Task.ID, candidate.Attempt.RunGeneration); err != nil {
		return model.VerifiedArtifact{}, err
	}
	if _, err := tx.Exec(`UPDATE tasks SET status=?, artifact_ref=?, claimed_done=0, completion_provenance=?, branch_pushed=0,
		remote_delivery_state='unverified', pr_state='', updated_at=? WHERE id=? AND current_attempt_id=?`, model.TaskStatusDone,
		artifact.OriginalRef, model.CompletionProvenanceReportRecovery, now(), candidate.Task.ID, candidate.Attempt.ID); err != nil {
		return model.VerifiedArtifact{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.VerifiedArtifact{}, err
	}
	return artifact, nil
}

func sameReportRecoveryAttestation(left, right model.ReportRecoveryAttestation) bool {
	return left.TaskID == right.TaskID && left.AttemptID == right.AttemptID && left.RepoID == right.RepoID &&
		left.RunGeneration == right.RunGeneration && left.CheckpointRevision == right.CheckpointRevision &&
		left.CheckpointSourceCursor == right.CheckpointSourceCursor && left.CheckpointSessionID == right.CheckpointSessionID &&
		left.CheckpointBranch == right.CheckpointBranch && left.CheckpointHeadCommit == right.CheckpointHeadCommit &&
		left.CheckpointWorktreeDirty == right.CheckpointWorktreeDirty && left.CheckpointWorkspaceFactsError == right.CheckpointWorkspaceFactsError && left.WorkspaceBackend == right.WorkspaceBackend &&
		left.WorkspaceState == right.WorkspaceState && left.WorktreePath == right.WorktreePath && left.WorktreeGitDir == right.WorktreeGitDir &&
		left.WorktreeCommonDir == right.WorktreeCommonDir && left.CanonicalReportPath == right.CanonicalReportPath &&
		left.FileIdentity == right.FileIdentity && left.FileMode == right.FileMode && left.FileModTimeUnixNano == right.FileModTimeUnixNano &&
		left.SHA256 == right.SHA256 && left.SizeBytes == right.SizeBytes && left.Reason == right.Reason &&
		left.ValidationKind == right.ValidationKind && left.AttestedByDriverID == right.AttestedByDriverID
}

const reportRecoveryAttestationSelect = `SELECT id, schema_version, task_id, attempt_id, repo_id, run_generation,
	checkpoint_revision, checkpoint_source_cursor, checkpoint_session_id, checkpoint_branch, checkpoint_head_commit,
	checkpoint_worktree_dirty, checkpoint_workspace_facts_error, workspace_backend, workspace_state, worktree_path, worktree_git_dir, worktree_common_dir,
	canonical_report_path, file_identity, file_mode, file_mod_time_unix_nano, sha256, size_bytes, reason, validation_kind,
	attested_by_driver_id, evidence_validated_at, created_at FROM report_recovery_attestations`

func scanReportRecoveryAttestation(scanner interface{ Scan(...any) error }) (model.ReportRecoveryAttestation, error) {
	var attestation model.ReportRecoveryAttestation
	var dirty int
	var validated, created string
	if err := scanner.Scan(&attestation.ID, &attestation.SchemaVersion, &attestation.TaskID, &attestation.AttemptID,
		&attestation.RepoID, &attestation.RunGeneration, &attestation.CheckpointRevision, &attestation.CheckpointSourceCursor,
		&attestation.CheckpointSessionID, &attestation.CheckpointBranch, &attestation.CheckpointHeadCommit, &dirty, &attestation.CheckpointWorkspaceFactsError,
		&attestation.WorkspaceBackend, &attestation.WorkspaceState, &attestation.WorktreePath, &attestation.WorktreeGitDir,
		&attestation.WorktreeCommonDir, &attestation.CanonicalReportPath, &attestation.FileIdentity, &attestation.FileMode,
		&attestation.FileModTimeUnixNano, &attestation.SHA256, &attestation.SizeBytes, &attestation.Reason,
		&attestation.ValidationKind, &attestation.AttestedByDriverID, &validated, &created); err != nil {
		return model.ReportRecoveryAttestation{}, err
	}
	attestation.CheckpointWorktreeDirty = dirty != 0
	attestation.EvidenceValidatedAt = parseTime(validated)
	attestation.CreatedAt = parseTime(created)
	return attestation, nil
}
