package store

import (
	"strings"
	"testing"
)

// TestEvidenceDigestMigrationRejectsMalformedDigests proves the released
// evidence-digest bridge step: a database at the source version carries its
// attestations into the baseline with the released default, and the baseline
// CHECK constraint rejects malformed digests on new writes.
func TestEvidenceDigestMigrationRejectsMalformedDigests(t *testing.T) {
	path := t.TempDir() + "/state.db"
	legacy := buildLegacyDBAt(t, path, 24)
	statements := []string{
		`INSERT INTO repos(id, name, path, default_branch, created_at, updated_at)
			VALUES('repo_digest', 'digest-migration', '/tmp/digest', 'main', '2026-08-18T00:00:00Z', '2026-08-18T00:00:00Z')`,
		`INSERT INTO tasks(id, repo_id, feature_key, driver_id, objective, deliverable, status, current_attempt_id, title, created_at, updated_at)
			VALUES('task_digest', 'repo_digest', 'digest-migration', 'driver:test', 'exercise migration', 'code', 'done', 'attempt_digest', 'Digest migration', '2026-08-18T00:00:00Z', '2026-08-18T00:00:00Z')`,
		`INSERT INTO attempts(id, task_id, number, harness, status, branch, release_state, created_at, updated_at)
			VALUES('attempt_digest', 'task_digest', 1, 'pi', 'done', 'shephrd/digest-migration', 'held', '2026-08-18T00:00:00Z', '2026-08-18T00:00:00Z')`,
		`INSERT INTO messages(task_id, attempt_id, direction, type, payload, artifact_ref, source_cursor, run_generation, created_at)
			VALUES('task_digest', 'attempt_digest', 'worker-to-driver', 'done', 'done', 'branch:shephrd/digest-migration', 1, 1, '2026-08-18T00:00:00Z')`,
	}
	for _, statement := range statements {
		if _, err := legacy.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	var doneMessageID int
	if err := legacy.QueryRow(`SELECT id FROM messages WHERE attempt_id='attempt_digest'`).Scan(&doneMessageID); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO external_delivery_attestations(
		id, schema_version, task_id, attempt_id, repo_id, run_generation, done_message_id, checkpoint_revision,
		original_artifact_ref, sealed_commit, provider, remote_host, remote_repository, pr_number, pr_node_id,
		pr_url, registered_default_branch, pr_base_ref, pr_head_ref, pr_head_commit, merge_commit, merged_at,
		default_head_at_validation, graph_validation, attested_by_driver_id, evidence_validated_at, created_at)
		VALUES(?, 1, ?, ?, ?, ?, ?, 1, 'branch:shephrd/digest-migration', ?, 'github', 'github.com', 'acme/digest-migration', 1,
		'node-1', 'https://github.com/acme/digest-migration/pull/1', 'main', 'main', 'review', ?, ?, '2026-08-18T00:00:00Z',
		?, 'github-pr-membership+compare-v1', 'driver:test', '2026-08-18T00:00:00Z', '2026-08-18T00:00:00Z')`
	args := []any{"attestation-1", "task_digest", "attempt_digest", "repo_digest", 1, doneMessageID,
		strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40), strings.Repeat("4", 40)}
	if _, err := legacy.Exec(insert, args...); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	var digest string
	if err := state.db.QueryRow(`SELECT evidence_digest FROM external_delivery_attestations WHERE id='attestation-1'`).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest != "" {
		t.Fatalf("migrated default evidence digest = %q", digest)
	}
	insertWithDigest := `INSERT INTO external_delivery_attestations(
		id, schema_version, task_id, attempt_id, repo_id, run_generation, done_message_id, checkpoint_revision,
		original_artifact_ref, sealed_commit, provider, remote_host, remote_repository, pr_number, pr_node_id,
		pr_url, registered_default_branch, pr_base_ref, pr_head_ref, pr_head_commit, merge_commit, merged_at,
		default_head_at_validation, graph_validation, attested_by_driver_id, evidence_validated_at, evidence_digest, created_at)
		VALUES(?, 1, ?, ?, ?, ?, ?, 1, 'branch:shephrd/digest-migration', ?, 'github', 'github.com', 'acme/digest-migration', 1,
		'node-1', 'https://github.com/acme/digest-migration/pull/1', 'main', 'main', 'review', ?, ?, '2026-08-18T00:00:00Z',
		?, 'github-pr-membership+compare-v1', 'driver:test', '2026-08-18T00:00:00Z', ?, '2026-08-18T00:00:00Z')`
	for _, value := range []string{strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		probe := append([]any{"attestation-" + value[:8]}, args[1:]...)
		probe = append(probe, value)
		if _, err := state.db.Exec(insertWithDigest, probe...); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Fatalf("evidence digest %q insert error = %v", value, err)
		}
	}
}
