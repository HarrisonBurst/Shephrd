ALTER TABLE tasks ADD COLUMN archived_at TEXT;

CREATE INDEX tasks_archive_idx ON tasks(archived_at, created_at);
