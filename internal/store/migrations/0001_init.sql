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

CREATE TABLE tasks (
    id INTEGER PRIMARY KEY,
    parent INTEGER REFERENCES tasks(id),
    driver TEXT,
    root INTEGER NOT NULL,
    depth INTEGER NOT NULL,
    request INTEGER,
    role TEXT NOT NULL CHECK (role IN ('worker', 'driver')),
    repo INTEGER REFERENCES repos(id),
    title TEXT NOT NULL,
    objective TEXT NOT NULL,
    acceptance TEXT NOT NULL,
    deliverable TEXT NOT NULL CHECK (deliverable IN ('code', 'report', 'answer')),
    host TEXT NOT NULL,
    harness TEXT NOT NULL,
    model TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'waiting', 'held', 'done', 'closed')),
    reason TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    revision INTEGER NOT NULL,
    milestone TEXT NOT NULL CHECK (milestone IN ('', 'published', 'merged')),
    run INTEGER,
    wake_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((parent IS NULL) <> (driver IS NULL))
);

CREATE INDEX tasks_parent ON tasks (parent);
CREATE INDEX tasks_driver ON tasks (driver);

CREATE TABLE dependencies (
    task INTEGER NOT NULL REFERENCES tasks(id),
    dependency INTEGER NOT NULL REFERENCES tasks(id),
    until TEXT NOT NULL CHECK (until IN ('published', 'merged')),
    PRIMARY KEY (task, dependency)
);

CREATE TABLE requests (
    task INTEGER NOT NULL REFERENCES tasks(id),
    seq INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('open', 'asked', 'answered')),
    result INTEGER,
    PRIMARY KEY (task, seq)
);

CREATE TABLE attempts (
    task INTEGER NOT NULL REFERENCES tasks(id),
    n INTEGER NOT NULL,
    host TEXT NOT NULL,
    harness TEXT NOT NULL,
    model TEXT NOT NULL,
    base TEXT NOT NULL,
    stacked_on INTEGER REFERENCES tasks(id),
    branch TEXT NOT NULL,
    workspace TEXT NOT NULL,
    workspace_state TEXT NOT NULL CHECK (workspace_state IN ('none', 'allocating', 'held', 'releasing', 'released', 'unknown')),
    session TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (task, n)
);

CREATE TABLE runs (
    id INTEGER PRIMARY KEY,
    task INTEGER NOT NULL REFERENCES tasks(id),
    attempt INTEGER NOT NULL,
    generation INTEGER NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    host TEXT NOT NULL,
    purpose TEXT NOT NULL CHECK (purpose IN ('start', 'resume', 'retry', 'continue', 'nudge')),
    dir TEXT NOT NULL,
    pid INTEGER,
    start_time TEXT,
    endpoint TEXT NOT NULL DEFAULT '',
    liveness TEXT NOT NULL CHECK (liveness IN ('starting', 'live', 'exited', 'unknown')),
    turn_reported INTEGER NOT NULL DEFAULT 0,
    exit_status INTEGER,
    stop_reason TEXT NOT NULL DEFAULT '',
    warned_long INTEGER NOT NULL DEFAULT 0,
    from_seq INTEGER NOT NULL,
    started_at TEXT NOT NULL,
    last_activity TEXT NOT NULL,
    exited_at TEXT,
    UNIQUE (task, generation),
    FOREIGN KEY (task, attempt) REFERENCES attempts (task, n)
);

CREATE TABLE plugin_data (
    task INTEGER NOT NULL REFERENCES tasks(id),
    plugin TEXT NOT NULL,
    data TEXT NOT NULL,
    PRIMARY KEY (task, plugin)
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
