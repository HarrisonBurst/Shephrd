CREATE TABLE report_recovery_attestations (
    id TEXT PRIMARY KEY,
    schema_version INTEGER NOT NULL CHECK(schema_version = 1),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    attempt_id TEXT NOT NULL UNIQUE REFERENCES attempts(id),
    repo_id TEXT NOT NULL REFERENCES repos(id),
    run_generation INTEGER NOT NULL,
    checkpoint_revision INTEGER NOT NULL,
    checkpoint_source_cursor INTEGER NOT NULL,
    checkpoint_session_id TEXT NOT NULL,
    checkpoint_branch TEXT NOT NULL,
    checkpoint_head_commit TEXT NOT NULL,
    checkpoint_worktree_dirty INTEGER NOT NULL CHECK(checkpoint_worktree_dirty IN (0, 1)),
    checkpoint_workspace_facts_error TEXT NOT NULL CHECK(checkpoint_workspace_facts_error = ''),
    workspace_backend TEXT NOT NULL CHECK(workspace_backend = 'native_git_worktree'),
    workspace_state TEXT NOT NULL CHECK(workspace_state = 'held'),
    worktree_path TEXT NOT NULL,
    worktree_git_dir TEXT NOT NULL,
    worktree_common_dir TEXT NOT NULL,
    canonical_report_path TEXT NOT NULL,
    file_identity TEXT NOT NULL,
    file_mode INTEGER NOT NULL,
    file_mod_time_unix_nano INTEGER NOT NULL,
    sha256 TEXT NOT NULL CHECK(length(sha256) = 64 AND sha256 = lower(sha256)),
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
    reason TEXT NOT NULL CHECK(reason <> ''),
    validation_kind TEXT NOT NULL,
    attested_by_driver_id TEXT NOT NULL CHECK(attested_by_driver_id <> ''),
    evidence_validated_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(task_id, attempt_id, run_generation, checkpoint_revision, checkpoint_source_cursor)
);

CREATE TRIGGER report_recovery_attestations_immutable_update
BEFORE UPDATE ON report_recovery_attestations
BEGIN
    SELECT RAISE(ABORT, 'report recovery attestations are immutable');
END;

CREATE TRIGGER report_recovery_attestations_immutable_delete
BEFORE DELETE ON report_recovery_attestations
BEGIN
    SELECT RAISE(ABORT, 'report recovery attestations are immutable');
END;

CREATE INDEX report_recovery_attestations_task_idx ON report_recovery_attestations(task_id, attempt_id);

ALTER TABLE tasks ADD COLUMN completion_provenance TEXT NOT NULL DEFAULT ''
    CHECK(completion_provenance IN ('', 'worker_done', 'driver_report_recovery'));
UPDATE tasks SET completion_provenance='worker_done' WHERE claimed_done=1;

ALTER TABLE verified_artifacts ADD COLUMN report_recovery_id TEXT REFERENCES report_recovery_attestations(id);
CREATE UNIQUE INDEX verified_artifacts_report_recovery_idx ON verified_artifacts(report_recovery_id) WHERE report_recovery_id IS NOT NULL;

DROP VIEW task_landing_projections;
DROP VIEW attempt_landing_projections;

CREATE VIEW attempt_landing_projections AS
SELECT
    proof.attempt_id,
    proof.task_id,
    proof.landed,
    CASE WHEN proof.landed = 1 THEN proof.landing_reason ELSE '' END AS landed_reason
FROM (
    SELECT
        a.id AS attempt_id,
        a.task_id AS task_id,
        a.landing_reason,
        CASE WHEN
            a.landed_proven = 1
            AND a.landing_kind <> ''
            AND a.landed_verified_at IS NOT NULL
            AND (
                (
                    a.landing_kind = 'report_artifact'
                    AND EXISTS (
                        SELECT 1
                        FROM verified_artifacts va
                        JOIN messages m ON m.id = va.done_message_id
                        LEFT JOIN report_recovery_attestations rr ON rr.id = va.report_recovery_id
                        WHERE va.producer_task_id = a.task_id
                          AND va.producer_attempt_id = a.id
                          AND va.kind = 'report'
                          AND m.task_id = a.task_id
                          AND m.attempt_id = a.id
                          AND m.run_generation = a.run_generation
                          AND m.stale = 0
                          AND m.artifact_ref = va.original_ref
                          AND (
                              (
                                  va.report_recovery_id IS NULL
                                  AND m.direction = 'worker-to-driver'
                                  AND m.type = 'done'
                              )
                              OR (
                                  va.report_recovery_id IS NOT NULL
                                  AND m.direction = 'system'
                                  AND m.type = 'report-recovery'
                                  AND rr.task_id = a.task_id
                                  AND rr.attempt_id = a.id
                                  AND rr.run_generation = a.run_generation
                                  AND rr.checkpoint_revision = a.landed_checkpoint_revision
                                  AND rr.sha256 = va.sha256
                                  AND rr.size_bytes = va.size_bytes
                                  AND 'report:' || rr.canonical_report_path = va.original_ref
                              )
                          )
                    )
                )
                OR (
                    a.landing_kind IN ('local_default_branch', 'github_pr')
                    AND a.landed_source_commit <> ''
                    AND a.landed_target_ref <> ''
                    AND a.landed_target_commit <> ''
                    AND a.landed_checkpoint_revision > 0
                )
                OR (
                    a.landing_kind = 'github_pr_attested_ancestry'
                    AND a.landed_source_commit <> ''
                    AND a.landed_target_ref <> ''
                    AND a.landed_target_commit <> ''
                    AND a.landed_checkpoint_revision > 0
                    AND EXISTS (
                        SELECT 1
                        FROM external_delivery_attestations e
                        WHERE e.task_id = a.task_id
                          AND e.attempt_id = a.id
                          AND e.run_generation = a.run_generation
                          AND e.sealed_commit = a.landed_source_commit
                          AND 'refs/heads/' || e.registered_default_branch = a.landed_target_ref
                          AND e.merge_commit = a.landed_target_commit
                          AND e.checkpoint_revision = a.landed_checkpoint_revision
                    )
                )
                OR (
                    a.landing_kind = 'local_default_branch_attested_ancestry'
                    AND a.landed_source_commit <> ''
                    AND a.landed_target_ref <> ''
                    AND a.landed_target_commit <> ''
                    AND a.landed_checkpoint_revision > 0
                    AND EXISTS (
                        SELECT 1
                        FROM local_delivery_recoveries l
                        WHERE l.task_id = a.task_id
                          AND l.attempt_id = a.id
                          AND l.run_generation = a.run_generation
                          AND l.sealed_commit = a.landed_source_commit
                          AND 'refs/heads/' || l.registered_default_branch = a.landed_target_ref
                          AND l.checkpoint_revision = a.landed_checkpoint_revision
                    )
                )
            )
        THEN 1 ELSE 0 END AS landed
    FROM attempts a
) proof;

CREATE VIEW task_landing_projections AS
SELECT
    t.id AS task_id,
    COALESCE(p.landed, 0) AS landed,
    CASE WHEN COALESCE(p.landed, 0) = 1 THEN p.landed_reason ELSE '' END AS landed_reason
FROM tasks t
LEFT JOIN attempt_landing_projections p
    ON p.attempt_id = t.current_attempt_id AND p.task_id = t.id;
