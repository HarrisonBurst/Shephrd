package store

import (
	"database/sql"
	"errors"

	"shephrd/internal/model"
)

func (s *Store) LocalDeliveryRecoveryForAttempt(attemptID string) (*model.LocalDeliveryRecovery, error) {
	recovery, err := scanLocalDeliveryRecovery(s.db.QueryRow(localDeliveryRecoverySelect+` WHERE attempt_id=?`, attemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &recovery, nil
}

func (s *Store) LocalDeliveryRecoveries(taskID string) ([]model.LocalDeliveryRecovery, error) {
	rows, err := s.db.Query(localDeliveryRecoverySelect+` WHERE task_id=? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.LocalDeliveryRecovery, 0)
	for rows.Next() {
		recovery, err := scanLocalDeliveryRecovery(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, recovery)
	}
	return result, rows.Err()
}

const localDeliveryRecoverySelect = `SELECT id, schema_version, task_id, attempt_id, repo_id, run_generation, done_message_id,
	checkpoint_revision, original_artifact_ref, attempt_branch, sealed_commit, registered_common_git_dir,
	registered_default_branch, default_head_at_validation, ancestry_validation, attested_by_driver_id,
	evidence_validated_at, created_at
	FROM local_delivery_recoveries`

func scanLocalDeliveryRecovery(scanner interface{ Scan(...any) error }) (model.LocalDeliveryRecovery, error) {
	var recovery model.LocalDeliveryRecovery
	var validated, created string
	if err := scanner.Scan(&recovery.ID, &recovery.SchemaVersion, &recovery.TaskID, &recovery.AttemptID, &recovery.RepoID,
		&recovery.RunGeneration, &recovery.DoneMessageID, &recovery.CheckpointRevision, &recovery.OriginalArtifactRef,
		&recovery.AttemptBranch, &recovery.SealedCommit, &recovery.RegisteredCommonGitDir, &recovery.RegisteredDefaultBranch,
		&recovery.DefaultHeadAtValidation, &recovery.AncestryValidation, &recovery.AttestedByDriverID, &validated, &created); err != nil {
		return model.LocalDeliveryRecovery{}, err
	}
	recovery.EvidenceValidatedAt = parseTime(validated)
	recovery.CreatedAt = parseTime(created)
	return recovery, nil
}
