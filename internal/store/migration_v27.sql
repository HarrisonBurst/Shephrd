CREATE TABLE plans (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    driver_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(driver_id, name)
);

CREATE TABLE plan_items (
    id TEXT PRIMARY KEY,
    plan_id TEXT NOT NULL REFERENCES plans(id),
    position INTEGER NOT NULL CHECK(position > 0),
    feature_key TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '',
    objective TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    repo_id TEXT REFERENCES repos(id),
    deliverable TEXT NOT NULL DEFAULT 'code' CHECK(deliverable IN ('code','report')),
    dispatched_task_id TEXT UNIQUE REFERENCES tasks(id),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(plan_id, position)
);

CREATE TABLE plan_prerequisites (
    item_id TEXT NOT NULL REFERENCES plan_items(id),
    prerequisite_item_id TEXT NOT NULL REFERENCES plan_items(id),
    created_at TEXT NOT NULL,
    PRIMARY KEY(item_id, prerequisite_item_id),
    CHECK(item_id <> prerequisite_item_id)
);

CREATE TABLE plan_report_inputs (
    item_id TEXT NOT NULL REFERENCES plan_items(id),
    prerequisite_item_id TEXT NOT NULL REFERENCES plan_items(id),
    position INTEGER NOT NULL CHECK(position > 0),
    artifact_id TEXT REFERENCES verified_artifacts(id),
    selected_by_driver_id TEXT NOT NULL DEFAULT '',
    selected_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(item_id, prerequisite_item_id),
    UNIQUE(item_id, position)
);

CREATE TABLE annotations (
    id TEXT PRIMARY KEY,
    driver_id TEXT NOT NULL CHECK(
        length(CAST(driver_id AS BLOB)) BETWEEN 1 AND 256 AND
        driver_id = trim(driver_id, char(9) || char(10) || char(11) || char(12) || char(13) || char(32) || char(133) || char(160) || char(5760) || char(8192) || char(8193) || char(8194) || char(8195) || char(8196) || char(8197) || char(8198) || char(8199) || char(8200) || char(8201) || char(8202) || char(8232) || char(8233) || char(8239) || char(8287) || char(12288)) AND
        instr(driver_id, char(0)) = 0
    ),
    task_id TEXT REFERENCES tasks(id),
    plan_id TEXT REFERENCES plans(id),
    plan_item_id TEXT,
    revision INTEGER NOT NULL CHECK(revision > 0),
    judgment TEXT NOT NULL CHECK(
        length(CAST(judgment AS BLOB)) BETWEEN 1 AND 4096 AND
        judgment = trim(judgment, char(9) || char(10) || char(11) || char(12) || char(13) || char(32) || char(133) || char(160) || char(5760) || char(8192) || char(8193) || char(8194) || char(8195) || char(8196) || char(8197) || char(8198) || char(8199) || char(8200) || char(8201) || char(8202) || char(8232) || char(8233) || char(8239) || char(8287) || char(12288)) AND
        instr(judgment, char(0)) = 0
    ),
    reason TEXT NOT NULL DEFAULT '' CHECK(
        length(CAST(reason AS BLOB)) <= 1024 AND
        reason = trim(reason, char(9) || char(10) || char(11) || char(12) || char(13) || char(32) || char(133) || char(160) || char(5760) || char(8192) || char(8193) || char(8194) || char(8195) || char(8196) || char(8197) || char(8198) || char(8199) || char(8200) || char(8201) || char(8202) || char(8232) || char(8233) || char(8239) || char(8287) || char(12288)) AND
        instr(reason, char(0)) = 0
    ),
    next_action TEXT NOT NULL DEFAULT '' CHECK(
        length(CAST(next_action AS BLOB)) <= 1024 AND
        next_action = trim(next_action, char(9) || char(10) || char(11) || char(12) || char(13) || char(32) || char(133) || char(160) || char(5760) || char(8192) || char(8193) || char(8194) || char(8195) || char(8196) || char(8197) || char(8198) || char(8199) || char(8200) || char(8201) || char(8202) || char(8232) || char(8233) || char(8239) || char(8287) || char(12288)) AND
        instr(next_action, char(0)) = 0
    ),
    created_at TEXT NOT NULL,
    CHECK(
        (task_id IS NOT NULL AND plan_id IS NULL AND plan_item_id IS NULL) OR
        (task_id IS NULL AND plan_id IS NOT NULL AND plan_item_id IS NOT NULL)
    )
);

INSERT INTO plans(id, name, driver_id, created_at, updated_at)
SELECT id, name, driver_id, created_at, updated_at FROM task_lists;

INSERT INTO plan_items(id, plan_id, position, feature_key, title, objective, description, acceptance_criteria, repo_id, deliverable, dispatched_task_id, created_at, updated_at)
SELECT id, task_list_id, position, feature_key, title, objective, description, acceptance_criteria, repo_id, deliverable, dispatched_task_id, created_at, updated_at FROM task_list_items;

INSERT INTO plan_prerequisites(item_id, prerequisite_item_id, created_at)
SELECT item_id, prerequisite_item_id, created_at FROM task_list_prerequisites;

INSERT INTO plan_report_inputs(item_id, prerequisite_item_id, position, artifact_id, selected_by_driver_id, selected_at, created_at, updated_at)
SELECT item_id, prerequisite_item_id, position, artifact_id, selected_by_driver_id, selected_at, created_at, updated_at FROM task_list_inputs;

INSERT INTO annotations(id, driver_id, task_id, plan_id, plan_item_id, revision, judgment, reason, next_action, created_at)
SELECT id, driver_id, task_id, task_list_id, task_list_item_id, revision, decision, reason, next_action, created_at FROM driver_decisions;

DROP TABLE task_list_inputs;
DROP TABLE task_list_prerequisites;
DROP TABLE task_list_items;
DROP TABLE task_lists;
DROP TABLE driver_decisions;

CREATE TRIGGER plan_items_list_immutable
BEFORE UPDATE OF plan_id ON plan_items
BEGIN
    SELECT RAISE(ABORT, 'plan item plan identity is immutable');
END;

CREATE TRIGGER plan_items_dispatch_immutable
BEFORE UPDATE OF dispatched_task_id ON plan_items
WHEN OLD.dispatched_task_id IS NOT NULL OR NEW.dispatched_task_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'plan item dispatch identity is immutable');
END;

CREATE TRIGGER plan_items_dispatched_scope_immutable
BEFORE UPDATE OF title, feature_key, objective, description, acceptance_criteria, repo_id, deliverable ON plan_items
WHEN OLD.dispatched_task_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'dispatched plan item scope is immutable');
END;

CREATE TRIGGER plan_items_dispatched_delete
BEFORE DELETE ON plan_items
WHEN OLD.dispatched_task_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'dispatched plan items cannot be deleted');
END;

CREATE TRIGGER plan_items_title_required_insert
BEFORE INSERT ON plan_items
WHEN TRIM(NEW.title)=''
BEGIN
    SELECT RAISE(ABORT, 'plan item title must not be empty');
END;

CREATE TRIGGER plan_items_title_required_update
BEFORE UPDATE OF title ON plan_items
WHEN TRIM(NEW.title)=''
BEGIN
    SELECT RAISE(ABORT, 'plan item title must not be empty');
END;

CREATE TRIGGER plan_prerequisites_validate_insert
BEFORE INSERT ON plan_prerequisites
BEGIN
    SELECT CASE WHEN
        (SELECT plan_id FROM plan_items WHERE id=NEW.item_id) IS NULL OR
        (SELECT plan_id FROM plan_items WHERE id=NEW.prerequisite_item_id) IS NULL OR
        (SELECT plan_id FROM plan_items WHERE id=NEW.item_id) <> (SELECT plan_id FROM plan_items WHERE id=NEW.prerequisite_item_id)
    THEN RAISE(ABORT, 'plan prerequisites must belong to the same plan') END;
    SELECT CASE WHEN (SELECT dispatched_task_id FROM plan_items WHERE id=NEW.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched plan item prerequisites are immutable') END;
    SELECT CASE WHEN EXISTS(
        WITH RECURSIVE dependencies(id) AS (
            SELECT NEW.prerequisite_item_id
            UNION
            SELECT p.prerequisite_item_id
            FROM plan_prerequisites p
            JOIN dependencies d ON p.item_id=d.id
        )
        SELECT 1 FROM dependencies WHERE id=NEW.item_id
    ) THEN RAISE(ABORT, 'plan prerequisite cycle') END;
END;

CREATE TRIGGER plan_prerequisites_validate_update
BEFORE UPDATE ON plan_prerequisites
BEGIN
    SELECT RAISE(ABORT, 'plan prerequisite identity is immutable');
END;

CREATE TRIGGER plan_prerequisites_validate_delete
BEFORE DELETE ON plan_prerequisites
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM plan_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched plan item prerequisites are immutable') END;
    SELECT CASE WHEN EXISTS(
        SELECT 1 FROM plan_report_inputs WHERE item_id=OLD.item_id AND prerequisite_item_id=OLD.prerequisite_item_id
    ) THEN RAISE(ABORT, 'remove the plan report input before its prerequisite') END;
END;

CREATE TRIGGER plan_report_inputs_validate_insert
BEFORE INSERT ON plan_report_inputs
BEGIN
    SELECT CASE WHEN NOT EXISTS(
        SELECT 1 FROM plan_prerequisites WHERE item_id=NEW.item_id AND prerequisite_item_id=NEW.prerequisite_item_id
    ) THEN RAISE(ABORT, 'plan report input must reference a prerequisite') END;
    SELECT CASE WHEN (SELECT dispatched_task_id FROM plan_items WHERE id=NEW.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched plan item report inputs are immutable') END;
    SELECT CASE WHEN NEW.artifact_id IS NOT NULL AND NOT EXISTS(
        SELECT 1
        FROM verified_artifacts va
        JOIN plan_items prerequisite ON prerequisite.id=NEW.prerequisite_item_id
        WHERE va.id=NEW.artifact_id AND va.producer_task_id=prerequisite.dispatched_task_id
    ) THEN RAISE(ABORT, 'plan report input artifact does not belong to its prerequisite task') END;
END;

CREATE TRIGGER plan_report_inputs_validate_update
BEFORE UPDATE ON plan_report_inputs
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM plan_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched plan item report inputs are immutable') END;
    SELECT CASE WHEN NEW.item_id<>OLD.item_id OR NEW.prerequisite_item_id<>OLD.prerequisite_item_id
    THEN RAISE(ABORT, 'plan report input identity is immutable') END;
    SELECT CASE WHEN NEW.artifact_id IS NOT NULL AND NOT EXISTS(
        SELECT 1
        FROM verified_artifacts va
        JOIN plan_items prerequisite ON prerequisite.id=NEW.prerequisite_item_id
        WHERE va.id=NEW.artifact_id AND va.producer_task_id=prerequisite.dispatched_task_id
    ) THEN RAISE(ABORT, 'plan report input artifact does not belong to its prerequisite task') END;
END;

CREATE TRIGGER plan_report_inputs_validate_delete
BEFORE DELETE ON plan_report_inputs
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM plan_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched plan item report inputs are immutable') END;
END;

CREATE TRIGGER annotations_item_scope_insert
BEFORE INSERT ON annotations
WHEN NEW.plan_item_id IS NOT NULL
BEGIN
    SELECT CASE WHEN NOT EXISTS(
        SELECT 1 FROM plan_items WHERE id=NEW.plan_item_id AND plan_id=NEW.plan_id
    ) THEN RAISE(ABORT, 'annotation item scope must reference a live item in its plan') END;
END;

CREATE TRIGGER annotations_revision_insert
BEFORE INSERT ON annotations
BEGIN
    SELECT CASE WHEN NEW.task_id IS NOT NULL AND NEW.revision <> COALESCE(
        (SELECT MAX(revision) FROM annotations WHERE task_id=NEW.task_id), 0
    ) + 1 THEN RAISE(ABORT, 'annotation task revision must append monotonically') END;
    SELECT CASE WHEN NEW.plan_item_id IS NOT NULL AND NEW.revision <> COALESCE(
        (SELECT MAX(revision) FROM annotations WHERE plan_id=NEW.plan_id AND plan_item_id=NEW.plan_item_id), 0
    ) + 1 THEN RAISE(ABORT, 'annotation item revision must append monotonically') END;
END;

CREATE TRIGGER annotations_immutable_update
BEFORE UPDATE ON annotations
BEGIN
    SELECT RAISE(ABORT, 'annotations are immutable');
END;

CREATE TRIGGER annotations_immutable_delete
BEFORE DELETE ON annotations
BEGIN
    SELECT RAISE(ABORT, 'annotations are immutable');
END;

CREATE INDEX plans_driver_idx ON plans(driver_id, created_at);
CREATE INDEX plan_items_plan_idx ON plan_items(plan_id, position);
CREATE INDEX plan_prerequisites_prerequisite_idx ON plan_prerequisites(prerequisite_item_id, item_id);
CREATE INDEX plan_report_inputs_item_idx ON plan_report_inputs(item_id, position);
CREATE INDEX plan_report_inputs_artifact_idx ON plan_report_inputs(artifact_id);
CREATE UNIQUE INDEX annotations_task_rev ON annotations(task_id, revision) WHERE task_id IS NOT NULL;
CREATE UNIQUE INDEX annotations_item_rev ON annotations(plan_id, plan_item_id, revision) WHERE plan_item_id IS NOT NULL;
