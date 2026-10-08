package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"shephrd/internal/model"
)

var fullGitOID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var evidenceDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Store) DeliveryAttestationCandidate(taskID, attemptID, driverID, sealedCommit string, explicitAttempt bool) (model.DeliveryAttestationCandidate, error) {
	task, err := s.Task(taskID)
	if err != nil {
		return model.DeliveryAttestationCandidate{}, err
	}
	if strings.TrimSpace(driverID) == "" {
		return model.DeliveryAttestationCandidate{}, model.Failure("driver_context_required", "driver identity is required for external delivery attestation")
	}
	if task.DriverID != driverID {
		return model.DeliveryAttestationCandidate{}, model.Failure("task_owner_mismatch", "task %s is owned by %s, not %s; adopt it explicitly before attesting delivery", task.ID, task.DriverID, driverID)
	}
	if task.Deliverable != "code" {
		return model.DeliveryAttestationCandidate{}, model.Failure("attempt_not_eligible", "task %s deliverable is %s, not code", task.ID, task.Deliverable)
	}
	if attemptID == "" {
		attemptID = task.CurrentAttemptID
	}
	if attemptID == "" {
		return model.DeliveryAttestationCandidate{}, model.Failure("attempt_not_eligible", "task %s has no attempt to attest", task.ID)
	}
	attempt, err := s.Attempt(attemptID)
	if err != nil {
		return model.DeliveryAttestationCandidate{}, err
	}
	if attempt.TaskID != task.ID {
		return model.DeliveryAttestationCandidate{}, model.Failure("attempt_not_eligible", "attempt %s does not belong to task %s", attempt.ID, task.ID)
	}
	if !explicitAttempt && task.CurrentAttemptID != attempt.ID {
		return model.DeliveryAttestationCandidate{}, model.Failure("attempt_selection_changed", "task %s current attempt changed from %s", task.ID, attempt.ID)
	}
	if attempt.Status != model.AttemptStatusDone && attempt.Status != model.AttemptStatusSuperseded {
		return model.DeliveryAttestationCandidate{}, model.Failure("attempt_not_eligible", "attempt %s is %s, not accepted done or superseded", attempt.ID, attempt.Status)
	}
	if attempt.RunnerPID != 0 {
		return model.DeliveryAttestationCandidate{}, model.Failure("attempt_not_eligible", "attempt %s still has worker process %d recorded; wait for finalization", attempt.ID, attempt.RunnerPID)
	}
	done, err := s.acceptedDoneForAttestation(attempt.ID, attempt.RunGeneration)
	if err != nil {
		return model.DeliveryAttestationCandidate{}, err
	}
	if done.ArtifactRef != "branch:"+attempt.Branch {
		return model.DeliveryAttestationCandidate{}, model.Failure("branch_artifact_required", "attempt %s accepted artifact is %q; external attestation requires branch:%s", attempt.ID, done.ArtifactRef, attempt.Branch)
	}
	if task.CurrentAttemptID == attempt.ID && task.ArtifactRef != done.ArtifactRef {
		return model.DeliveryAttestationCandidate{}, model.Failure("branch_artifact_required", "task %s current artifact %q does not match accepted artifact %q", task.ID, task.ArtifactRef, done.ArtifactRef)
	}
	checkpoint, err := s.LatestCheckpoint(attempt.ID)
	if err != nil {
		return model.DeliveryAttestationCandidate{}, model.Failure("checkpoint_not_clean", "attempt %s has no final checkpoint", attempt.ID)
	}
	if err := validateAttestationCheckpoint(attempt, done, checkpoint); err != nil {
		return model.DeliveryAttestationCandidate{}, err
	}
	if !fullGitOID.MatchString(sealedCommit) || sealedCommit != checkpoint.HeadCommit {
		return model.DeliveryAttestationCandidate{}, model.Failure("sealed_commit_mismatch", "--commit must be the full sealed checkpoint commit %s", checkpoint.HeadCommit)
	}
	if attempt.LandedProven && (attempt.LandingKind != "github_pr_attested_ancestry" || attempt.LandedSourceCommit != checkpoint.HeadCommit) {
		return model.DeliveryAttestationCandidate{}, model.Failure("landing_proof_conflict", "attempt %s already has immutable %s landing proof", attempt.ID, attempt.LandingKind)
	}
	repo, err := s.Repo(task.RepoID)
	if err != nil {
		return model.DeliveryAttestationCandidate{}, err
	}
	existing, err := s.ExternalDeliveryAttestationForAttempt(attempt.ID)
	if err != nil {
		return model.DeliveryAttestationCandidate{}, err
	}
	return model.DeliveryAttestationCandidate{Task: task, Repo: repo, Attempt: attempt, Done: done, Checkpoint: checkpoint, ExplicitAttempt: explicitAttempt, Existing: existing}, nil
}

func (s *Store) acceptedDoneForAttestation(attemptID string, generation int) (model.Message, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND run_generation=? AND direction='worker-to-driver' AND type='done' AND stale=0`, attemptID, generation).Scan(&count); err != nil {
		return model.Message{}, err
	}
	if count != 1 {
		return model.Message{}, model.Failure("accepted_done_missing", "attempt %s has %d accepted worker done events in run generation %d; exactly one is required", attemptID, count, generation)
	}
	message, err := scanMessage(s.db.QueryRow(`SELECT id, task_id, attempt_id, direction, type, payload, artifact_ref, stale, wake,
		source_cursor, run_generation, checkpoint_json, created_at FROM messages
		WHERE attempt_id=? AND run_generation=? AND direction='worker-to-driver' AND type='done' AND stale=0`, attemptID, generation))
	if err != nil {
		return model.Message{}, err
	}
	return message, nil
}

func validateAttestationCheckpoint(attempt model.Attempt, done model.Message, checkpoint model.AttemptCheckpoint) error {
	if checkpoint.Producer != "worker" || checkpoint.AttemptID != attempt.ID || checkpoint.RunGeneration != attempt.RunGeneration || checkpoint.SessionID != attempt.SessionID || checkpoint.Branch != attempt.Branch || checkpoint.SourceCursor >= done.SourceCursor {
		return model.Failure("checkpoint_not_clean", "attempt %s final checkpoint identity does not match its accepted done event", attempt.ID)
	}
	if checkpoint.WorktreeDirty {
		return model.Failure("checkpoint_not_clean", "attempt %s final worker checkpoint was dirty", attempt.ID)
	}
	if checkpoint.WorkspaceFactsError != "" {
		return model.Failure("checkpoint_not_clean", "attempt %s final worker checkpoint has a workspace facts error", attempt.ID)
	}
	if !fullGitOID.MatchString(checkpoint.HeadCommit) {
		return model.Failure("checkpoint_not_clean", "attempt %s final worker checkpoint does not contain a full lowercase Git object ID", attempt.ID)
	}
	return nil
}

func (s *Store) ExternalDeliveryAttestationForAttempt(attemptID string) (*model.ExternalDeliveryAttestation, error) {
	attestation, err := scanExternalDeliveryAttestation(s.db.QueryRow(externalDeliveryAttestationSelect+` WHERE attempt_id=?`, attemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attestation, nil
}

func (s *Store) ExternalDeliveryAttestations(taskID string) ([]model.ExternalDeliveryAttestation, error) {
	rows, err := s.db.Query(externalDeliveryAttestationSelect+` WHERE task_id=? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.ExternalDeliveryAttestation, 0)
	for rows.Next() {
		attestation, err := scanExternalDeliveryAttestation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, attestation)
	}
	return result, rows.Err()
}

func (s *Store) RecordExternalDeliveryAttestation(candidate model.DeliveryAttestationCandidate, evidence model.ExternalDeliveryAttestation) (model.ExternalDeliveryAttestation, bool, error) {
	if evidence.Provider != "github" || evidence.RemoteHost == "" || evidence.RemoteRepository == "" || evidence.PRNumber < 1 || evidence.PRNodeID == "" || evidence.PRURL == "" || evidence.PRBaseRef == "" || evidence.PRHeadRef == "" || !fullGitOID.MatchString(evidence.PRHeadCommit) || !fullGitOID.MatchString(evidence.MergeCommit) || evidence.MergedAt == "" || !fullGitOID.MatchString(evidence.DefaultHeadAtValidation) || evidence.GraphValidation == "" || !evidenceDigest.MatchString(evidence.EvidenceDigest) {
		return model.ExternalDeliveryAttestation{}, false, model.Failure("github_unavailable", "validated external delivery evidence is incomplete")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.ExternalDeliveryAttestation{}, false, err
	}
	defer tx.Rollback()
	var currentAttempt, owner, deliverable, taskArtifact, defaultBranch string
	if err := tx.QueryRow(`SELECT COALESCE(t.current_attempt_id,''), t.driver_id, t.deliverable, t.artifact_ref, r.default_branch
		FROM tasks t JOIN repos r ON r.id=t.repo_id WHERE t.id=? AND t.repo_id=?`, candidate.Task.ID, candidate.Repo.ID).
		Scan(&currentAttempt, &owner, &deliverable, &taskArtifact, &defaultBranch); err != nil {
		return model.ExternalDeliveryAttestation{}, false, err
	}
	if owner != candidate.Task.DriverID {
		return model.ExternalDeliveryAttestation{}, false, model.Failure("task_owner_mismatch", "task %s owner changed from %s to %s during attestation", candidate.Task.ID, candidate.Task.DriverID, owner)
	}
	if deliverable != "code" || defaultBranch != candidate.Repo.DefaultBranch {
		return model.ExternalDeliveryAttestation{}, false, model.Failure("attempt_selection_changed", "task %s repository or deliverable changed during attestation", candidate.Task.ID)
	}
	if !candidate.ExplicitAttempt && currentAttempt != candidate.Attempt.ID {
		return model.ExternalDeliveryAttestation{}, false, model.Failure("attempt_selection_changed", "task %s current attempt changed from %s to %s during attestation", candidate.Task.ID, candidate.Attempt.ID, currentAttempt)
	}
	attemptState, err := revalidateAttemptIdentityTx(tx, candidate.Task, candidate.Attempt, candidate.Checkpoint, "github_pr_attested_ancestry", "attestation")
	if err != nil {
		return model.ExternalDeliveryAttestation{}, false, err
	}
	var doneCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND run_generation=? AND direction='worker-to-driver' AND type='done' AND stale=0`, candidate.Attempt.ID, attemptState.generation).Scan(&doneCount); err != nil {
		return model.ExternalDeliveryAttestation{}, false, err
	}
	if doneCount != 1 {
		return model.ExternalDeliveryAttestation{}, false, model.Failure("accepted_done_missing", "attempt %s no longer has exactly one accepted worker done event", candidate.Attempt.ID)
	}
	var doneArtifact string
	if err := tx.QueryRow(`SELECT artifact_ref FROM messages WHERE id=? AND task_id=? AND attempt_id=? AND run_generation=? AND direction='worker-to-driver' AND type='done' AND stale=0`, candidate.Done.ID, candidate.Task.ID, candidate.Attempt.ID, attemptState.generation).Scan(&doneArtifact); err != nil {
		return model.ExternalDeliveryAttestation{}, false, model.Failure("attempt_selection_changed", "attempt %s accepted done identity changed during attestation", candidate.Attempt.ID)
	}
	if doneArtifact != candidate.Done.ArtifactRef || doneArtifact != "branch:"+attemptState.branch || (currentAttempt == candidate.Attempt.ID && taskArtifact != doneArtifact) {
		return model.ExternalDeliveryAttestation{}, false, model.Failure("branch_artifact_required", "attempt %s accepted branch artifact changed during attestation", candidate.Attempt.ID)
	}
	if err := revalidateCheckpointIdentityTx(tx, candidate.Attempt, candidate.Checkpoint, "attestation"); err != nil {
		return model.ExternalDeliveryAttestation{}, false, err
	}
	evidence.TaskID = candidate.Task.ID
	evidence.AttemptID = candidate.Attempt.ID
	evidence.RepoID = candidate.Repo.ID
	evidence.RunGeneration = candidate.Attempt.RunGeneration
	evidence.DoneMessageID = candidate.Done.ID
	evidence.CheckpointRevision = candidate.Checkpoint.Revision
	evidence.OriginalArtifactRef = candidate.Done.ArtifactRef
	evidence.SealedCommit = candidate.Checkpoint.HeadCommit
	evidence.RegisteredDefaultBranch = candidate.Repo.DefaultBranch
	evidence.AttestedByDriverID = candidate.Task.DriverID
	if existing, existingErr := scanExternalDeliveryAttestation(tx.QueryRow(externalDeliveryAttestationSelect+` WHERE attempt_id=?`, candidate.Attempt.ID)); existingErr == nil {
		if !sameAttestationRequest(existing, evidence) {
			return model.ExternalDeliveryAttestation{}, false, model.Failure("attestation_conflict", "attempt %s already has an immutable external delivery attestation for %s", candidate.Attempt.ID, existing.PRURL)
		}
		if err := tx.Commit(); err != nil {
			return model.ExternalDeliveryAttestation{}, false, err
		}
		return existing, true, nil
	} else if !errors.Is(existingErr, sql.ErrNoRows) {
		return model.ExternalDeliveryAttestation{}, false, existingErr
	}
	if attemptState.landed {
		return model.ExternalDeliveryAttestation{}, false, model.Failure("landing_proof_conflict", "attempt %s has landing proof but no matching external attestation", candidate.Attempt.ID)
	}
	if evidence.ID == "" {
		evidence.ID = NewID("attestation")
	}
	evidence.SchemaVersion = 1
	if evidence.EvidenceValidatedAt.IsZero() {
		evidence.EvidenceValidatedAt = time.Now().UTC()
	}
	if evidence.CreatedAt.IsZero() {
		evidence.CreatedAt = time.Now().UTC()
	}
	_, err = tx.Exec(`INSERT INTO external_delivery_attestations(id, schema_version, task_id, attempt_id, repo_id, run_generation,
		done_message_id, checkpoint_revision, original_artifact_ref, sealed_commit, provider, remote_host, remote_repository,
		pr_number, pr_node_id, pr_url, registered_default_branch, pr_base_ref, pr_head_ref, pr_head_commit, merge_commit,
		merged_at, default_head_at_validation, graph_validation, evidence_digest, attested_by_driver_id, evidence_validated_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, evidence.ID,
		evidence.SchemaVersion, evidence.TaskID, evidence.AttemptID, evidence.RepoID, evidence.RunGeneration, evidence.DoneMessageID,
		evidence.CheckpointRevision, evidence.OriginalArtifactRef, evidence.SealedCommit, evidence.Provider, evidence.RemoteHost,
		evidence.RemoteRepository, evidence.PRNumber, evidence.PRNodeID, evidence.PRURL, evidence.RegisteredDefaultBranch,
		evidence.PRBaseRef, evidence.PRHeadRef, evidence.PRHeadCommit, evidence.MergeCommit, evidence.MergedAt,
		evidence.DefaultHeadAtValidation, evidence.GraphValidation, evidence.EvidenceDigest, evidence.AttestedByDriverID, stamp(evidence.EvidenceValidatedAt), stamp(evidence.CreatedAt))
	if err != nil {
		return model.ExternalDeliveryAttestation{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.ExternalDeliveryAttestation{}, false, err
	}
	return evidence, false, nil
}

func sameAttestationRequest(left, right model.ExternalDeliveryAttestation) bool {
	return left.TaskID == right.TaskID && left.AttemptID == right.AttemptID && left.RepoID == right.RepoID && left.RunGeneration == right.RunGeneration && left.DoneMessageID == right.DoneMessageID && left.CheckpointRevision == right.CheckpointRevision && left.OriginalArtifactRef == right.OriginalArtifactRef && left.SealedCommit == right.SealedCommit && left.Provider == right.Provider && left.RemoteHost == right.RemoteHost && left.RemoteRepository == right.RemoteRepository && left.PRNumber == right.PRNumber && left.PRURL == right.PRURL && left.RegisteredDefaultBranch == right.RegisteredDefaultBranch
}

const externalDeliveryAttestationSelect = `SELECT id, schema_version, task_id, attempt_id, repo_id, run_generation, done_message_id,
	checkpoint_revision, original_artifact_ref, sealed_commit, provider, remote_host, remote_repository, pr_number, pr_node_id,
	pr_url, registered_default_branch, pr_base_ref, pr_head_ref, pr_head_commit, merge_commit, merged_at,
	default_head_at_validation, graph_validation, evidence_digest, attested_by_driver_id, evidence_validated_at, created_at
	FROM external_delivery_attestations`

func scanExternalDeliveryAttestation(scanner interface{ Scan(...any) error }) (model.ExternalDeliveryAttestation, error) {
	var attestation model.ExternalDeliveryAttestation
	var validated, created string
	if err := scanner.Scan(&attestation.ID, &attestation.SchemaVersion, &attestation.TaskID, &attestation.AttemptID,
		&attestation.RepoID, &attestation.RunGeneration, &attestation.DoneMessageID, &attestation.CheckpointRevision,
		&attestation.OriginalArtifactRef, &attestation.SealedCommit, &attestation.Provider, &attestation.RemoteHost,
		&attestation.RemoteRepository, &attestation.PRNumber, &attestation.PRNodeID, &attestation.PRURL,
		&attestation.RegisteredDefaultBranch, &attestation.PRBaseRef, &attestation.PRHeadRef, &attestation.PRHeadCommit,
		&attestation.MergeCommit, &attestation.MergedAt, &attestation.DefaultHeadAtValidation, &attestation.GraphValidation,
		&attestation.EvidenceDigest, &attestation.AttestedByDriverID, &validated, &created); err != nil {
		return model.ExternalDeliveryAttestation{}, err
	}
	attestation.EvidenceValidatedAt = parseTime(validated)
	attestation.CreatedAt = parseTime(created)
	return attestation, nil
}
