DROP TRIGGER task_report_inputs_immutable_update;
DROP TRIGGER task_report_inputs_immutable_delete;
DROP INDEX task_report_inputs_artifact_idx;

ALTER TABLE task_report_inputs RENAME TO task_report_inputs_positioned;

CREATE TABLE task_report_inputs (
    target_task_id TEXT NOT NULL REFERENCES tasks(id),
    position INTEGER NOT NULL DEFAULT 1 CHECK(position > 0),
    artifact_id TEXT NOT NULL REFERENCES verified_artifacts(id),
    attached_by_driver_id TEXT NOT NULL,
    attached_at TEXT NOT NULL,
    PRIMARY KEY(target_task_id, artifact_id),
    UNIQUE(target_task_id, position)
);

INSERT INTO task_report_inputs(target_task_id, position, artifact_id, attached_by_driver_id, attached_at)
SELECT target_task_id, position, artifact_id, attached_by_driver_id, attached_at
FROM task_report_inputs_positioned;

DROP TABLE task_report_inputs_positioned;

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

CREATE INDEX task_report_inputs_artifact_idx ON task_report_inputs(artifact_id);
