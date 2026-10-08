ALTER TABLE verified_artifacts ADD COLUMN accepted_event_id TEXT NOT NULL DEFAULT '';
ALTER TABLE verified_artifacts ADD COLUMN accepted_event_name TEXT NOT NULL DEFAULT '';
ALTER TABLE verified_artifacts ADD COLUMN accepted_event_version INTEGER NOT NULL DEFAULT 0;

CREATE UNIQUE INDEX verified_artifacts_accepted_event_idx ON verified_artifacts(accepted_event_id) WHERE accepted_event_id <> '';

CREATE TABLE report_lifecycle_invocations (
    id TEXT PRIMARY KEY,
    event_id TEXT NOT NULL,
    event_name TEXT NOT NULL CHECK(event_name = 'report.accepted'),
    event_version INTEGER NOT NULL CHECK(event_version = 1),
    artifact_id TEXT NOT NULL REFERENCES verified_artifacts(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    handler_name TEXT NOT NULL,
    extension_id TEXT NOT NULL,
    configuration_hash TEXT NOT NULL CHECK(length(configuration_hash) = 64 AND configuration_hash = lower(configuration_hash)),
    position INTEGER NOT NULL CHECK(position > 0),
    state TEXT NOT NULL CHECK(state IN ('pending', 'invoking', 'succeeded', 'failed', 'unknown')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
    claim_token TEXT NOT NULL DEFAULT '',
    annotation TEXT NOT NULL DEFAULT '',
    receipt_system TEXT NOT NULL DEFAULT '',
    receipt_id TEXT NOT NULL DEFAULT '',
    failure_kind TEXT NOT NULL DEFAULT '',
    failure_message TEXT NOT NULL DEFAULT '',
    effect TEXT NOT NULL DEFAULT '',
    started_at TEXT,
    deadline_at TEXT,
    completed_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(event_id, handler_name),
    UNIQUE(artifact_id, handler_name)
);

CREATE TRIGGER report_lifecycle_invocations_identity_insert
BEFORE INSERT ON report_lifecycle_invocations
WHEN NOT EXISTS (
    SELECT 1 FROM verified_artifacts artifact
    WHERE artifact.id = NEW.artifact_id
      AND artifact.accepted_event_id = NEW.event_id
      AND artifact.accepted_event_name = NEW.event_name
      AND artifact.accepted_event_version = NEW.event_version
      AND artifact.producer_task_id = NEW.task_id
      AND artifact.producer_attempt_id = NEW.attempt_id
)
BEGIN
    SELECT RAISE(ABORT, 'report lifecycle invocation identity does not match accepted artifact');
END;

CREATE TRIGGER report_lifecycle_invocations_identity_update
BEFORE UPDATE OF event_id, event_name, event_version, artifact_id, task_id, attempt_id, handler_name, extension_id, configuration_hash, position, created_at ON report_lifecycle_invocations
BEGIN
    SELECT RAISE(ABORT, 'report lifecycle invocation identity is immutable');
END;

CREATE TRIGGER report_lifecycle_invocations_delete
BEFORE DELETE ON report_lifecycle_invocations
BEGIN
    SELECT RAISE(ABORT, 'report lifecycle invocations are durable');
END;

CREATE INDEX report_lifecycle_invocations_task_idx ON report_lifecycle_invocations(task_id, attempt_id, position);
CREATE INDEX report_lifecycle_invocations_state_idx ON report_lifecycle_invocations(state, deadline_at);
