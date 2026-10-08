CREATE TABLE local_delivery_recoveries (
    id TEXT PRIMARY KEY,
    schema_version INTEGER NOT NULL CHECK(schema_version = 1),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    attempt_id TEXT NOT NULL UNIQUE REFERENCES attempts(id),
    repo_id TEXT NOT NULL REFERENCES repos(id),
    run_generation INTEGER NOT NULL,
    done_message_id INTEGER NOT NULL REFERENCES messages(id),
    checkpoint_revision INTEGER NOT NULL,
    original_artifact_ref TEXT NOT NULL CHECK(original_artifact_ref LIKE 'branch:%'),
    attempt_branch TEXT NOT NULL CHECK(attempt_branch <> '' AND original_artifact_ref <> 'branch:' || attempt_branch),
    sealed_commit TEXT NOT NULL CHECK(length(sealed_commit) IN (40, 64) AND sealed_commit = lower(sealed_commit)),
    registered_common_git_dir TEXT NOT NULL CHECK(registered_common_git_dir <> ''),
    registered_default_branch TEXT NOT NULL CHECK(registered_default_branch <> ''),
    default_head_at_validation TEXT NOT NULL CHECK(length(default_head_at_validation) IN (40, 64) AND default_head_at_validation = lower(default_head_at_validation)),
    ancestry_validation TEXT NOT NULL CHECK(ancestry_validation <> ''),
    attested_by_driver_id TEXT NOT NULL CHECK(attested_by_driver_id <> ''),
    evidence_validated_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(task_id, attempt_id, run_generation, done_message_id, checkpoint_revision)
);

CREATE TRIGGER local_delivery_recoveries_immutable_update
BEFORE UPDATE ON local_delivery_recoveries
BEGIN
    SELECT RAISE(ABORT, 'local delivery recoveries are immutable');
END;

CREATE TRIGGER local_delivery_recoveries_immutable_delete
BEFORE DELETE ON local_delivery_recoveries
BEGIN
    SELECT RAISE(ABORT, 'local delivery recoveries are immutable');
END;

CREATE INDEX local_delivery_recoveries_task_idx ON local_delivery_recoveries(task_id, attempt_id);
