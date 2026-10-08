CREATE TABLE driver_decisions (
    id TEXT PRIMARY KEY,
    driver_id TEXT NOT NULL CHECK(
        length(CAST(driver_id AS BLOB)) BETWEEN 1 AND 256 AND
        driver_id = trim(driver_id, char(9) || char(10) || char(11) || char(12) || char(13) || char(32) || char(133) || char(160) || char(5760) || char(8192) || char(8193) || char(8194) || char(8195) || char(8196) || char(8197) || char(8198) || char(8199) || char(8200) || char(8201) || char(8202) || char(8232) || char(8233) || char(8239) || char(8287) || char(12288)) AND
        instr(driver_id, char(0)) = 0
    ),
    task_id TEXT REFERENCES tasks(id),
    task_list_id TEXT REFERENCES task_lists(id),
    task_list_item_id TEXT,
    revision INTEGER NOT NULL CHECK(revision > 0),
    decision TEXT NOT NULL CHECK(
        length(CAST(decision AS BLOB)) BETWEEN 1 AND 4096 AND
        decision = trim(decision, char(9) || char(10) || char(11) || char(12) || char(13) || char(32) || char(133) || char(160) || char(5760) || char(8192) || char(8193) || char(8194) || char(8195) || char(8196) || char(8197) || char(8198) || char(8199) || char(8200) || char(8201) || char(8202) || char(8232) || char(8233) || char(8239) || char(8287) || char(12288)) AND
        instr(decision, char(0)) = 0
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
        (task_id IS NOT NULL AND task_list_id IS NULL AND task_list_item_id IS NULL) OR
        (task_id IS NULL AND task_list_id IS NOT NULL AND task_list_item_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX driver_decisions_task_rev ON driver_decisions(task_id, revision) WHERE task_id IS NOT NULL;
CREATE UNIQUE INDEX driver_decisions_item_rev ON driver_decisions(task_list_id, task_list_item_id, revision) WHERE task_list_item_id IS NOT NULL;

CREATE TRIGGER driver_decisions_item_scope_insert
BEFORE INSERT ON driver_decisions
WHEN NEW.task_list_item_id IS NOT NULL
BEGIN
    SELECT CASE WHEN NOT EXISTS(
        SELECT 1 FROM task_list_items WHERE id=NEW.task_list_item_id AND task_list_id=NEW.task_list_id
    ) THEN RAISE(ABORT, 'driver decision item scope must reference a live item in its list') END;
END;

CREATE TRIGGER driver_decisions_revision_insert
BEFORE INSERT ON driver_decisions
BEGIN
    SELECT CASE WHEN NEW.task_id IS NOT NULL AND NEW.revision <> COALESCE(
        (SELECT MAX(revision) FROM driver_decisions WHERE task_id=NEW.task_id), 0
    ) + 1 THEN RAISE(ABORT, 'driver decision task revision must append monotonically') END;
    SELECT CASE WHEN NEW.task_list_item_id IS NOT NULL AND NEW.revision <> COALESCE(
        (SELECT MAX(revision) FROM driver_decisions WHERE task_list_id=NEW.task_list_id AND task_list_item_id=NEW.task_list_item_id), 0
    ) + 1 THEN RAISE(ABORT, 'driver decision item revision must append monotonically') END;
END;

CREATE TRIGGER driver_decisions_immutable_update
BEFORE UPDATE ON driver_decisions
BEGIN
    SELECT RAISE(ABORT, 'driver decisions are immutable');
END;

CREATE TRIGGER driver_decisions_immutable_delete
BEFORE DELETE ON driver_decisions
BEGIN
    SELECT RAISE(ABORT, 'driver decisions are immutable');
END;
