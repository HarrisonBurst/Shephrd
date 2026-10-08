package control

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

// The live reproduced shape from task_19d6d50a4f9a: exact attempt/checkpoint
// branch, accepted artifact ref truncated by one character, sealed commit
// already merged into local main, lease held.
const (
	recoveryBranch    = "shephrd/task_19d6d50a4f9a"
	recoveryTruncated = "branch:shephrd/task_19d6d50a4f9"
)

// TestStoredLocalRecoveryRemainsVisibleAndIdempotentlyVerified pins the
// retirement boundary: the one immutable local delivery recovery recorded
// before the writer retired stays readable, and a proven, released attempt
// verifies idempotently from its immutable landing proof with the original
// mismatched artifact unchanged.
func TestStoredLocalRecoveryRemainsVisibleAndIdempotentlyVerified(t *testing.T) {
	service, state, task, attempt, databasePath := localRecoveryFixture(t)
	defer state.Close()
	sealed := runLocalGit(t, attempt.WorktreePath, "rev-parse", "HEAD")
	repo, err := state.Repo(task.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	defaultHead := runLocalGit(t, repo.Path, "rev-parse", "HEAD")

	// The writer is retired: the immutable record is inserted the way the
	// released binary once recorded it, then the proof and release follow.
	commonDir := runLocalGit(t, repo.Path, "rev-parse", "--git-common-dir")
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var doneMessageID int64
	if err := raw.QueryRow(`SELECT id FROM messages WHERE attempt_id=? AND type='done' AND stale=0`, attempt.ID).Scan(&doneMessageID); err != nil {
		t.Fatal(err)
	}
	var checkpointRevision int
	if err := raw.QueryRow(`SELECT revision FROM attempt_checkpoints WHERE attempt_id=? ORDER BY revision DESC LIMIT 1`, attempt.ID).Scan(&checkpointRevision); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := raw.Exec(`INSERT INTO local_delivery_recoveries(id, schema_version, task_id, attempt_id, repo_id, run_generation,
		done_message_id, checkpoint_revision, original_artifact_ref, attempt_branch, sealed_commit, registered_common_git_dir,
		registered_default_branch, default_head_at_validation, ancestry_validation, attested_by_driver_id, evidence_validated_at, created_at)
		VALUES(?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"recovery_test", task.ID, attempt.ID, task.RepoID, attempt.RunGeneration,
		doneMessageID, checkpointRevision, recoveryTruncated, recoveryBranch, sealed, commonDir,
		"main", defaultHead, "local-default-ancestry+shared-common-dir-v1", task.DriverID, now, now); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	state.Close()
	if state, err = store.Open(databasePath); err != nil {
		t.Fatal(err)
	}
	task, _ = state.Task(task.ID)
	attempt, _ = state.Attempt(attempt.ID)
	service = New(config.Config{DataDir: t.TempDir(), WorktreeRoot: filepath.Dir(attempt.WorktreePath)}, state)
	service.environment = environmentCapability{get: func(string) string { return "" }}

	recovery, err := state.LocalDeliveryRecoveryForAttempt(attempt.ID)
	if err != nil || recovery == nil || recovery.OriginalArtifactRef != recoveryTruncated || recovery.SealedCommit != sealed || recovery.AttemptBranch != recoveryBranch {
		t.Fatalf("stored recovery = %+v err = %v", recovery, err)
	}
	if _, err := state.RecordLandingProof(attempt.ID, model.LandingProof{TaskID: task.ID, AttemptID: attempt.ID,
		RunGeneration: attempt.RunGeneration, Kind: model.LandingKindLocalAttestedAncestry,
		SourceCommit: sealed, TargetRef: "refs/heads/main", TargetCommit: defaultHead,
		CheckpointRevision: checkpointRevision, VerifiedAt: time.Now().UTC()}, "driver-attested local recovery previously verified"); err != nil {
		t.Fatal(err)
	}
	if err := service.Release(task.ID, attempt.ID, false); err != nil {
		t.Fatal(err)
	}

	// Idempotent verification from the immutable proof: no re-attestation,
	// no artifact rewrite, no worktree churn.
	for pass := 1; pass <= 2; pass++ {
		result, err := service.VerifyLocal(task.ID, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if !result.Landed || result.Landing.Kind != model.LandingKindLocalAttestedAncestry || result.Landing.SourceCommit != sealed {
			t.Fatalf("pass %d result = %+v", pass, result)
		}
		current, _ := state.Task(task.ID)
		if current.ArtifactRef != recoveryTruncated {
			t.Fatalf("verification rewrote the original artifact: %q", current.ArtifactRef)
		}
	}
	stored, _ := state.Attempt(attempt.ID)
	if !stored.LandedProven || stored.LandingKind != model.LandingKindLocalAttestedAncestry || stored.ReleaseState != "released" {
		t.Fatalf("attempt = %+v", stored)
	}
	again, err := state.LocalDeliveryRecoveryForAttempt(attempt.ID)
	if err != nil || again == nil || again.ID != "recovery_test" {
		t.Fatalf("recovery row no longer visible: %+v err = %v", again, err)
	}
}

// TestMismatchedArtifactWithoutRecoveryFailsClosed pins the other side of the
// boundary: with the compatibility command retired, an accepted mismatched
// branch artifact that has no immutable recovery record cannot be verified or
// silently healed; ordinary local verification rejects it and no recovery row
// appears.
func TestMismatchedArtifactWithoutRecoveryFailsClosed(t *testing.T) {
	service, state, task, attempt, _ := localRecoveryFixture(t)
	defer state.Close()
	if _, err := service.VerifyLocal(task.ID, "", ""); err == nil || !strings.Contains(err.Error(), "local landing requires artifact branch:"+recoveryBranch) {
		t.Fatalf("ordinary local verify error = %v", err)
	}
	if recovery, err := state.LocalDeliveryRecoveryForAttempt(attempt.ID); err != nil || recovery != nil {
		t.Fatalf("unexpected recovery row: %+v err = %v", recovery, err)
	}
	stored, _ := state.Attempt(attempt.ID)
	if stored.LandedProven || stored.ReleaseState != "held" {
		t.Fatalf("unproven mismatch landed or released: %+v", stored)
	}
	if _, err := os.Stat(attempt.WorktreePath); err != nil {
		t.Fatalf("failed verification changed the native worktree: %v", err)
	}
}

func localRecoveryFixture(t *testing.T) (Service, *store.Store, model.Task, model.Attempt, string) {
	return localRecoveryFixtureWithMerge(t, true)
}

// localRecoveryFixtureWithMerge reproduces the live task_19d6d50a4f9a shape in
// an isolated repository: the attempt and checkpoint branch is exact, the done
// event was accepted with a truncated branch artifact by a legacy binary
// (modeled by rewriting the accepted rows directly, since AddEventForRun now
// fails closed), and the sealed commit is merged into local main when
// merged is true.
func localRecoveryFixtureWithMerge(t *testing.T, merged bool) (Service, *store.Store, model.Task, model.Attempt, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	worker := filepath.Join(t.TempDir(), "worker")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, root, "init", "-b", "main")
	runLocalGit(t, root, "config", "user.name", "Test")
	runLocalGit(t, root, "config", "user.email", "test@example.com")
	runLocalGit(t, root, "commit", "--allow-empty", "-m", "base")
	base := runLocalGit(t, root, "rev-parse", "HEAD")
	runLocalGit(t, root, "worktree", "add", "-b", recoveryBranch, worker, "main")
	if err := os.WriteFile(filepath.Join(worker, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, worker, "add", "work.txt")
	runLocalGit(t, worker, "commit", "-m", "worker")
	sealed := runLocalGit(t, worker, "rev-parse", "HEAD")
	databasePath := filepath.Join(t.TempDir(), "state.db")
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "recovery", Objective: "recover", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", worker, "lease-1", recoveryBranch); err != nil {
		t.Fatal(err)
	}
	if err := state.SetAttemptBaseCommit(attempt.ID, base); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{HeadCommit: base}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: 1, Summary: "done", NextSteps: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "done", Checkpoint: &checkpoint}, 1,
		model.WorkspaceFacts{HeadCommit: sealed}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "SENSITIVE_DONE_PROSE claims branch shephrd/wrong", Artifact: "branch:" + recoveryBranch}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if merged {
		runLocalGit(t, root, "merge", "--no-ff", "-m", "merge worker", recoveryBranch)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	// Rewrite the accepted rows to the truncated legacy shape the live
	// database holds; the fence prevents producing it through the store API.
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE messages SET artifact_ref=? WHERE attempt_id=? AND type='done' AND stale=0`, recoveryTruncated, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE tasks SET artifact_ref=? WHERE id=?`, recoveryTruncated, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	task, _ = state.Task(task.ID)
	attempt, _ = state.Attempt(attempt.ID)
	if task.ArtifactRef != recoveryTruncated || task.Status != "done" {
		t.Fatalf("fixture task = %+v", task)
	}
	service := New(config.Config{DataDir: t.TempDir(), WorktreeRoot: filepath.Dir(worker)}, state)
	service.environment = environmentCapability{get: func(string) string { return "" }}
	return service, state, task, attempt, databasePath
}
