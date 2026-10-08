ALTER TABLE tasks ADD COLUMN title TEXT NOT NULL DEFAULT '';
UPDATE tasks
SET title=CASE
    WHEN TRIM(feature_key)<>'' THEN feature_key
    WHEN TRIM(objective)<>'' THEN objective
    ELSE 'Untitled task'
END;

ALTER TABLE task_list_items ADD COLUMN title TEXT NOT NULL DEFAULT '';
UPDATE task_list_items
SET title=CASE
    WHEN TRIM(feature_key)<>'' THEN feature_key
    WHEN TRIM(objective)<>'' THEN objective
    ELSE 'Untitled task'
END;

CREATE TRIGGER tasks_title_required_insert
BEFORE INSERT ON tasks
WHEN TRIM(NEW.title)=''
BEGIN
    SELECT RAISE(ABORT, 'task title must not be empty');
END;

CREATE TRIGGER tasks_title_required_update
BEFORE UPDATE OF title ON tasks
WHEN TRIM(NEW.title)=''
BEGIN
    SELECT RAISE(ABORT, 'task title must not be empty');
END;

CREATE TRIGGER task_list_items_title_required_insert
BEFORE INSERT ON task_list_items
WHEN TRIM(NEW.title)=''
BEGIN
    SELECT RAISE(ABORT, 'task-list item title must not be empty');
END;

CREATE TRIGGER task_list_items_title_required_update
BEFORE UPDATE OF title ON task_list_items
WHEN TRIM(NEW.title)=''
BEGIN
    SELECT RAISE(ABORT, 'task-list item title must not be empty');
END;

DROP TRIGGER task_list_items_dispatched_scope_immutable;
CREATE TRIGGER task_list_items_dispatched_scope_immutable
BEFORE UPDATE OF title, feature_key, objective, description, acceptance_criteria, repo_id, deliverable ON task_list_items
WHEN OLD.dispatched_task_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'dispatched task-list item scope is immutable');
END;
