CREATE TABLE verified_artifacts (
    id TEXT PRIMARY KEY,
    producer_task_id TEXT NOT NULL REFERENCES tasks(id),
    producer_attempt_id TEXT NOT NULL REFERENCES attempts(id),
    done_message_id INTEGER NOT NULL UNIQUE REFERENCES messages(id),
    kind TEXT NOT NULL CHECK(kind = 'report'),
    original_ref TEXT NOT NULL,
    sha256 TEXT NOT NULL CHECK(length(sha256) = 64 AND sha256 = lower(sha256)),
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
    snapshot_path TEXT NOT NULL,
    verified_at TEXT NOT NULL
);

CREATE TABLE task_report_inputs (
    target_task_id TEXT PRIMARY KEY REFERENCES tasks(id),
    artifact_id TEXT NOT NULL REFERENCES verified_artifacts(id),
    attached_by_driver_id TEXT NOT NULL,
    attached_at TEXT NOT NULL
);

CREATE TRIGGER verified_artifacts_immutable_update
BEFORE UPDATE ON verified_artifacts
BEGIN
    SELECT RAISE(ABORT, 'verified artifacts are immutable');
END;

CREATE TRIGGER verified_artifacts_immutable_delete
BEFORE DELETE ON verified_artifacts
BEGIN
    SELECT RAISE(ABORT, 'verified artifacts are immutable');
END;

CREATE TRIGGER task_report_inputs_immutable_update
BEFORE UPDATE ON task_report_inputs
BEGIN
    SELECT RAISE(ABORT, 'task report inputs are immutable');
END;

CREATE TRIGGER task_report_inputs_immutable_delete
BEFORE DELETE ON task_report_inputs
BEGIN
    SELECT RAISE(ABORT, 'task report inputs are immutable');
END;

CREATE INDEX verified_artifacts_producer_idx ON verified_artifacts(producer_task_id, producer_attempt_id);
CREATE INDEX task_report_inputs_artifact_idx ON task_report_inputs(artifact_id);
