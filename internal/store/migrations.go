package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrations embed.FS

//go:embed baseline_v29.sql
var baselineBody string

// The baseline is the current schema contract. Fresh databases execute the
// consolidated body once; databases at the supported bridge versions 15
// through 28 are upgraded by a one-shot migrator that converges on the same
// schema objects and the same single ledger identity. Schemas below the
// minimum upgradable version are read-only export candidates for the bounded
// legacy window, never in-place upgrade candidates.
const (
	baselineVersion      = 29
	bridgeFloorVersion   = 15
	baselineName         = "029_consolidated_baseline"
	baselineReleasedAt   = "2026-08-18T15:35:00Z"
	legacyExportClosesAt = "2026-09-17T15:35:00Z"
)

func baselineChecksum() string {
	sum := sha256.Sum256([]byte(baselineBody))
	return hex.EncodeToString(sum[:])
}

// legacyTaskTables are the duplicate task-list tables the historical
// migration 9 preserved from the abandoned v6/v7 schema. They carry no live
// rows in any supported database and are dropped by the baseline migrator
// once exported.
var legacyTaskTables = []string{"legacy_task_lists", "legacy_task_list_items", "legacy_task_list_prerequisites", "legacy_task_list_item_inputs", "legacy_task_list_events"}

// frozenMigrationIdentity is the released name and checksum of a migration
// this binary can no longer execute. Identities are frozen at release: a
// recorded version number can never again mask incompatible SQL, and the
// bodies themselves must not be edited or relabeled after release.
type frozenMigrationIdentity struct {
	name     string
	checksum string
}

var frozenMigrationIdentities = map[int]frozenMigrationIdentity{
	1:  {"001_current", "62d6bd2991767aa0c0dc9f903f4d1e92eda2d9a385ef5fcd872d9c2feef7cc52"},
	2:  {"002_verified_report_inputs", "6333f2e1779f0125fa0a227bb99625905de38d61a5138ae846830c8935ceaac3"},
	3:  {"003_local_landing", "b5160c20ff3ef32a7dcff1531d57478d2c9ef411e0c094a68fbceda234195e82"},
	4:  {"004_task_archive", "4ebbf5966a765c66599f0605a1f61e8d1947f319ae3480607ec79cad53bb3060"},
	5:  {"005_runtime_executable", "b8ca2f9873ccf8211e99519c47ed4a8df7b272ed6ca32c87e7b566028e702b97"},
	6:  {"006_task_lists", "792d1906d9f3d82c83678823567c52914d61c7689d575a58ff56b745f1242eb6"},
	7:  {"007_report_input_position_default", "9554c1397092d50cba1d4d08446b6fef180e7963ef7eee69e6a6c0358c433ecf"},
	8:  {"008_external_delivery_attestations", "ff89c95114843930c67b6c19ab727ccdee645512e9674b5cec23d745b797ce38"},
	9:  {"009_lifecycle_integrity_repair", "891d29ccfd1ed8f70061e5d79ee1b31ae99049418b95cb5ba8145d598f524ace"},
	10: {"010_task_report_inputs_default", "7bdb7bcf7cc07dcedb494dfe0eb911607f1623bd666e1750fc9ead4b6378cfc5"},
	11: {"011_local_delivery_recoveries", "b80da7cb38649a556dea744b3d1a41f6c92396a55bbe79b7d5e241773f464cb5"},
	12: {"012_native_worktree_identity", "2f2ad6b1bee2f4591017bcea84c238e032ccdc467c8fab31601cc41418f8899e"},
	13: {"013_authoritative_landing_projection", "93baf6556029751d3682408c96ca4d07b6637090edf1a0510bb9f98b8b7d9487"},
	14: {"014_drop_task_landing_duplicates", "989a04fd4e9ca4011dcd932e0aca23424b8ff03f13b497a7a0cab963ffb33b29"},
	15: {"015_treehouse_retirement_gate", "659a62026877f88ec7be1437c7cf36b2a2047c99044fccc5bb13c25eda21d68c"},
	16: {"016_drop_repository_memory", "2dd72620e1266c5abd737c919e180c18a263e82d26c631dd3f75db066805ad2d"},
	17: {"017_driver_decisions", "fa78aaf1209ceec7e127133fb8a227bc10f0c1c8268f0b35db4a811c38de1b55"},
	18: {"018_terminal_endpoints", "275158aff01b36a92df2706bbc4dadf6f9d6723107218fd30b051353edc4f40c"},
	19: {"019_terminal_provider_diagnostics", "e803fca9157f89f58546e613ebd7615804276d685ec5850e3e8813029deb51d0"},
	20: {"020_task_titles", "a3b5cc54bcd18835bab546e6f9c826ac68f2d585a3ad8a2cf0222a8b6f05af6c"},
	21: {"021_report_recovery", "d3917e84b31270300cc22e22a092d7cbd6973f2190b396db304e7f07544d7679"},
	22: {"022_report_accepted_lifecycle", "d1feef6bcac1e201529f1c9115d795f83691fe5b9948d5f8bafa0c0aa42a12be"},
	23: {"023_drop_task_correlation_group", "d62cbe1553ae83e9f573dfff77ca5e41b87327562ac1bd170c3db3c530c840bb"},
	24: {"024_settlement_notification", "acd866bedebe9120c2b031e1146f0faf730035d717b8e69dfa80a84c60277098"},
	25: {"025_external_delivery_evidence_digest", "c4cdfff0515fe8808df31e2fb1bf1afa37a731a9ee212cf2fee3837ec2a9a336"},
	26: {"026_terminal_create_intent", "32688236c30ea6a4b2a4f39e229db1806c00aeba7d726df79ec3d24a04a636d2"},
	27: {"027_plan_surface", "32ed3b1c29025f6cb59e8033e9f4f4a931e0e98529a735d144d0f63dfac1aca7"},
	28: {"028_consolidated_baseline", "e9686cf907fa2c051bf53c8d6eeeee49e846c7c53c7dbed9569b6fd22b9ee552"},
}

// bridgeMigration is one executable step of the temporary compatibility bridge.
// Versions 1-15 are frozen: their bodies are no longer embedded in
// production and a database that early is refused for in-place upgrade.
type bridgeMigration struct {
	version            int
	name               string
	sql                string
	run                func(ctx context.Context, conn *sql.Conn) error
	identity           string
	disableForeignKeys bool
}

func (m bridgeMigration) checksum() string {
	body := m.sql
	if m.run != nil {
		body = m.identity
	}
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// loadBridgeMigrations returns the executable bridge for versions 16-27.
// Version 15 is a frozen gate with no executable body: databases that reach
// the bridge already recorded it, and the baseline preconditions re-assert
// its release gate (every Treehouse attempt fully released).
func loadBridgeMigrations() ([]bridgeMigration, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded bridge migrations: %w", err)
	}
	registry := make([]bridgeMigration, 0, len(entries)+2)
	for _, entry := range entries {
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("invalid bridge migration filename %q", entry.Name())
		}
		body, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read bridge migration %d: %w", version, err)
		}
		registry = append(registry, bridgeMigration{version: version, name: strings.TrimSuffix(entry.Name(), ".sql"), sql: string(body)})
	}
	registry = append(registry,
		bridgeMigration{version: 21, name: "021_report_recovery", run: migrateReportRecoveryV21, identity: migration21SQL},
		bridgeMigration{version: 27, name: frozenMigrationIdentities[27].name, run: migratePlanSurfaceV27, identity: migration27SQL, disableForeignKeys: true},
	)
	sort.Slice(registry, func(i, j int) bool { return registry[i].version < registry[j].version })
	for index, entry := range registry {
		if index == 0 && entry.version != 16 {
			return nil, fmt.Errorf("bridge migrations must start at version 16; found %d", entry.version)
		}
		if index > 0 && entry.version != registry[index-1].version+1 {
			return nil, fmt.Errorf("bridge migrations must be contiguous; gap before version %d", entry.version)
		}
	}
	return registry, nil
}

type recordedMigration struct {
	name     string
	checksum string
}

// validateBridgeLedger verifies the complete recorded history against the
// frozen identities. Versions 1-8 may carry an empty checksum (binaries that
// predate identity recording never backfilled them); everything else must
// match the frozen release exactly.
func validateBridgeLedger(recorded map[int]recordedMigration, maxVersion int) error {
	if maxVersion == 28 && len(recorded) == 1 {
		record := recorded[28]
		if record.name == frozenMigrationIdentities[28].name && record.checksum == frozenMigrationIdentities[28].checksum {
			return nil
		}
	}
	versions := make([]int, 0, len(recorded))
	for version := range recorded {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	for index, version := range versions {
		if version != index+1 {
			return fmt.Errorf("database schema history is not contiguous: version %d is missing; refusing to open", version)
		}
	}
	if len(versions) != maxVersion {
		return fmt.Errorf("database schema history records %d versions but the maximum is %d; refusing to open", len(versions), maxVersion)
	}
	for version := 1; version <= maxVersion; version++ {
		record := recorded[version]
		if version < baselineVersion {
			frozen := frozenMigrationIdentities[version]
			if record.name != frozen.name {
				return fmt.Errorf("database migration %d identity is %q but the released identity is %q; a recorded version number cannot substitute for compatible SQL, refusing to open", version, record.name, frozen.name)
			}
			if record.checksum != "" && record.checksum != frozen.checksum {
				return fmt.Errorf("database migration %d (%s) was applied with checksum %s but the released identity is %s; refusing to open an incompatibly migrated database", version, frozen.name, record.checksum, frozen.checksum)
			}
			if version >= 9 && record.name == "" {
				return fmt.Errorf("database migration %d is recorded without its mandatory identity; refusing to trust the bare version number", version)
			}
			continue
		}
		if record.name != baselineName || record.checksum != baselineChecksum() {
			return fmt.Errorf("database migration %d identity is %q/%s but the baseline identity is %q; refusing to open an incompatibly migrated database", version, record.name, record.checksum, baselineName)
		}
	}
	return nil
}

// baselineShape is the complete schema-object contract of the baseline: every
// object the consolidated body creates, plus the exact column order of the
// tables that must survive the bridge byte-for-byte.
var baselineObjectContract = map[string][]string{
	"annotations":                    {"id", "driver_id", "task_id", "plan_id", "plan_item_id", "revision", "judgment", "reason", "next_action", "created_at"},
	"attempt_checkpoints":            {"attempt_id", "revision", "schema_version", "producer", "run_generation", "source_cursor", "session_id", "branch", "head_commit", "worktree_dirty", "workspace_facts_error", "summary", "completed_json", "next_steps_json", "decisions_json", "changed_paths_json", "checks_json", "blockers_json", "source_attempt_id", "source_revision", "captured_at"},
	"attempts":                       {"id", "task_id", "number", "harness", "model", "runtime_backend", "runtime_generation", "run_generation", "resume_source_attempt_id", "resume_source_revision", "session_id", "worktree_path", "lease_id", "branch", "status", "runner_pid", "cursor", "exit_code", "failure_reason", "landed_proven", "discard_authorized", "released_at", "created_at", "updated_at", "ended_at", "base_commit", "landing_kind", "landed_source_commit", "landed_target_ref", "landed_target_commit", "landed_checkpoint_revision", "landed_verified_at", "release_state", "release_claimed_at", "release_owner_pid", "release_reason", "runtime_executable", "landing_quarantine_reason", "workspace_backend", "workspace_state", "intended_worktree_path", "worktree_git_dir", "worktree_common_dir", "workspace_state_changed_at", "landing_reason", "terminal_socket_path", "terminal_window_id", "terminal_workspace_id", "terminal_tab_id", "terminal_pane_id", "terminal_surface_id", "terminal_provider_version", "terminal_protocol_version", "terminal_capabilities_json", "terminal_create_state", "terminal_create_run_generation", "terminal_create_backend", "terminal_create_source", "terminal_create_window_id", "terminal_create_workspace_id", "terminal_create_cwd", "terminal_create_label", "terminal_create_generation", "base_strategy", "base_ref"},
	"driver_notifications":           {"notification_id", "message_id", "task_id", "attempt_id", "target_driver_id", "worker_run_generation", "source_cursor", "kind", "state", "created_at", "updated_at", "claim_owner", "driver_generation", "claim_token", "claimed_at", "claim_until", "delivery_attempts", "acked_at", "ack_owner", "ack_driver_generation", "handling_id", "superseded_at", "supersede_reason"},
	"external_delivery_attestations": {"id", "schema_version", "task_id", "attempt_id", "repo_id", "run_generation", "done_message_id", "checkpoint_revision", "original_artifact_ref", "sealed_commit", "provider", "remote_host", "remote_repository", "pr_number", "pr_node_id", "pr_url", "registered_default_branch", "pr_base_ref", "pr_head_ref", "pr_head_commit", "merge_commit", "merged_at", "default_head_at_validation", "graph_validation", "attested_by_driver_id", "evidence_validated_at", "created_at", "evidence_digest"},
	"local_delivery_recoveries":      {"id", "schema_version", "task_id", "attempt_id", "repo_id", "run_generation", "done_message_id", "checkpoint_revision", "original_artifact_ref", "attempt_branch", "sealed_commit", "registered_common_git_dir", "registered_default_branch", "default_head_at_validation", "ancestry_validation", "attested_by_driver_id", "evidence_validated_at", "created_at"},
	"messages":                       {"id", "task_id", "attempt_id", "direction", "type", "payload", "artifact_ref", "stale", "wake", "source_cursor", "run_generation", "checkpoint_json", "created_at"},
	"notification_delivery_log":      {"id", "notification_id", "target_driver_id", "operation", "consumer_id", "driver_generation", "claim_token", "handling_id", "result", "detail", "created_at"},
	"plan_items":                     {"id", "plan_id", "position", "feature_key", "title", "objective", "description", "acceptance_criteria", "repo_id", "deliverable", "dispatched_task_id", "created_at", "updated_at"},
	"plan_prerequisites":             {"item_id", "prerequisite_item_id", "created_at"},
	"plan_report_inputs":             {"item_id", "prerequisite_item_id", "position", "artifact_id", "selected_by_driver_id", "selected_at", "created_at", "updated_at"},
	"plans":                          {"id", "name", "driver_id", "created_at", "updated_at"},
	"report_lifecycle_invocations":   {"id", "event_id", "event_name", "event_version", "artifact_id", "task_id", "attempt_id", "handler_name", "extension_id", "configuration_hash", "position", "state", "attempts", "claim_token", "annotation", "receipt_system", "receipt_id", "failure_kind", "failure_message", "effect", "started_at", "deadline_at", "completed_at", "created_at", "updated_at"},
	"report_recovery_attestations":   {"id", "schema_version", "task_id", "attempt_id", "repo_id", "run_generation", "checkpoint_revision", "checkpoint_source_cursor", "checkpoint_session_id", "checkpoint_branch", "checkpoint_head_commit", "checkpoint_worktree_dirty", "checkpoint_workspace_facts_error", "workspace_backend", "workspace_state", "worktree_path", "worktree_git_dir", "worktree_common_dir", "canonical_report_path", "file_identity", "file_mode", "file_mod_time_unix_nano", "sha256", "size_bytes", "reason", "validation_kind", "attested_by_driver_id", "evidence_validated_at", "created_at"},
	"repos":                          {"id", "name", "path", "default_branch", "context_file", "setup_hook", "created_at", "updated_at"},
	"schema_migrations":              {"version", "name", "checksum", "applied_at"},
	"schema_baseline_provenance":     {"baseline_version", "baseline_name", "baseline_checksum", "original_version", "old_ledger_digest", "baseline_released_at", "legacy_export_closes_at", "upgraded_at"},
	"task_report_inputs":             {"target_task_id", "position", "artifact_id", "attached_by_driver_id", "attached_at"},
	"tasks":                          {"id", "repo_id", "feature_key", "driver_id", "objective", "acceptance_criteria", "deliverable", "status", "current_attempt_id", "artifact_ref", "claimed_done", "process_alive", "branch_pushed", "pr_state", "discard_authorized", "created_at", "updated_at", "remote_delivery_state", "archived_at", "title", "completion_provenance"},
	"verified_artifacts":             {"id", "producer_task_id", "producer_attempt_id", "done_message_id", "kind", "original_ref", "sha256", "size_bytes", "snapshot_path", "verified_at", "report_recovery_id", "accepted_event_id", "accepted_event_name", "accepted_event_version"},
}

var baselineIndexContract = []string{
	"annotations_item_rev", "annotations_task_rev", "attempt_checkpoints_revision_idx", "attempts_task_idx",
	"driver_notifications_attempt_idx", "driver_notifications_claim_idx", "driver_notifications_order_idx",
	"driver_notifications_settled_run_idx", "driver_notifications_task_idx", "external_delivery_attestations_pr_idx",
	"external_delivery_attestations_task_idx", "local_delivery_recoveries_task_idx", "messages_attempt_generation_idx",
	"notification_delivery_log_target_idx", "plan_items_plan_idx", "plan_prerequisites_prerequisite_idx",
	"plan_report_inputs_artifact_idx", "plan_report_inputs_item_idx", "plans_driver_idx",
	"report_lifecycle_invocations_state_idx", "report_lifecycle_invocations_task_idx", "report_recovery_attestations_task_idx",
	"task_report_inputs_artifact_idx", "tasks_archive_idx", "tasks_status_idx", "verified_artifacts_accepted_event_idx",
	"verified_artifacts_producer_idx", "verified_artifacts_report_recovery_idx",
}

var baselineTriggerContract = []string{
	"annotations_immutable_delete", "annotations_immutable_update", "annotations_item_scope_insert", "annotations_revision_insert",
	"external_delivery_attestations_immutable_delete", "external_delivery_attestations_immutable_update",
	"local_delivery_recoveries_immutable_delete", "local_delivery_recoveries_immutable_update",
	"plan_items_dispatch_immutable", "plan_items_dispatched_delete", "plan_items_dispatched_scope_immutable",
	"plan_items_list_immutable", "plan_items_title_required_insert", "plan_items_title_required_update",
	"plan_prerequisites_validate_delete", "plan_prerequisites_validate_insert", "plan_prerequisites_validate_update",
	"plan_report_inputs_validate_delete", "plan_report_inputs_validate_insert", "plan_report_inputs_validate_update",
	"report_lifecycle_invocations_delete", "report_lifecycle_invocations_identity_insert", "report_lifecycle_invocations_identity_update",
	"report_recovery_attestations_immutable_delete", "report_recovery_attestations_immutable_update",
	"task_report_inputs_immutable_delete", "task_report_inputs_immutable_update",
	"tasks_title_required_insert", "tasks_title_required_update",
	"verified_artifacts_immutable_delete", "verified_artifacts_immutable_update",
}

var baselineViewContract = []string{"attempt_landing_projections", "task_landing_projections"}

var baselineForbiddenTables = append([]string{"memory", "task_lists", "task_list_items", "task_list_prerequisites", "task_list_inputs", "driver_decisions"}, legacyTaskTables...)

var baselineForbiddenColumns = map[string][]string{
	"tasks":    {"landed", "landed_reason", "group_id"},
	"attempts": {"herdr_socket_path", "herdr_workspace_id", "herdr_tab_id", "herdr_pane_id"},
}

func sortedKeys(set map[string][]string) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
