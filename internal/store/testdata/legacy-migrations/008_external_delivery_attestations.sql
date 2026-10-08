CREATE TABLE external_delivery_attestations (
    id TEXT PRIMARY KEY,
    schema_version INTEGER NOT NULL CHECK(schema_version = 1),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    attempt_id TEXT NOT NULL UNIQUE REFERENCES attempts(id),
    repo_id TEXT NOT NULL REFERENCES repos(id),
    run_generation INTEGER NOT NULL,
    done_message_id INTEGER NOT NULL REFERENCES messages(id),
    checkpoint_revision INTEGER NOT NULL,
    original_artifact_ref TEXT NOT NULL,
    sealed_commit TEXT NOT NULL CHECK(length(sealed_commit) IN (40, 64) AND sealed_commit = lower(sealed_commit)),
    provider TEXT NOT NULL CHECK(provider = 'github'),
    remote_host TEXT NOT NULL,
    remote_repository TEXT NOT NULL,
    pr_number INTEGER NOT NULL CHECK(pr_number > 0),
    pr_node_id TEXT NOT NULL,
    pr_url TEXT NOT NULL,
    registered_default_branch TEXT NOT NULL,
    pr_base_ref TEXT NOT NULL,
    pr_head_ref TEXT NOT NULL,
    pr_head_commit TEXT NOT NULL,
    merge_commit TEXT NOT NULL,
    merged_at TEXT NOT NULL,
    default_head_at_validation TEXT NOT NULL,
    graph_validation TEXT NOT NULL,
    attested_by_driver_id TEXT NOT NULL,
    evidence_validated_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(task_id, attempt_id, run_generation, done_message_id, checkpoint_revision)
);

CREATE TRIGGER external_delivery_attestations_immutable_update
BEFORE UPDATE ON external_delivery_attestations
BEGIN
    SELECT RAISE(ABORT, 'external delivery attestations are immutable');
END;

CREATE TRIGGER external_delivery_attestations_immutable_delete
BEFORE DELETE ON external_delivery_attestations
BEGIN
    SELECT RAISE(ABORT, 'external delivery attestations are immutable');
END;

CREATE INDEX external_delivery_attestations_task_idx ON external_delivery_attestations(task_id, attempt_id);
CREATE INDEX external_delivery_attestations_pr_idx ON external_delivery_attestations(remote_host, remote_repository, pr_number);
