package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/delivery"
	forgegithub "shephrd/internal/forge/github"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/worktree"
)

const (
	controlSealed  = "1111111111111111111111111111111111111111"
	controlHead    = "2222222222222222222222222222222222222222"
	controlMerge   = "3333333333333333333333333333333333333333"
	controlDefault = "4444444444444444444444444444444444444444"
)

type controlExternalRunner struct {
	calls int
}

type failingExternalReleaseRunner struct {
	worktree.ExecRunner
}

type controlForge struct{}

type livenessForge struct {
	observation forgegithub.Observation
}

func (controlForge) Observe(ctx context.Context, request forgegithub.Request) (forgegithub.Observation, error) {
	return controlObservation(), nil
}

func (f *livenessForge) Observe(ctx context.Context, request forgegithub.Request) (forgegithub.Observation, error) {
	return f.observation, nil
}

func controlObservation() forgegithub.Observation {
	return forgegithub.Observation{
		SchemaVersion:      forgegithub.ObservationSchemaVersion,
		Repository:         forgegithub.RepositoryObservation{ID: "repo-node", NameWithOwner: "acme/demo", URL: "https://github.com/acme/demo", DefaultBranchName: "main", DefaultBranchOID: controlDefault},
		PullRequest:        forgegithub.PullRequestObservation{ID: "pr-node", URL: "https://github.com/acme/demo/pull/42", Number: 42, State: "MERGED", MergedAt: "2026-08-13T07:25:38Z", BaseRepository: "acme/demo", BaseRefName: "main", HeadRefName: "review", HeadRefOID: controlHead, MergeCommitOID: controlMerge},
		Commits:            []string{controlSealed, controlHead},
		PaginationComplete: true,
		SealedCompare:      forgegithub.CompareObservation{Status: "ahead", MergeBaseSHA: controlSealed},
		MergeCompare:       forgegithub.CompareObservation{Status: "ahead", MergeBaseSHA: controlMerge},
	}
}

func (r failingExternalReleaseRunner) Run(dir, command string, args ...string) ([]byte, []byte, error) {
	if command == "git" && len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
		return nil, nil, errors.New("release failed")
	}
	return r.ExecRunner.Run(dir, command, args...)
}

func (r *controlExternalRunner) Run(dir, command string, args ...string) ([]byte, []byte, error) {
	if command == "git" {
		if args[0] == "check-ref-format" {
			return nil, nil, nil
		}
		if args[0] == "remote" {
			return []byte("git@github.com:acme/demo.git\n"), nil, nil
		}
		if args[0] == "ls-remote" {
			return nil, nil, errors.New("not pushed")
		}
	}
	if command == "gh" {
		r.calls++
		call := strings.Join(args, " ")
		if strings.Contains(call, "graphql") {
			return []byte(fmt.Sprintf(`[{"data":{"repository":{"id":"repo-node","nameWithOwner":"acme/demo","url":"https://github.com/acme/demo","defaultBranchRef":{"name":"main","target":{"oid":"%s"}},"pullRequest":{"id":"pr-node","url":"https://github.com/acme/demo/pull/42","state":"MERGED","mergedAt":"2026-08-13T07:25:38Z","baseRefName":"main","headRefName":"review","headRefOid":"%s","baseRepository":{"nameWithOwner":"acme/demo"},"mergeCommit":{"oid":"%s"},"commits":{"nodes":[{"commit":{"oid":"%s"}},{"commit":{"oid":"%s"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}]`, controlDefault, controlHead, controlMerge, controlSealed, controlHead)), nil, nil
		}
		base := controlMerge
		if strings.Contains(call, controlSealed+"..."+controlHead) {
			base = controlSealed
		}
		body, _ := json.Marshal(map[string]any{"status": "ahead", "merge_base_commit": map[string]string{"sha": base}})
		return body, nil, nil
	}
	return nil, nil, errors.New("unexpected command")
}

func TestExternalDeliveryAttestationRequiresExplicitVerifyAndReleasesOnce(t *testing.T) {
	service, state, task, attempt, github := externalControlFixture(t)
	defer state.Close()
	ordinary, err := service.Verify(task.ID)
	if err != nil || ordinary.Landed {
		t.Fatalf("ordinary result=%+v err=%v", ordinary, err)
	}
	attested, err := service.AttestDelivery(task.ID, "", task.DriverID, "https://github.com/acme/demo/pull/42", controlSealed, false)
	if err != nil {
		t.Fatal(err)
	}
	if attested.LandingProven || attested.CommitRelation != "ancestor" || attested.OriginalArtifactRef != "branch:"+attempt.Branch || !attested.VerificationEnabled {
		t.Fatalf("attestation = %+v", attested)
	}
	callsAfterAttestation := github.calls
	again, err := service.AttestDelivery(task.ID, "", task.DriverID, "https://github.com/acme/demo/pull/42", controlSealed, false)
	if err != nil || !again.Idempotent || again.AttestationID != attested.AttestationID || github.calls != callsAfterAttestation {
		t.Fatalf("idempotent attestation=%+v calls=%d err=%v", again, github.calls, err)
	}
	storedTask, _ := state.Task(task.ID)
	storedAttempt, _ := state.Attempt(attempt.ID)
	if storedTask.Landed || storedAttempt.LandedProven || storedAttempt.ReleaseState != "held" || storedTask.ArtifactRef != "branch:"+attempt.Branch {
		t.Fatalf("attestation mutated lifecycle: task=%+v attempt=%+v", storedTask, storedAttempt)
	}
	if _, err := os.Stat(attempt.WorktreePath); err != nil {
		t.Fatalf("attestation changed native worktree: %v", err)
	}
	result, err := service.VerifyAttempt(task.ID, "", task.DriverID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Landed || result.Landing.Kind != "github_pr_attested_ancestry" || result.Landing.SourceCommit != controlSealed || result.Landing.TargetCommit != controlMerge || !result.Worktree.Released || !strings.Contains(result.Reason, "original artifact") {
		t.Fatalf("verification = %+v", result)
	}
	storedTask, _ = state.Task(task.ID)
	if !storedTask.Landed || storedTask.ArtifactRef != "branch:"+attempt.Branch || storedTask.BranchPushed || storedTask.PRState != "" {
		t.Fatalf("task = %+v", storedTask)
	}
	calls := github.calls
	if _, err := service.VerifyAttempt(task.ID, "", task.DriverID); err != nil {
		t.Fatal(err)
	}
	if github.calls != calls {
		t.Fatalf("idempotent proof revalidated GitHub: before=%d after=%d", calls, github.calls)
	}
	if _, err := os.Stat(attempt.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("verified native worktree still exists: %v", err)
	}
}

func TestExternalDeliveryReverificationAllowsDefaultHeadAdvance(t *testing.T) {
	service, state, task, _, _ := externalControlFixture(t)
	defer state.Close()
	forge := &livenessForge{observation: controlObservation()}
	service.Verifier.Forge = forge
	if _, err := service.AttestDelivery(task.ID, "", task.DriverID, "https://github.com/acme/demo/pull/42", controlSealed, false); err != nil {
		t.Fatal(err)
	}
	attestation, err := state.ExternalDeliveryAttestationForAttempt(task.CurrentAttemptID)
	if err != nil || attestation == nil {
		t.Fatalf("attestation=%+v err=%v", attestation, err)
	}
	initialDigest, err := forgegithub.ObservationDigest(forge.observation)
	if err != nil || attestation.EvidenceDigest != initialDigest {
		t.Fatalf("stored digest=%q want=%q err=%v", attestation.EvidenceDigest, initialDigest, err)
	}
	forge.observation.Repository.DefaultBranchOID = "5555555555555555555555555555555555555555"
	forge.observation.MergeCompare.MergeBaseSHA = controlMerge
	advancedDigest, err := forgegithub.ObservationDigest(forge.observation)
	if err != nil || advancedDigest == attestation.EvidenceDigest {
		t.Fatalf("advanced digest=%q initial=%q err=%v", advancedDigest, attestation.EvidenceDigest, err)
	}
	result, err := service.VerifyAttempt(task.ID, "", task.DriverID)
	if err != nil || !result.Landed {
		t.Fatalf("reverification result=%+v err=%v", result, err)
	}
}

func TestExternalDeliveryReverificationRejectsChangedImmutableEvidence(t *testing.T) {
	service, state, task, _, _ := externalControlFixture(t)
	defer state.Close()
	forge := &livenessForge{observation: controlObservation()}
	service.Verifier.Forge = forge
	if _, err := service.AttestDelivery(task.ID, "", task.DriverID, "https://github.com/acme/demo/pull/42", controlSealed, false); err != nil {
		t.Fatal(err)
	}
	forge.observation.PullRequest.HeadRefName = "rewritten"
	if _, err := service.VerifyAttempt(task.ID, "", task.DriverID); err == nil || controlErrorKind(err) != "attestation_conflict" {
		t.Fatalf("changed immutable evidence error = %v", err)
	}
}

func TestExternalDeliveryVerificationRetainsProofAcrossReleaseFailure(t *testing.T) {
	service, state, task, attempt, github := externalControlFixture(t)
	defer state.Close()
	if _, err := service.AttestDelivery(task.ID, "", task.DriverID, "https://github.com/acme/demo/pull/42", controlSealed, false); err != nil {
		t.Fatal(err)
	}
	service.Native.Runner = failingExternalReleaseRunner{ExecRunner: worktree.ExecRunner{}}
	result, err := service.VerifyAttempt(task.ID, "", task.DriverID)
	if err == nil || !result.Landed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stored, _ := state.Attempt(attempt.ID)
	if !stored.LandedProven || stored.ReleaseState != "held" {
		t.Fatalf("attempt after release failure = %+v", stored)
	}
	calls := github.calls
	service.Native.Runner = worktree.ExecRunner{}
	result, err = service.VerifyAttempt(task.ID, "", task.DriverID)
	if err != nil || !result.Worktree.Released || github.calls != calls {
		t.Fatalf("retry result=%+v calls=%d err=%v", result, github.calls, err)
	}
}

func TestExternalDeliveryVerificationKeepsSupersededAttemptIsolated(t *testing.T) {
	service, state, task, attempt, _ := externalControlFixture(t)
	defer state.Close()
	if _, err := service.AttestDelivery(task.ID, "", task.DriverID, "https://github.com/acme/demo/pull/42", controlSealed, false); err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	current, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.VerifyAttempt(task.ID, attempt.ID, task.DriverID)
	if err != nil || !result.Landed || result.Landing.Kind != "github_pr_attested_ancestry" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	storedTask, _ := state.Task(task.ID)
	storedOld, _ := state.Attempt(attempt.ID)
	if storedTask.CurrentAttemptID != current.ID || storedTask.Landed || !storedOld.LandedProven || storedOld.LandingKind != "github_pr_attested_ancestry" {
		t.Fatalf("task=%+v old=%+v", storedTask, storedOld)
	}
}

func TestExternalDeliveryAttestationSupportsArchivedDiscardReleasedAttempt(t *testing.T) {
	service, state, task, attempt, _ := externalControlFixture(t)
	defer state.Close()
	if _, err := state.ArchiveTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Release(task.ID, attempt.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AttestDelivery(task.ID, "", task.DriverID, "https://github.com/acme/demo/pull/42", controlSealed, false); err != nil {
		t.Fatal(err)
	}
	result, err := service.VerifyAttempt(task.ID, "", task.DriverID)
	if err != nil || !result.Landed || !result.Worktree.Released {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(attempt.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("released historical native worktree exists: %v", err)
	}
}

func TestExternalDeliveryAttestationOwnershipWorkerContextAndAdoption(t *testing.T) {
	service, state, task, attempt, _ := externalControlFixture(t)
	defer state.Close()
	if _, err := service.AttestDelivery(task.ID, "", "driver:other", "https://github.com/acme/demo/pull/42", controlSealed, false); err == nil || controlErrorKind(err) != "task_owner_mismatch" {
		t.Fatalf("owner error = %v", err)
	}
	workerService := service
	workerService.environment = environmentCapability{get: func(name string) string {
		if name == "SHEPHRD_ATTEMPT_ID" {
			return "attempt-worker"
		}
		return ""
	}}
	if _, err := workerService.AttestDelivery(task.ID, "", task.DriverID, "https://github.com/acme/demo/pull/42", controlSealed, false); err == nil || controlErrorKind(err) != "worker_context_forbidden" {
		t.Fatalf("worker error = %v", err)
	}
	if _, err := service.AttestDelivery(task.ID, "", task.DriverID, "https://github.com/acme/demo/pull/42", controlSealed, false); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AdoptTask(task.ID, task.DriverID, "driver:adopted"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyAttempt(task.ID, attempt.ID, "driver:adopted"); err != nil {
		t.Fatal(err)
	}
	attestation, _ := state.ExternalDeliveryAttestationForAttempt(attempt.ID)
	if attestation.AttestedByDriverID != task.DriverID {
		t.Fatalf("attested by changed after adoption: %+v", attestation)
	}
}

func externalControlFixture(t *testing.T) (Service, *store.Store, model.Task, model.Attempt, *controlExternalRunner) {
	t.Helper()
	base := t.TempDir()
	repoPath := filepath.Join(base, "repo")
	worktreeRoot := filepath.Join(base, "worktrees")
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repoPath, "init", "-b", "main")
	runLocalGit(t, repoPath, "config", "user.name", "Test")
	runLocalGit(t, repoPath, "config", "user.email", "test@example.com")
	runLocalGit(t, repoPath, "commit", "--allow-empty", "-m", "base")
	state, err := store.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: repoPath, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "external-control", Objective: "external", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	workerPath := filepath.Join(worktreeRoot, repo.ID, attempt.ID, "demo")
	if err := os.MkdirAll(filepath.Dir(workerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repoPath, "worktree", "add", "-b", "shephrd/external", workerPath, "main")
	if err := configureAttempt(t, state, attempt.ID, "session", workerPath, "", "shephrd/external"); err != nil {
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
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "done", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{HeadCommit: controlSealed}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "branch:shephrd/external"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	task, _ = state.Task(task.ID)
	attempt, _ = state.Attempt(attempt.ID)
	github := &controlExternalRunner{}
	service := New(config.Config{DataDir: t.TempDir(), WorktreeRoot: worktreeRoot}, state)
	service.environment = environmentCapability{get: func(string) string { return "" }}
	service.Verifier = delivery.Verifier{Runner: github, DataDir: t.TempDir(), Forge: controlForge{}}
	return service, state, task, attempt, github
}

func controlErrorKind(err error) string {
	type classified interface {
		ErrorKind() string
	}
	if value, ok := err.(classified); ok {
		return value.ErrorKind()
	}
	return ""
}
