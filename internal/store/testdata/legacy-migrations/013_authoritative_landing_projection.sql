ALTER TABLE attempts ADD COLUMN landing_reason TEXT NOT NULL DEFAULT '';

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
                        WHERE va.producer_task_id = a.task_id
                          AND va.producer_attempt_id = a.id
                          AND va.kind = 'report'
                          AND m.task_id = a.task_id
                          AND m.attempt_id = a.id
                          AND m.run_generation = a.run_generation
                          AND m.direction = 'worker-to-driver'
                          AND m.type = 'done'
                          AND m.stale = 0
                          AND m.artifact_ref = va.original_ref
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

UPDATE attempts
SET landing_reason = COALESCE((
    SELECT t.landed_reason
    FROM tasks t
    WHERE t.id = attempts.task_id AND t.current_attempt_id = attempts.id
), '')
WHERE landing_reason = ''
  AND EXISTS (
      SELECT 1
      FROM attempt_landing_projections p
      WHERE p.attempt_id = attempts.id AND p.landed = 1
  );

CREATE VIEW task_landing_projections AS
SELECT
    t.id AS task_id,
    COALESCE(p.landed, 0) AS landed,
    CASE WHEN COALESCE(p.landed, 0) = 1 THEN p.landed_reason ELSE '' END AS landed_reason
FROM tasks t
LEFT JOIN attempt_landing_projections p
    ON p.attempt_id = t.current_attempt_id AND p.task_id = t.id;

UPDATE tasks
SET landed = (SELECT p.landed FROM task_landing_projections p WHERE p.task_id = tasks.id),
    landed_reason = (SELECT p.landed_reason FROM task_landing_projections p WHERE p.task_id = tasks.id);

CREATE TABLE migration_013_landing_equality (
    mismatches INTEGER NOT NULL CHECK(mismatches = 0)
);

INSERT INTO migration_013_landing_equality(mismatches)
SELECT COUNT(*)
FROM tasks t
JOIN task_landing_projections p ON p.task_id = t.id
WHERE t.landed <> p.landed OR t.landed_reason <> p.landed_reason;

DROP TABLE migration_013_landing_equality;
