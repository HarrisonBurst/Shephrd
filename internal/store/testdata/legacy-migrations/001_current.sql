CREATE TABLE repos (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    path TEXT NOT NULL UNIQUE,
    default_branch TEXT NOT NULL,
    context_file TEXT NOT NULL DEFAULT '',
    setup_hook TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    repo_id TEXT NOT NULL REFERENCES repos(id),
    feature_key TEXT NOT NULL,
    group_id TEXT NOT NULL DEFAULT '',
    driver_id TEXT NOT NULL,
    objective TEXT NOT NULL,
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    deliverable TEXT NOT NULL DEFAULT 'code',
    status TEXT NOT NULL,
    current_attempt_id TEXT,
    artifact_ref TEXT NOT NULL DEFAULT '',
    claimed_done INTEGER NOT NULL DEFAULT 0,
    process_alive INTEGER NOT NULL DEFAULT 0,
    branch_pushed INTEGER NOT NULL DEFAULT 0,
    pr_state TEXT NOT NULL DEFAULT '',
    landed INTEGER NOT NULL DEFAULT 0,
    landed_reason TEXT NOT NULL DEFAULT '',
    discard_authorized INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(repo_id, feature_key)
);

CREATE TABLE attempts (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    number INTEGER NOT NULL,
    harness TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    runtime_backend TEXT NOT NULL DEFAULT 'headless',
    runtime_generation INTEGER NOT NULL DEFAULT 0,
    run_generation INTEGER NOT NULL DEFAULT 0,
    resume_source_attempt_id TEXT REFERENCES attempts(id),
    resume_source_revision INTEGER NOT NULL DEFAULT 0,
    herdr_socket_path TEXT NOT NULL DEFAULT '',
    herdr_workspace_id TEXT NOT NULL DEFAULT '',
    herdr_tab_id TEXT NOT NULL DEFAULT '',
    herdr_pane_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    worktree_path TEXT NOT NULL DEFAULT '',
    lease_id TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    runner_pid INTEGER NOT NULL DEFAULT 0,
    cursor INTEGER NOT NULL DEFAULT 0,
    exit_code INTEGER,
    failure_reason TEXT NOT NULL DEFAULT '',
    landed_proven INTEGER NOT NULL DEFAULT 0,
    discard_authorized INTEGER NOT NULL DEFAULT 0,
    released_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    ended_at TEXT,
    UNIQUE(task_id, number)
);

CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    direction TEXT NOT NULL,
    type TEXT NOT NULL,
    payload TEXT NOT NULL,
    artifact_ref TEXT NOT NULL DEFAULT '',
    stale INTEGER NOT NULL DEFAULT 0,
    wake INTEGER NOT NULL DEFAULT 0,
    source_cursor INTEGER NOT NULL DEFAULT 0,
    run_generation INTEGER NOT NULL DEFAULT 0,
    checkpoint_json TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE TABLE attempt_checkpoints (
    attempt_id TEXT PRIMARY KEY REFERENCES attempts(id),
    revision INTEGER NOT NULL,
    schema_version INTEGER NOT NULL,
    producer TEXT NOT NULL,
    run_generation INTEGER NOT NULL,
    source_cursor INTEGER NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    head_commit TEXT NOT NULL DEFAULT '',
    worktree_dirty INTEGER NOT NULL DEFAULT 0,
    workspace_facts_error TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL,
    completed_json TEXT NOT NULL,
    next_steps_json TEXT NOT NULL,
    decisions_json TEXT NOT NULL,
    changed_paths_json TEXT NOT NULL,
    checks_json TEXT NOT NULL,
    blockers_json TEXT NOT NULL,
    source_attempt_id TEXT NOT NULL DEFAULT '',
    source_revision INTEGER NOT NULL DEFAULT 0,
    captured_at TEXT NOT NULL
);

CREATE TABLE driver_notifications (
    notification_id TEXT PRIMARY KEY,
    message_id INTEGER NOT NULL UNIQUE REFERENCES messages(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    target_driver_id TEXT NOT NULL,
    worker_run_generation INTEGER NOT NULL,
    source_cursor INTEGER NOT NULL,
    kind TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('pending', 'claimed', 'acknowledged', 'superseded')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    claim_owner TEXT NOT NULL DEFAULT '',
    driver_generation TEXT NOT NULL DEFAULT '',
    claim_token TEXT NOT NULL DEFAULT '',
    claimed_at TEXT,
    claim_until TEXT,
    delivery_attempts INTEGER NOT NULL DEFAULT 0,
    acked_at TEXT,
    ack_owner TEXT NOT NULL DEFAULT '',
    ack_driver_generation TEXT NOT NULL DEFAULT '',
    handling_id TEXT NOT NULL DEFAULT '',
    superseded_at TEXT,
    supersede_reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE notification_delivery_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    notification_id TEXT NOT NULL REFERENCES driver_notifications(notification_id),
    target_driver_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK(operation IN ('claim', 'reclaim', 'renew', 'notify', 'ack', 'adopt', 'supersede', 'dedupe')),
    consumer_id TEXT NOT NULL DEFAULT '',
    driver_generation TEXT NOT NULL DEFAULT '',
    claim_token TEXT NOT NULL DEFAULT '',
    handling_id TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE TABLE memory (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id TEXT NOT NULL REFERENCES repos(id),
    task_id TEXT REFERENCES tasks(id),
    note TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(repo_id, note)
);

CREATE INDEX attempts_task_idx ON attempts(task_id, number);
CREATE INDEX tasks_status_idx ON tasks(status, updated_at);
CREATE INDEX attempt_checkpoints_revision_idx ON attempt_checkpoints(run_generation, revision);
CREATE INDEX messages_attempt_generation_idx ON messages(attempt_id, run_generation, source_cursor, id);
CREATE INDEX driver_notifications_order_idx ON driver_notifications(target_driver_id, state, created_at, message_id);
CREATE INDEX driver_notifications_task_idx ON driver_notifications(task_id, target_driver_id, state, created_at, message_id);
CREATE INDEX driver_notifications_claim_idx ON driver_notifications(state, claim_until);
CREATE INDEX driver_notifications_attempt_idx ON driver_notifications(attempt_id, worker_run_generation, state);
CREATE INDEX notification_delivery_log_target_idx ON notification_delivery_log(target_driver_id, id);
