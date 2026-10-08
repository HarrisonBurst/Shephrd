DROP TRIGGER task_report_inputs_immutable_update;
DROP TRIGGER task_report_inputs_immutable_delete;
DROP INDEX task_report_inputs_artifact_idx;

ALTER TABLE task_report_inputs RENAME TO task_report_inputs_single;

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
SELECT target_task_id, 1, artifact_id, attached_by_driver_id, attached_at
FROM task_report_inputs_single;

DROP TABLE task_report_inputs_single;

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

CREATE TABLE task_lists (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    driver_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(driver_id, name)
);

CREATE TABLE task_list_items (
    id TEXT PRIMARY KEY,
    task_list_id TEXT NOT NULL REFERENCES task_lists(id),
    position INTEGER NOT NULL CHECK(position > 0),
    feature_key TEXT NOT NULL DEFAULT '',
    objective TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    repo_id TEXT REFERENCES repos(id),
    deliverable TEXT NOT NULL DEFAULT 'code' CHECK(deliverable IN ('code', 'report')),
    dispatched_task_id TEXT UNIQUE REFERENCES tasks(id),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(task_list_id, position)
);

CREATE TABLE task_list_prerequisites (
    item_id TEXT NOT NULL REFERENCES task_list_items(id),
    prerequisite_item_id TEXT NOT NULL REFERENCES task_list_items(id),
    created_at TEXT NOT NULL,
    PRIMARY KEY(item_id, prerequisite_item_id),
    CHECK(item_id <> prerequisite_item_id)
);

CREATE TABLE task_list_inputs (
    item_id TEXT NOT NULL REFERENCES task_list_items(id),
    prerequisite_item_id TEXT NOT NULL REFERENCES task_list_items(id),
    position INTEGER NOT NULL CHECK(position > 0),
    artifact_id TEXT REFERENCES verified_artifacts(id),
    selected_by_driver_id TEXT NOT NULL DEFAULT '',
    selected_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(item_id, prerequisite_item_id),
    UNIQUE(item_id, position)
);

CREATE TRIGGER task_list_items_list_immutable
BEFORE UPDATE OF task_list_id ON task_list_items
BEGIN
    SELECT RAISE(ABORT, 'task-list item list identity is immutable');
END;

CREATE TRIGGER task_list_items_dispatch_immutable
BEFORE UPDATE OF dispatched_task_id ON task_list_items
WHEN OLD.dispatched_task_id IS NOT NULL OR NEW.dispatched_task_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'task-list item dispatch identity is immutable');
END;

CREATE TRIGGER task_list_items_dispatched_scope_immutable
BEFORE UPDATE OF feature_key, objective, description, acceptance_criteria, repo_id, deliverable ON task_list_items
WHEN OLD.dispatched_task_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'dispatched task-list item scope is immutable');
END;

CREATE TRIGGER task_list_items_dispatched_delete
BEFORE DELETE ON task_list_items
WHEN OLD.dispatched_task_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'dispatched task-list items cannot be deleted');
END;

CREATE TRIGGER task_list_prerequisites_validate_insert
BEFORE INSERT ON task_list_prerequisites
BEGIN
    SELECT CASE WHEN
        (SELECT task_list_id FROM task_list_items WHERE id=NEW.item_id) IS NULL OR
        (SELECT task_list_id FROM task_list_items WHERE id=NEW.prerequisite_item_id) IS NULL OR
        (SELECT task_list_id FROM task_list_items WHERE id=NEW.item_id) <> (SELECT task_list_id FROM task_list_items WHERE id=NEW.prerequisite_item_id)
    THEN RAISE(ABORT, 'task-list prerequisites must belong to the same list') END;
    SELECT CASE WHEN (SELECT dispatched_task_id FROM task_list_items WHERE id=NEW.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched task-list item prerequisites are immutable') END;
    SELECT CASE WHEN EXISTS(
        WITH RECURSIVE dependencies(id) AS (
            SELECT NEW.prerequisite_item_id
            UNION
            SELECT p.prerequisite_item_id
            FROM task_list_prerequisites p
            JOIN dependencies d ON p.item_id=d.id
        )
        SELECT 1 FROM dependencies WHERE id=NEW.item_id
    ) THEN RAISE(ABORT, 'task-list prerequisite cycle') END;
END;

CREATE TRIGGER task_list_prerequisites_validate_update
BEFORE UPDATE ON task_list_prerequisites
BEGIN
    SELECT RAISE(ABORT, 'task-list prerequisite identity is immutable');
END;

CREATE TRIGGER task_list_prerequisites_validate_delete
BEFORE DELETE ON task_list_prerequisites
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM task_list_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched task-list item prerequisites are immutable') END;
    SELECT CASE WHEN EXISTS(
        SELECT 1 FROM task_list_inputs WHERE item_id=OLD.item_id AND prerequisite_item_id=OLD.prerequisite_item_id
    ) THEN RAISE(ABORT, 'remove the task-list input before its prerequisite') END;
END;

CREATE TRIGGER task_list_inputs_validate_insert
BEFORE INSERT ON task_list_inputs
BEGIN
    SELECT CASE WHEN NOT EXISTS(
        SELECT 1 FROM task_list_prerequisites WHERE item_id=NEW.item_id AND prerequisite_item_id=NEW.prerequisite_item_id
    ) THEN RAISE(ABORT, 'task-list input must reference a prerequisite') END;
    SELECT CASE WHEN (SELECT dispatched_task_id FROM task_list_items WHERE id=NEW.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched task-list item inputs are immutable') END;
    SELECT CASE WHEN NEW.artifact_id IS NOT NULL AND NOT EXISTS(
        SELECT 1
        FROM verified_artifacts va
        JOIN task_list_items prerequisite ON prerequisite.id=NEW.prerequisite_item_id
        WHERE va.id=NEW.artifact_id AND va.producer_task_id=prerequisite.dispatched_task_id
    ) THEN RAISE(ABORT, 'task-list input artifact does not belong to its prerequisite task') END;
END;

CREATE TRIGGER task_list_inputs_validate_update
BEFORE UPDATE ON task_list_inputs
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM task_list_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched task-list item inputs are immutable') END;
    SELECT CASE WHEN NEW.item_id<>OLD.item_id OR NEW.prerequisite_item_id<>OLD.prerequisite_item_id
    THEN RAISE(ABORT, 'task-list input identity is immutable') END;
    SELECT CASE WHEN NEW.artifact_id IS NOT NULL AND NOT EXISTS(
        SELECT 1
        FROM verified_artifacts va
        JOIN task_list_items prerequisite ON prerequisite.id=NEW.prerequisite_item_id
        WHERE va.id=NEW.artifact_id AND va.producer_task_id=prerequisite.dispatched_task_id
    ) THEN RAISE(ABORT, 'task-list input artifact does not belong to its prerequisite task') END;
END;

CREATE TRIGGER task_list_inputs_validate_delete
BEFORE DELETE ON task_list_inputs
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM task_list_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched task-list item inputs are immutable') END;
END;

CREATE INDEX task_lists_driver_idx ON task_lists(driver_id, created_at);
CREATE INDEX task_list_items_list_idx ON task_list_items(task_list_id, position);
CREATE INDEX task_list_prerequisites_prerequisite_idx ON task_list_prerequisites(prerequisite_item_id, item_id);
CREATE INDEX task_list_inputs_item_idx ON task_list_inputs(item_id, position);
CREATE INDEX task_list_inputs_artifact_idx ON task_list_inputs(artifact_id);
