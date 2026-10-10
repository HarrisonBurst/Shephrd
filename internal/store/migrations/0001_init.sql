CREATE TABLE repos (
    id INTEGER PRIMARY KEY,
    host TEXT NOT NULL,
    path TEXT NOT NULL,
    name TEXT NOT NULL UNIQUE,
    default_branch TEXT NOT NULL,
    setup TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    UNIQUE (host, path)
);

CREATE TABLE events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    task INTEGER,
    attempt INTEGER,
    run INTEGER,
    caller TEXT NOT NULL,
    data TEXT NOT NULL,
    time TEXT NOT NULL
);

CREATE TRIGGER events_append_only_update BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

CREATE TRIGGER events_append_only_delete BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

CREATE TABLE idempotency (
    caller TEXT NOT NULL,
    key TEXT NOT NULL,
    digest TEXT NOT NULL,
    result TEXT NOT NULL,
    time TEXT NOT NULL,
    PRIMARY KEY (caller, key)
);

CREATE TABLE call_tokens (
    hash TEXT PRIMARY KEY,
    plugin TEXT NOT NULL,
    acts_as TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
