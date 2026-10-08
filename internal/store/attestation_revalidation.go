package store

import (
	"database/sql"

	"shephrd/internal/model"
)

type attestationAttemptState struct {
	generation int
	branch     string
	landed     bool
}

func revalidateAttemptIdentityTx(tx *sql.Tx, task model.Task, attempt model.Attempt, checkpoint model.AttemptCheckpoint, landingKind, phase string) (attestationAttemptState, error) {
	var taskID, status, branch, session, existingLandingKind, landedSource string
	var generation, runnerPID, landed int
	if err := tx.QueryRow(`SELECT task_id, status, branch, session_id, run_generation, runner_pid, landed_proven, landing_kind, landed_source_commit FROM attempts WHERE id=?`, attempt.ID).
		Scan(&taskID, &status, &branch, &session, &generation, &runnerPID, &landed, &existingLandingKind, &landedSource); err != nil {
		return attestationAttemptState{}, err
	}
	if taskID != task.ID || (status != model.AttemptStatusDone && status != model.AttemptStatusSuperseded) || runnerPID != 0 || generation != attempt.RunGeneration || branch != attempt.Branch || session != attempt.SessionID {
		return attestationAttemptState{}, model.Failure("attempt_selection_changed", "attempt %s identity or lifecycle changed during %s", attempt.ID, phase)
	}
	if landed != 0 && (existingLandingKind != landingKind || landedSource != checkpoint.HeadCommit) {
		return attestationAttemptState{}, model.Failure("landing_proof_conflict", "attempt %s acquired conflicting immutable %s landing proof", attempt.ID, existingLandingKind)
	}
	return attestationAttemptState{generation: generation, branch: branch, landed: landed != 0}, nil
}

func revalidateCheckpointIdentityTx(tx *sql.Tx, attempt model.Attempt, checkpoint model.AttemptCheckpoint, phase string) error {
	var producer, session, branch, head, workspaceError string
	var generation, revision, dirty int
	var cursor int64
	if err := tx.QueryRow(`SELECT producer, run_generation, revision, source_cursor, session_id, branch, head_commit, worktree_dirty, workspace_facts_error FROM attempt_checkpoints WHERE attempt_id=?`, attempt.ID).
		Scan(&producer, &generation, &revision, &cursor, &session, &branch, &head, &dirty, &workspaceError); err != nil {
		return model.Failure("attempt_selection_changed", "attempt %s checkpoint disappeared during %s", attempt.ID, phase)
	}
	if producer != "worker" || generation != checkpoint.RunGeneration || revision != checkpoint.Revision || cursor != checkpoint.SourceCursor || session != checkpoint.SessionID || branch != checkpoint.Branch || head != checkpoint.HeadCommit || dirty != 0 || workspaceError != "" {
		return model.Failure("attempt_selection_changed", "attempt %s checkpoint identity changed during %s", attempt.ID, phase)
	}
	return nil
}
