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

CREATE TABLE "attempts" (
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
    base_commit TEXT NOT NULL DEFAULT '',
    landing_kind TEXT NOT NULL DEFAULT '',
    landed_source_commit TEXT NOT NULL DEFAULT '',
    landed_target_ref TEXT NOT NULL DEFAULT '',
    landed_target_commit TEXT NOT NULL DEFAULT '',
    landed_checkpoint_revision INTEGER NOT NULL DEFAULT 0,
    landed_verified_at TEXT,
    release_state TEXT NOT NULL DEFAULT 'held' CHECK(release_state IN ('held', 'releasing', 'released', 'no_workspace')),
    release_claimed_at TEXT,
    release_owner_pid INTEGER NOT NULL DEFAULT 0,
    release_reason TEXT NOT NULL DEFAULT '',
    runtime_executable TEXT NOT NULL DEFAULT '',
    landing_quarantine_reason TEXT NOT NULL DEFAULT '', workspace_backend TEXT NOT NULL DEFAULT ''
    CHECK(workspace_backend IN ('', 'treehouse', 'native_git_worktree')), workspace_state TEXT NOT NULL DEFAULT ''
    CHECK(workspace_state IN ('', 'allocating', 'held', 'no_workspace', 'unknown', 'releasing', 'released')), intended_worktree_path TEXT NOT NULL DEFAULT '', worktree_git_dir TEXT NOT NULL DEFAULT '', worktree_common_dir TEXT NOT NULL DEFAULT '', workspace_state_changed_at TEXT, landing_reason TEXT NOT NULL DEFAULT '', terminal_socket_path TEXT NOT NULL DEFAULT '', terminal_window_id TEXT NOT NULL DEFAULT '', terminal_workspace_id TEXT NOT NULL DEFAULT '', terminal_tab_id TEXT NOT NULL DEFAULT '', terminal_pane_id TEXT NOT NULL DEFAULT '', terminal_surface_id TEXT NOT NULL DEFAULT '', terminal_provider_version TEXT NOT NULL DEFAULT '', terminal_protocol_version TEXT NOT NULL DEFAULT '', terminal_capabilities_json TEXT NOT NULL DEFAULT '[]', terminal_create_state TEXT NOT NULL DEFAULT '', terminal_create_run_generation INTEGER NOT NULL DEFAULT 0, terminal_create_backend TEXT NOT NULL DEFAULT '', terminal_create_source TEXT NOT NULL DEFAULT '', terminal_create_window_id TEXT NOT NULL DEFAULT '', terminal_create_workspace_id TEXT NOT NULL DEFAULT '', terminal_create_cwd TEXT NOT NULL DEFAULT '', terminal_create_label TEXT NOT NULL DEFAULT '', terminal_create_generation INTEGER NOT NULL DEFAULT 0,
    UNIQUE(task_id, number),
    CHECK((release_state = 'released') = (released_at IS NOT NULL)),
    CHECK(landed_proven = 0 OR (landing_kind <> '' AND landed_verified_at IS NOT NULL))
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

CREATE TABLE external_delivery_attestations (
    id TEXT PRIMARY KEY,
    schema_version INTEGER NOT NULL CHECK(schema_version = 1),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    attempt_id TEXT NOT NULL UNIQUE REFERENCES attempts(id),
    repo_id TEXT NOT NULL REFERENCES repos(id),
    run_generation INTEGER NOT NULL,
    done_message_id INTEGER NOT NULL REFERENCES messages(id),
    checkpoint_revision INTEGER NOT NULL,
    original_artifact_ref TEXT NOT NULL,
    sealed_commit TEXT NOT NULL CHECK(length(sealed_commit) IN (40, 64) AND sealed_commit = lower(sealed_commit)),
    provider TEXT NOT NULL CHECK(provider = 'github'),
    remote_host TEXT NOT NULL,
    remote_repository TEXT NOT NULL,
    pr_number INTEGER NOT NULL CHECK(pr_number > 0),
    pr_node_id TEXT NOT NULL,
    pr_url TEXT NOT NULL,
    registered_default_branch TEXT NOT NULL,
    pr_base_ref TEXT NOT NULL,
    pr_head_ref TEXT NOT NULL,
    pr_head_commit TEXT NOT NULL,
    merge_commit TEXT NOT NULL,
    merged_at TEXT NOT NULL,
    default_head_at_validation TEXT NOT NULL,
    graph_validation TEXT NOT NULL,
    attested_by_driver_id TEXT NOT NULL,
    evidence_validated_at TEXT NOT NULL,
    created_at TEXT NOT NULL, evidence_digest TEXT NOT NULL DEFAULT '' CHECK(length(evidence_digest) IN (0, 64) AND (evidence_digest = '' OR evidence_digest = lower(evidence_digest))),
    UNIQUE(task_id, attempt_id, run_generation, done_message_id, checkpoint_revision)
);

CREATE TABLE local_delivery_recoveries (
    id TEXT PRIMARY KEY,
    schema_version INTEGER NOT NULL CHECK(schema_version = 1),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    attempt_id TEXT NOT NULL UNIQUE REFERENCES attempts(id),
    repo_id TEXT NOT NULL REFERENCES repos(id),
    run_generation INTEGER NOT NULL,
    done_message_id INTEGER NOT NULL REFERENCES messages(id),
    checkpoint_revision INTEGER NOT NULL,
    original_artifact_ref TEXT NOT NULL CHECK(original_artifact_ref LIKE 'branch:%'),
    attempt_branch TEXT NOT NULL CHECK(attempt_branch <> '' AND original_artifact_ref <> 'branch:' || attempt_branch),
    sealed_commit TEXT NOT NULL CHECK(length(sealed_commit) IN (40, 64) AND sealed_commit = lower(sealed_commit)),
    registered_common_git_dir TEXT NOT NULL CHECK(registered_common_git_dir <> ''),
    registered_default_branch TEXT NOT NULL CHECK(registered_default_branch <> ''),
    default_head_at_validation TEXT NOT NULL CHECK(length(default_head_at_validation) IN (40, 64) AND default_head_at_validation = lower(default_head_at_validation)),
    ancestry_validation TEXT NOT NULL CHECK(ancestry_validation <> ''),
    attested_by_driver_id TEXT NOT NULL CHECK(attested_by_driver_id <> ''),
    evidence_validated_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(task_id, attempt_id, run_generation, done_message_id, checkpoint_revision)
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

CREATE TABLE plans (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    driver_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(driver_id, name)
);

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

CREATE TABLE report_recovery_attestations (
    id TEXT PRIMARY KEY,
    schema_version INTEGER NOT NULL CHECK(schema_version = 1),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    attempt_id TEXT NOT NULL UNIQUE REFERENCES attempts(id),
    repo_id TEXT NOT NULL REFERENCES repos(id),
    run_generation INTEGER NOT NULL,
    checkpoint_revision INTEGER NOT NULL,
    checkpoint_source_cursor INTEGER NOT NULL,
    checkpoint_session_id TEXT NOT NULL,
    checkpoint_branch TEXT NOT NULL,
    checkpoint_head_commit TEXT NOT NULL,
    checkpoint_worktree_dirty INTEGER NOT NULL CHECK(checkpoint_worktree_dirty IN (0, 1)),
    checkpoint_workspace_facts_error TEXT NOT NULL CHECK(checkpoint_workspace_facts_error = ''),
    workspace_backend TEXT NOT NULL CHECK(workspace_backend = 'native_git_worktree'),
    workspace_state TEXT NOT NULL CHECK(workspace_state = 'held'),
    worktree_path TEXT NOT NULL,
    worktree_git_dir TEXT NOT NULL,
    worktree_common_dir TEXT NOT NULL,
    canonical_report_path TEXT NOT NULL,
    file_identity TEXT NOT NULL,
    file_mode INTEGER NOT NULL,
    file_mod_time_unix_nano INTEGER NOT NULL,
    sha256 TEXT NOT NULL CHECK(length(sha256) = 64 AND sha256 = lower(sha256)),
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
    reason TEXT NOT NULL CHECK(reason <> ''),
    validation_kind TEXT NOT NULL,
    attested_by_driver_id TEXT NOT NULL CHECK(attested_by_driver_id <> ''),
    evidence_validated_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(task_id, attempt_id, run_generation, checkpoint_revision, checkpoint_source_cursor)
);

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

CREATE TABLE "schema_migrations" (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			checksum TEXT NOT NULL DEFAULT '',
			applied_at TEXT NOT NULL
		);

CREATE TABLE task_report_inputs (
    target_task_id TEXT NOT NULL REFERENCES tasks(id),
    position INTEGER NOT NULL DEFAULT 1 CHECK(position > 0),
    artifact_id TEXT NOT NULL REFERENCES verified_artifacts(id),
    attached_by_driver_id TEXT NOT NULL,
    attached_at TEXT NOT NULL,
    PRIMARY KEY(target_task_id, artifact_id),
    UNIQUE(target_task_id, position)
);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    repo_id TEXT NOT NULL REFERENCES repos(id),
    feature_key TEXT NOT NULL,
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
    discard_authorized INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL, remote_delivery_state TEXT NOT NULL DEFAULT 'unverified', archived_at TEXT, title TEXT NOT NULL DEFAULT '', completion_provenance TEXT NOT NULL DEFAULT ''
    CHECK(completion_provenance IN ('', 'worker_done', 'driver_report_recovery')),
    UNIQUE(repo_id, feature_key)
);

CREATE TABLE verified_artifacts (
    id TEXT PRIMARY KEY,
    producer_task_id TEXT NOT NULL REFERENCES tasks(id),
    producer_attempt_id TEXT NOT NULL REFERENCES attempts(id),
    done_message_id INTEGER NOT NULL UNIQUE REFERENCES messages(id),
    kind TEXT NOT NULL CHECK(kind = 'report'),
    original_ref TEXT NOT NULL,
    sha256 TEXT NOT NULL CHECK(length(sha256) = 64 AND sha256 = lower(sha256)),
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
    snapshot_path TEXT NOT NULL,
    verified_at TEXT NOT NULL
, report_recovery_id TEXT REFERENCES report_recovery_attestations(id), accepted_event_id TEXT NOT NULL DEFAULT '', accepted_event_name TEXT NOT NULL DEFAULT '', accepted_event_version INTEGER NOT NULL DEFAULT 0);

CREATE UNIQUE INDEX annotations_item_rev ON annotations(plan_id, plan_item_id, revision) WHERE plan_item_id IS NOT NULL;

CREATE UNIQUE INDEX annotations_task_rev ON annotations(task_id, revision) WHERE task_id IS NOT NULL;

CREATE INDEX attempt_checkpoints_revision_idx ON attempt_checkpoints(run_generation, revision);

CREATE INDEX attempts_task_idx ON attempts(task_id, number);

CREATE INDEX driver_notifications_attempt_idx ON driver_notifications(attempt_id, worker_run_generation, state);

CREATE INDEX driver_notifications_claim_idx ON driver_notifications(state, claim_until);

CREATE INDEX driver_notifications_order_idx ON driver_notifications(target_driver_id, state, created_at, message_id);

CREATE UNIQUE INDEX driver_notifications_settled_run_idx
ON driver_notifications(attempt_id, worker_run_generation)
WHERE kind='settled';

CREATE INDEX driver_notifications_task_idx ON driver_notifications(task_id, target_driver_id, state, created_at, message_id);

CREATE INDEX external_delivery_attestations_pr_idx ON external_delivery_attestations(remote_host, remote_repository, pr_number);

CREATE INDEX external_delivery_attestations_task_idx ON external_delivery_attestations(task_id, attempt_id);

CREATE INDEX local_delivery_recoveries_task_idx ON local_delivery_recoveries(task_id, attempt_id);

CREATE INDEX messages_attempt_generation_idx ON messages(attempt_id, run_generation, source_cursor, id);

CREATE INDEX notification_delivery_log_target_idx ON notification_delivery_log(target_driver_id, id);

CREATE INDEX plan_items_plan_idx ON plan_items(plan_id, position);

CREATE INDEX plan_prerequisites_prerequisite_idx ON plan_prerequisites(prerequisite_item_id, item_id);

CREATE INDEX plan_report_inputs_artifact_idx ON plan_report_inputs(artifact_id);

CREATE INDEX plan_report_inputs_item_idx ON plan_report_inputs(item_id, position);

CREATE INDEX plans_driver_idx ON plans(driver_id, created_at);

CREATE INDEX report_lifecycle_invocations_state_idx ON report_lifecycle_invocations(state, deadline_at);

CREATE INDEX report_lifecycle_invocations_task_idx ON report_lifecycle_invocations(task_id, attempt_id, position);

CREATE INDEX report_recovery_attestations_task_idx ON report_recovery_attestations(task_id, attempt_id);

CREATE INDEX task_report_inputs_artifact_idx ON task_report_inputs(artifact_id);

CREATE INDEX tasks_archive_idx ON tasks(archived_at, created_at);

CREATE INDEX tasks_status_idx ON tasks(status, updated_at);

CREATE UNIQUE INDEX verified_artifacts_accepted_event_idx ON verified_artifacts(accepted_event_id) WHERE accepted_event_id <> '';

CREATE INDEX verified_artifacts_producer_idx ON verified_artifacts(producer_task_id, producer_attempt_id);

CREATE UNIQUE INDEX verified_artifacts_report_recovery_idx ON verified_artifacts(report_recovery_id) WHERE report_recovery_id IS NOT NULL;

CREATE TRIGGER annotations_immutable_delete
BEFORE DELETE ON annotations
BEGIN
    SELECT RAISE(ABORT, 'annotations are immutable');
END;

CREATE TRIGGER annotations_immutable_update
BEFORE UPDATE ON annotations
BEGIN
    SELECT RAISE(ABORT, 'annotations are immutable');
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

CREATE TRIGGER external_delivery_attestations_immutable_delete
BEFORE DELETE ON external_delivery_attestations
BEGIN
    SELECT RAISE(ABORT, 'external delivery attestations are immutable');
END;

CREATE TRIGGER external_delivery_attestations_immutable_update
BEFORE UPDATE ON external_delivery_attestations
BEGIN
    SELECT RAISE(ABORT, 'external delivery attestations are immutable');
END;

CREATE TRIGGER local_delivery_recoveries_immutable_delete
BEFORE DELETE ON local_delivery_recoveries
BEGIN
    SELECT RAISE(ABORT, 'local delivery recoveries are immutable');
END;

CREATE TRIGGER local_delivery_recoveries_immutable_update
BEFORE UPDATE ON local_delivery_recoveries
BEGIN
    SELECT RAISE(ABORT, 'local delivery recoveries are immutable');
END;

CREATE TRIGGER plan_items_dispatch_immutable
BEFORE UPDATE OF dispatched_task_id ON plan_items
WHEN OLD.dispatched_task_id IS NOT NULL OR NEW.dispatched_task_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'plan item dispatch identity is immutable');
END;

CREATE TRIGGER plan_items_dispatched_delete
BEFORE DELETE ON plan_items
WHEN OLD.dispatched_task_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'dispatched plan items cannot be deleted');
END;

CREATE TRIGGER plan_items_dispatched_scope_immutable
BEFORE UPDATE OF title, feature_key, objective, description, acceptance_criteria, repo_id, deliverable ON plan_items
WHEN OLD.dispatched_task_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'dispatched plan item scope is immutable');
END;

CREATE TRIGGER plan_items_list_immutable
BEFORE UPDATE OF plan_id ON plan_items
BEGIN
    SELECT RAISE(ABORT, 'plan item plan identity is immutable');
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

CREATE TRIGGER plan_prerequisites_validate_delete
BEFORE DELETE ON plan_prerequisites
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM plan_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched plan item prerequisites are immutable') END;
    SELECT CASE WHEN EXISTS(
        SELECT 1 FROM plan_report_inputs WHERE item_id=OLD.item_id AND prerequisite_item_id=OLD.prerequisite_item_id
    ) THEN RAISE(ABORT, 'remove the plan report input before its prerequisite') END;
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

CREATE TRIGGER plan_report_inputs_validate_delete
BEFORE DELETE ON plan_report_inputs
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM plan_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched plan item report inputs are immutable') END;
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

CREATE TRIGGER report_lifecycle_invocations_delete
BEFORE DELETE ON report_lifecycle_invocations
BEGIN
    SELECT RAISE(ABORT, 'report lifecycle invocations are durable');
END;

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

CREATE TRIGGER report_recovery_attestations_immutable_delete
BEFORE DELETE ON report_recovery_attestations
BEGIN
    SELECT RAISE(ABORT, 'report recovery attestations are immutable');
END;

CREATE TRIGGER report_recovery_attestations_immutable_update
BEFORE UPDATE ON report_recovery_attestations
BEGIN
    SELECT RAISE(ABORT, 'report recovery attestations are immutable');
END;

CREATE TRIGGER task_report_inputs_immutable_delete
BEFORE DELETE ON task_report_inputs
BEGIN
    SELECT RAISE(ABORT, 'task report inputs are immutable');
END;

CREATE TRIGGER task_report_inputs_immutable_update
BEFORE UPDATE ON task_report_inputs
BEGIN
    SELECT RAISE(ABORT, 'task report inputs are immutable');
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

CREATE TRIGGER verified_artifacts_immutable_delete
BEFORE DELETE ON verified_artifacts
BEGIN
    SELECT RAISE(ABORT, 'verified artifacts are immutable');
END;

CREATE TRIGGER verified_artifacts_immutable_update
BEFORE UPDATE ON verified_artifacts
BEGIN
    SELECT RAISE(ABORT, 'verified artifacts are immutable');
END;

CREATE VIEW attempt_landing_projections AS
SELECT
    proof.attempt_id,
    proof.task_id,
    proof.landed,
    CASE WHEN proof.landed = 1 THEN proof.landing_reason ELSE '' END AS landed_reason
FROM (
    SELECT
        a.id AS attempt_id,
        a.task_id AS task_id,
        a.landing_reason,
        CASE WHEN
            a.landed_proven = 1
            AND a.landing_kind <> ''
            AND a.landed_verified_at IS NOT NULL
            AND (
                (
                    a.landing_kind = 'report_artifact'
                    AND EXISTS (
                        SELECT 1
                        FROM verified_artifacts va
                        JOIN messages m ON m.id = va.done_message_id
                        LEFT JOIN report_recovery_attestations rr ON rr.id = va.report_recovery_id
                        WHERE va.producer_task_id = a.task_id
                          AND va.producer_attempt_id = a.id
                          AND va.kind = 'report'
                          AND m.task_id = a.task_id
                          AND m.attempt_id = a.id
                          AND m.run_generation = a.run_generation
                          AND m.stale = 0
                          AND m.artifact_ref = va.original_ref
                          AND (
                              (
                                  va.report_recovery_id IS NULL
                                  AND m.direction = 'worker-to-driver'
                                  AND m.type = 'done'
                              )
                              OR (
                                  va.report_recovery_id IS NOT NULL
                                  AND m.direction = 'system'
                                  AND m.type = 'report-recovery'
                                  AND rr.task_id = a.task_id
                                  AND rr.attempt_id = a.id
                                  AND rr.run_generation = a.run_generation
                                  AND rr.checkpoint_revision = a.landed_checkpoint_revision
                                  AND rr.sha256 = va.sha256
                                  AND rr.size_bytes = va.size_bytes
                                  AND 'report:' || rr.canonical_report_path = va.original_ref
                              )
                          )
                    )
                )
                OR (
                    a.landing_kind IN ('local_default_branch', 'github_pr')
                    AND a.landed_source_commit <> ''
                    AND a.landed_target_ref <> ''
                    AND a.landed_target_commit <> ''
                    AND a.landed_checkpoint_revision > 0
                )
                OR (
                    a.landing_kind = 'github_pr_attested_ancestry'
                    AND a.landed_source_commit <> ''
                    AND a.landed_target_ref <> ''
                    AND a.landed_target_commit <> ''
                    AND a.landed_checkpoint_revision > 0
                    AND EXISTS (
                        SELECT 1
                        FROM external_delivery_attestations e
                        WHERE e.task_id = a.task_id
                          AND e.attempt_id = a.id
                          AND e.run_generation = a.run_generation
                          AND e.sealed_commit = a.landed_source_commit
                          AND 'refs/heads/' || e.registered_default_branch = a.landed_target_ref
                          AND e.merge_commit = a.landed_target_commit
                          AND e.checkpoint_revision = a.landed_checkpoint_revision
                    )
                )
                OR (
                    a.landing_kind = 'local_default_branch_attested_ancestry'
                    AND a.landed_source_commit <> ''
                    AND a.landed_target_ref <> ''
                    AND a.landed_target_commit <> ''
                    AND a.landed_checkpoint_revision > 0
                    AND EXISTS (
                        SELECT 1
                        FROM local_delivery_recoveries l
                        WHERE l.task_id = a.task_id
                          AND l.attempt_id = a.id
                          AND l.run_generation = a.run_generation
                          AND l.sealed_commit = a.landed_source_commit
                          AND 'refs/heads/' || l.registered_default_branch = a.landed_target_ref
                          AND l.checkpoint_revision = a.landed_checkpoint_revision
                    )
                )
            )
        THEN 1 ELSE 0 END AS landed
    FROM attempts a
) proof;

CREATE VIEW task_landing_projections AS
SELECT
    t.id AS task_id,
    COALESCE(p.landed, 0) AS landed,
    CASE WHEN COALESCE(p.landed, 0) = 1 THEN p.landed_reason ELSE '' END AS landed_reason
FROM tasks t
LEFT JOIN attempt_landing_projections p
    ON p.attempt_id = t.current_attempt_id AND p.task_id = t.id;

CREATE TABLE schema_baseline_provenance (
    baseline_version INTEGER NOT NULL,
    baseline_name TEXT NOT NULL,
    baseline_checksum TEXT NOT NULL CHECK(length(baseline_checksum) = 64 AND baseline_checksum = lower(baseline_checksum)),
    original_version INTEGER NOT NULL DEFAULT 0,
    old_ledger_digest TEXT NOT NULL DEFAULT '' CHECK(length(old_ledger_digest) IN (0, 64) AND (old_ledger_digest = '' OR old_ledger_digest = lower(old_ledger_digest))),
    baseline_released_at TEXT NOT NULL,
    legacy_export_closes_at TEXT NOT NULL,
    upgraded_at TEXT NOT NULL
);
