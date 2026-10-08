package store

import (
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
)

const (
	storeSealed  = "1111111111111111111111111111111111111111"
	storeHead    = "2222222222222222222222222222222222222222"
	storeMerge   = "3333333333333333333333333333333333333333"
	storeDefault = "4444444444444444444444444444444444444444"
)

func TestExternalDeliveryAttestationIsImmutableIdempotentAndAdditive(t *testing.T) {
	state, task, attempt := externalAttestationAttempt(t)
	defer state.Close()
	candidate, err := state.DeliveryAttestationCandidate(task.ID, "", task.DriverID, storeSealed, false)
	if err != nil {
		t.Fatal(err)
	}
	stored, idempotent, err := state.RecordExternalDeliveryAttestation(candidate, storeEvidence())
	if err != nil || idempotent {
		t.Fatalf("stored=%+v idempotent=%t err=%v", stored, idempotent, err)
	}
	again, idempotent, err := state.RecordExternalDeliveryAttestation(candidate, storeEvidence())
	if err != nil || !idempotent || again.ID != stored.ID || !again.CreatedAt.Equal(stored.CreatedAt) {
		t.Fatalf("again=%+v idempotent=%t err=%v", again, idempotent, err)
	}
	conflict := storeEvidence()
	conflict.PRURL = "https://github.com/acme/demo/pull/43"
	conflict.PRNumber = 43
	if _, _, err := state.RecordExternalDeliveryAttestation(candidate, conflict); err == nil || attestationErrorKind(err) != "attestation_conflict" {
		t.Fatalf("conflict error = %v", err)
	}
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ArtifactRef != "branch:"+attempt.Branch || current.Landed || current.BranchPushed || current.RemoteDeliveryState != "unverified" || current.PRState != "" {
		t.Fatalf("task projections changed = %+v", current)
	}
	detail, err := state.Detail(task.ID)
	if err != nil || len(detail.ExternalDeliveryAttestations) != 1 || detail.ExternalDeliveryAttestations[0].ID != stored.ID {
		t.Fatalf("detail=%+v err=%v", detail.ExternalDeliveryAttestations, err)
	}
	if _, err := state.db.Exec(`UPDATE external_delivery_attestations SET pr_number=44 WHERE id=?`, stored.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("update error = %v", err)
	}
	if _, err := state.db.Exec(`DELETE FROM external_delivery_attestations WHERE id=?`, stored.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("delete error = %v", err)
	}
}

func TestExternalDeliveryAttestationRequiresEvidenceDigest(t *testing.T) {
	state, task, _ := externalAttestationAttempt(t)
	defer state.Close()
	candidate, err := state.DeliveryAttestationCandidate(task.ID, "", task.DriverID, storeSealed, false)
	if err != nil {
		t.Fatal(err)
	}
	evidence := storeEvidence()
	evidence.EvidenceDigest = ""
	if _, _, err := state.RecordExternalDeliveryAttestation(candidate, evidence); err == nil || attestationErrorKind(err) != "github_unavailable" {
		t.Fatalf("missing digest error = %v", err)
	}
}

func TestExternalDeliveryAttestationFencesOwnerSelectionAndLandingProof(t *testing.T) {
	state, task, attempt := externalAttestationAttempt(t)
	defer state.Close()
	if _, err := state.DeliveryAttestationCandidate(task.ID, "", "driver:other", storeSealed, false); err == nil || attestationErrorKind(err) != "task_owner_mismatch" {
		t.Fatalf("owner error = %v", err)
	}
	candidate, err := state.DeliveryAttestationCandidate(task.ID, "", task.DriverID, storeSealed, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.BeginAttempt(task.ID, "pi", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.RecordExternalDeliveryAttestation(candidate, storeEvidence()); err == nil || attestationErrorKind(err) != "attempt_selection_changed" {
		t.Fatalf("retry race error = %v", err)
	}
	old, err := state.DeliveryAttestationCandidate(task.ID, attempt.ID, task.DriverID, storeSealed, true)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := state.RecordExternalDeliveryAttestation(old, storeEvidence())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordLandingProof((mustTask(t, state, task.ID)).CurrentAttemptID, model.LandingProof{TaskID: task.ID, AttemptID: attempt.ID,
		RunGeneration: attempt.RunGeneration, Kind: "github_pr_attested_ancestry", SourceCommit: storeSealed,
		TargetRef: "refs/heads/main", TargetCommit: storeMerge, CheckpointRevision: old.Checkpoint.Revision}, "attested"); err != nil {
		t.Fatal(err)
	}
	current := mustTask(t, state, task.ID)
	if current.Landed {
		t.Fatalf("superseded attestation projected onto current task: %+v", current)
	}
	persisted, err := state.ExternalDeliveryAttestationForAttempt(attempt.ID)
	if err != nil || persisted == nil || persisted.ID != stored.ID {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
}

func TestExternalAttestedLandingProjectionRejectsMismatchedEvidence(t *testing.T) {
	state, task, attempt := externalAttestationAttempt(t)
	defer state.Close()
	candidate, err := state.DeliveryAttestationCandidate(task.ID, "", task.DriverID, storeSealed, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.RecordExternalDeliveryAttestation(candidate, storeEvidence()); err != nil {
		t.Fatal(err)
	}
	if _, err := state.db.Exec(`UPDATE attempts SET landed_proven=1, landing_kind='github_pr_attested_ancestry',
		landed_source_commit=?, landed_target_ref='refs/heads/main', landed_target_commit=?,
		landed_checkpoint_revision=?, landed_verified_at=?, landing_reason='mismatched evidence' WHERE id=?`,
		storeSealed, strings.Repeat("9", 40), candidate.Checkpoint.Revision, now(), attempt.ID); err != nil {
		t.Fatal(err)
	}
	projected, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Landed || projected.LandedReason != "" {
		t.Fatalf("mismatched attestation projected onto task: %+v", projected)
	}
	if _, err := state.CompleteLandingProof(attempt.ID); err == nil || !strings.Contains(err.Error(), "attestation evidence") {
		t.Fatalf("mismatched attestation completed proof: %v", err)
	}
}

func TestExternalDeliveryEligibilityRejectsInvalidAcceptedEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Store, model.Task, model.Attempt)
		commit string
		kind   string
	}{
		{name: "abbreviated commit", commit: storeSealed[:12], kind: "sealed_commit_mismatch"},
		{name: "wrong commit", commit: strings.Repeat("a", 40), kind: "sealed_commit_mismatch"},
		{name: "dirty checkpoint", commit: storeSealed, kind: "checkpoint_not_clean", mutate: func(s *Store, _ model.Task, a model.Attempt) {
			_, _ = s.db.Exec(`UPDATE attempt_checkpoints SET worktree_dirty=1 WHERE attempt_id=?`, a.ID)
		}},
		{name: "wrong artifact", commit: storeSealed, kind: "branch_artifact_required", mutate: func(s *Store, _ model.Task, a model.Attempt) {
			_, _ = s.db.Exec(`UPDATE messages SET artifact_ref='https://github.com/acme/demo/pull/42' WHERE attempt_id=? AND type='done'`, a.ID)
		}},
		{name: "recorded process", commit: storeSealed, kind: "attempt_not_eligible", mutate: func(s *Store, _ model.Task, a model.Attempt) {
			_, _ = s.db.Exec(`UPDATE attempts SET runner_pid=12345 WHERE id=?`, a.ID)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, task, attempt := externalAttestationAttempt(t)
			defer state.Close()
			if test.mutate != nil {
				test.mutate(state, task, attempt)
			}
			_, err := state.DeliveryAttestationCandidate(task.ID, "", task.DriverID, test.commit, false)
			if err == nil || attestationErrorKind(err) != test.kind {
				t.Fatalf("error = %v kind=%q, want %q", err, attestationErrorKind(err), test.kind)
			}
		})
	}
}

func externalAttestationAttempt(t *testing.T) (*Store, model.Task, model.Attempt) {
	t.Helper()
	state, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "external", Objective: "external", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(attempt.ID, "session", t.TempDir(), "lease", "shephrd/task-external"); err != nil {
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
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "done", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{HeadCommit: storeSealed}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "branch:shephrd/task-external"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	task = mustTask(t, state, task.ID)
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return state, task, attempt
}

func storeEvidence() model.ExternalDeliveryAttestation {
	return model.ExternalDeliveryAttestation{Provider: "github", RemoteHost: "github.com", RemoteRepository: "acme/demo", PRNumber: 42,
		PRNodeID: "pr-node", PRURL: "https://github.com/acme/demo/pull/42", PRBaseRef: "main", PRHeadRef: "review",
		PRHeadCommit: storeHead, MergeCommit: storeMerge, MergedAt: "2026-08-13T07:25:38Z", DefaultHeadAtValidation: storeDefault,
		GraphValidation: "github-pr-membership+compare-v1", EvidenceDigest: strings.Repeat("a", 64), EvidenceValidatedAt: time.Now().UTC()}
}

func mustTask(t *testing.T, state *Store, taskID string) model.Task {
	t.Helper()
	task, err := state.Task(taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func attestationErrorKind(err error) string {
	type classified interface {
		ErrorKind() string
	}
	if value, ok := err.(classified); ok {
		return value.ErrorKind()
	}
	return ""
}
