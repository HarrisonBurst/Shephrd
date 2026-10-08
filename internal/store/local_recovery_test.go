package store

import (
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
)

// The live reproduced shape from task_19d6d50a4f9a: the attempt and checkpoint
// branch is exact, the accepted artifact ref lost its final character, and the
// sealed commit is already contained in local main.
const (
	liveBranch    = "shephrd/task_19d6d50a4f9a"
	liveTruncated = "branch:shephrd/task_19d6d50a4f9"
	liveSealed    = "534d3ecaeac339bc914ecea45b481bf472e84e5e"
	liveHead      = "aaaa111122223333444455556666777788889999"
)

func TestDoneBranchArtifactMustNameExactAttemptBranch(t *testing.T) {
	for _, test := range []struct {
		name     string
		artifact string
	}{
		{name: "truncated ref", artifact: liveTruncated},
		{name: "wrong branch", artifact: "branch:shephrd/other-task"},
		{name: "cross-attempt ref", artifact: "branch:shephrd/task_19d6d50a4f9a-attempt-2"},
		{name: "padded ref", artifact: "branch:" + liveBranch + " "},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, task, attempt, generation := localRecoveryWorkingAttempt(t)
			defer state.Close()
			message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: test.artifact}, 2, model.WorkspaceFacts{})
			if err == nil || !strings.Contains(err.Error(), "protocol violation") {
				t.Fatalf("error = %v", err)
			}
			if message.Direction != "system" || message.Type != "blocked" {
				t.Fatalf("message = %+v", message)
			}
			current := mustTask(t, state, task.ID)
			if current.Status != "blocked" || current.ClaimedDone || current.ArtifactRef != "" {
				t.Fatalf("mismatched branch artifact was accepted: %+v", current)
			}
			var staleCount, acceptedCount int
			if err := state.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND type='done' AND stale=1`, attempt.ID).Scan(&staleCount); err != nil {
				t.Fatal(err)
			}
			if err := state.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND type='done' AND stale=0`, attempt.ID).Scan(&acceptedCount); err != nil {
				t.Fatal(err)
			}
			if staleCount != 1 || acceptedCount != 0 {
				t.Fatalf("stale=%d accepted=%d", staleCount, acceptedCount)
			}
		})
	}
}

func TestDoneBranchArtifactFenceRequiresCheckpointBranchIdentity(t *testing.T) {
	state, task, attempt, generation := localRecoveryWorkingAttempt(t)
	defer state.Close()
	if _, err := state.db.Exec(`UPDATE attempt_checkpoints SET branch='shephrd/stale-branch' WHERE attempt_id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "branch:" + liveBranch}, 2, model.WorkspaceFacts{}); err == nil || !strings.Contains(err.Error(), "protocol violation") {
		t.Fatalf("stale checkpoint branch was accepted: %v", err)
	}
	if current := mustTask(t, state, task.ID); current.Status != "blocked" || current.ClaimedDone {
		t.Fatalf("task = %+v", current)
	}
}

func TestDoneExactBranchAndNonBranchArtifactsRemainAccepted(t *testing.T) {
	state, task, attempt, generation := localRecoveryWorkingAttempt(t)
	defer state.Close()
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "branch:" + liveBranch}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	current := mustTask(t, state, task.ID)
	if current.Status != "done" || !current.ClaimedDone || current.ArtifactRef != "branch:"+liveBranch {
		t.Fatalf("task = %+v", current)
	}
	pr, taskPR, attemptPR, generationPR := localRecoveryWorkingAttempt(t)
	defer pr.Close()
	if _, err := pr.AddEventForRun(attemptPR.ID, generationPR, model.Event{Type: "done", Payload: "done", Artifact: "https://github.com/acme/demo/pull/42"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if current := mustTask(t, pr, taskPR.ID); current.Status != "done" || current.ArtifactRef != "https://github.com/acme/demo/pull/42" {
		t.Fatalf("PR task = %+v", current)
	}
}

func TestLocalRecoveryRowsRemainImmutable(t *testing.T) {
	state, task, attempt, generation := localRecoveryWorkingAttempt(t)
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "branch:" + liveBranch}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	var doneMessageID int
	if err := state.db.QueryRow(`SELECT id FROM messages WHERE attempt_id=? AND type='done' AND stale=0`, attempt.ID).Scan(&doneMessageID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := state.db.Exec(`INSERT INTO local_delivery_recoveries(id, schema_version, task_id, attempt_id, repo_id, run_generation,
		done_message_id, checkpoint_revision, original_artifact_ref, attempt_branch, sealed_commit, registered_common_git_dir,
		registered_default_branch, default_head_at_validation, ancestry_validation, attested_by_driver_id, evidence_validated_at, created_at)
		VALUES(?, 1, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"recovery-immutable", task.ID, attempt.ID, task.RepoID, attempt.RunGeneration,
		doneMessageID, liveTruncated, liveBranch, liveSealed, "/repo/.git", "main", liveHead,
		"local-default-ancestry+shared-common-dir-v1", task.DriverID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := state.db.Exec(`UPDATE local_delivery_recoveries SET sealed_commit=? WHERE id=?`, strings.Repeat("c", 40), "recovery-immutable"); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("update error = %v", err)
	}
	if _, err := state.db.Exec(`DELETE FROM local_delivery_recoveries WHERE id=?`, "recovery-immutable"); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("delete error = %v", err)
	}
	persisted, err := state.LocalDeliveryRecoveryForAttempt(attempt.ID)
	if err != nil || persisted == nil || persisted.ID != "recovery-immutable" || persisted.OriginalArtifactRef != liveTruncated {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	detail, err := state.Detail(task.ID)
	if err != nil || len(detail.LocalDeliveryRecoveries) != 1 || detail.LocalDeliveryRecoveries[0].ID != "recovery-immutable" {
		t.Fatalf("detail=%+v err=%v", detail.LocalDeliveryRecoveries, err)
	}
}

func localRecoveryWorkingAttempt(t *testing.T) (*Store, model.Task, model.Attempt, int) {
	t.Helper()
	state, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
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
	if err := state.ConfigureAttempt(attempt.ID, "session", t.TempDir(), "lease", liveBranch); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{HeadCommit: strings.Repeat("0", 40)}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: 1, Summary: "done", NextSteps: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "done", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{HeadCommit: liveSealed}); err != nil {
		t.Fatal(err)
	}
	task = mustTask(t, state, task.ID)
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return state, task, attempt, generation
}
