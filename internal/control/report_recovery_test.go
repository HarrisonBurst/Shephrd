package control

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
	"shephrd/internal/worktree"
)

func TestAttestReportRecoveryThenVerifySnapshotsAndReleases(t *testing.T) {
	service, state, task, attempt, checkpoint, reportBody := reportRecoveryControlFixture(t)
	defer state.Close()
	if _, err := service.AttestReportRecovery(task.ID, attempt.ID, "driver:other", "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor); err == nil || controlErrorKind(err) != "task_owner_mismatch" {
		t.Fatalf("owner error = %v", err)
	}
	worker := service
	worker.environment = environmentCapability{get: func(name string) string {
		if name == "SHEPHRD_ATTEMPT_ID" {
			return attempt.ID
		}
		return ""
	}}
	if _, err := worker.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor); err == nil || controlErrorKind(err) != "worker_context_forbidden" {
		t.Fatalf("worker attestation error = %v", err)
	}
	attested, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil {
		t.Fatal(err)
	}
	if attested.Idempotent || attested.LandingProven || attested.ReleaseState != "held" || attested.SHA256 == "" || attested.SizeBytes != int64(len(reportBody)) || attested.CompletionProvenance != model.CompletionProvenanceReportRecovery {
		t.Fatalf("attested = %+v", attested)
	}
	again, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil || !again.Idempotent || again.AttestationID != attested.AttestationID {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	if _, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "different reason", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor); err == nil || controlErrorKind(err) != "attestation_conflict" {
		t.Fatalf("reason conflict = %v", err)
	}
	if _, err := worker.VerifyAttempt(task.ID, attempt.ID, task.DriverID); err == nil || controlErrorKind(err) != "worker_context_forbidden" {
		t.Fatalf("worker verify error = %v", err)
	}
	result, err := service.VerifyAttempt(task.ID, attempt.ID, task.DriverID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Landed || !result.Worktree.Released || result.CompletionProvenance != model.CompletionProvenanceReportRecovery || result.VerifiedArtifact == nil || result.VerifiedArtifact.ReportRecoveryID != attested.AttestationID {
		t.Fatalf("result = %+v", result)
	}
	snapshot, err := os.ReadFile(result.VerifiedArtifact.SnapshotPath)
	if err != nil || !bytes.Equal(snapshot, reportBody) {
		t.Fatalf("snapshot=%q err=%v", snapshot, err)
	}
	current, _ := state.Task(task.ID)
	stored, _ := state.Attempt(attempt.ID)
	if current.ClaimedDone || current.CompletionProvenance != model.CompletionProvenanceReportRecovery || !current.Landed || stored.ReleaseState != "released" {
		t.Fatalf("task=%+v attempt=%+v", current, stored)
	}
	if _, err := os.Stat(attempt.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree remains: %v", err)
	}
	if _, err := service.VerifyAttempt(task.ID, attempt.ID, task.DriverID); err != nil {
		t.Fatalf("idempotent verify: %v", err)
	}
	replayed, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil || !replayed.Idempotent || replayed.AttestationID != attested.AttestationID {
		t.Fatalf("post-verification replay=%+v err=%v", replayed, err)
	}
}

func TestAttestedReportRecoveryFreezesSameWorktreeContinuation(t *testing.T) {
	service, state, task, attempt, checkpoint, _ := reportRecoveryControlFixture(t)
	defer state.Close()
	if _, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor); err != nil {
		t.Fatal(err)
	}
	service.environment.executable = func() (string, error) { return "", errors.New("worker launch must not be attempted") }
	beforeTask, _ := state.Task(task.ID)
	beforeAttempt, _ := state.Attempt(attempt.ID)
	beforeMessages, _ := state.Messages(task.ID)
	operations := []func() error{
		func() error { _, err := service.Relaunch(task.ID); return err },
		func() error { _, err := service.Send(task.ID, "continue in the same worktree"); return err },
	}
	errors := make(chan error, len(operations))
	var group sync.WaitGroup
	for _, operation := range operations {
		group.Add(1)
		go func(operation func() error) {
			defer group.Done()
			errors <- operation()
		}(operation)
	}
	group.Wait()
	close(errors)
	verifyCommand := fmt.Sprintf("shephrd task verify-delivery %s --attempt %s --driver-id %s --json", task.ID, attempt.ID, model.ShellQuote(task.DriverID))
	retryCommand := fmt.Sprintf("shephrd worker retry %s --json", task.ID)
	for err := range errors {
		if err == nil || controlErrorKind(err) != "report_recovery_continuation_forbidden" || !strings.Contains(err.Error(), verifyCommand) || !strings.Contains(err.Error(), retryCommand) {
			t.Fatalf("continuation error = %v", err)
		}
		typed, ok := err.(interface{ ErrorEvidence() map[string]string })
		if !ok || typed.ErrorEvidence()["verify_command"] != verifyCommand || typed.ErrorEvidence()["retry_command"] != retryCommand {
			t.Fatalf("continuation evidence = %+v", err)
		}
	}
	afterTask, _ := state.Task(task.ID)
	afterAttempt, _ := state.Attempt(attempt.ID)
	afterMessages, _ := state.Messages(task.ID)
	if !reflect.DeepEqual(afterTask, beforeTask) || !reflect.DeepEqual(afterAttempt, beforeAttempt) || !reflect.DeepEqual(afterMessages, beforeMessages) {
		t.Fatalf("rejected continuation mutated state\ntask: %+v\nattempt: %+v\nmessages: %+v", afterTask, afterAttempt, afterMessages)
	}
	if _, err := os.Stat(filepath.Join(service.Config.DataDir, task.ID, fmt.Sprintf("follow-up-%d.md", len(beforeMessages)+1))); !os.IsNotExist(err) {
		t.Fatalf("rejected send wrote a follow-up file: %v", err)
	}
	result, err := service.VerifyAttempt(task.ID, attempt.ID, task.DriverID)
	if err != nil || !result.Landed || result.CompletionProvenance != model.CompletionProvenanceReportRecovery {
		t.Fatalf("verification after rejected continuation = %+v, %v", result, err)
	}
}

func TestReportRecoveryAttestationWinsRelaunchRaceBeforeMutation(t *testing.T) {
	service, state, task, attempt, checkpoint, _ := reportRecoveryControlFixture(t)
	defer state.Close()
	entered := make(chan struct{})
	resume := make(chan struct{})
	relaunch := service
	relaunch.processes.alive = func(int) bool {
		close(entered)
		<-resume
		return false
	}
	result := make(chan error, 1)
	go func() {
		_, err := relaunch.Relaunch(task.ID)
		result <- err
	}()
	<-entered
	if _, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor); err != nil {
		t.Fatal(err)
	}
	beforeTask, _ := state.Task(task.ID)
	beforeAttempt, _ := state.Attempt(attempt.ID)
	beforeMessages, _ := state.Messages(task.ID)
	close(resume)
	if err := <-result; err == nil || controlErrorKind(err) != "report_recovery_continuation_forbidden" {
		t.Fatalf("racing relaunch error = %v", err)
	}
	afterTask, _ := state.Task(task.ID)
	afterAttempt, _ := state.Attempt(attempt.ID)
	afterMessages, _ := state.Messages(task.ID)
	if !reflect.DeepEqual(afterTask, beforeTask) || !reflect.DeepEqual(afterAttempt, beforeAttempt) || !reflect.DeepEqual(afterMessages, beforeMessages) {
		t.Fatalf("racing relaunch mutated attested state\ntask: %+v\nattempt: %+v\nmessages: %+v", afterTask, afterAttempt, afterMessages)
	}
	if _, err := service.VerifyAttempt(task.ID, attempt.ID, task.DriverID); err != nil {
		t.Fatalf("verification after racing relaunch: %v", err)
	}
}

func TestAttestReportRecoveryRequiresDeadExactTerminalEndpoint(t *testing.T) {
	service, state, task, attempt, checkpoint, _ := reportRecoveryControlFixture(t)
	defer state.Close()
	if err := state.SetRuntimeBackend(attempt.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	endpoint := terminal.Endpoint{Backend: "fixture", SocketPath: "/fixture.sock", WorkspaceID: "workspace", TabID: "tab", PaneID: "pane",
		ProviderVersion: "fixture-1", ProtocolVersion: "1", Capabilities: []string{"exact-close", "processes"}}
	if _, err := state.SetTerminalEndpointForRun(attempt.ID, attempt.RunGeneration, model.TerminalEndpoint{Backend: endpoint.Backend,
		SocketPath: endpoint.SocketPath, WorkspaceID: endpoint.WorkspaceID, TabID: endpoint.TabID, PaneID: endpoint.PaneID,
		ProviderVersion: endpoint.ProviderVersion, ProtocolVersion: endpoint.ProtocolVersion, Capabilities: endpoint.Capabilities}); err != nil {
		t.Fatal(err)
	}
	provider := &intentFaultProvider{state: state, attemptID: attempt.ID, endpoint: endpoint}
	service.TerminalProviders = []terminal.Provider{provider}
	if _, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor); err == nil || controlErrorKind(err) != "terminal_endpoint_alive" {
		t.Fatalf("live endpoint error = %v", err)
	}
	provider.closed = true
	if _, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor); err != nil {
		t.Fatalf("dead endpoint attestation = %v", err)
	}
	provider.closed = false
	if _, err := service.VerifyAttempt(task.ID, attempt.ID, task.DriverID); err == nil || controlErrorKind(err) != "terminal_endpoint_alive" {
		t.Fatalf("reappeared endpoint verification = %v", err)
	}
}

func TestVerifyRecoveredReportFailsClosedWhenCanonicalFileChanges(t *testing.T) {
	service, state, task, attempt, checkpoint, _ := reportRecoveryControlFixture(t)
	defer state.Close()
	if _, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor); err != nil {
		t.Fatal(err)
	}
	path, _ := service.Verifier.CanonicalReportPath(task.ID, attempt)
	if err := os.WriteFile(path, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyAttempt(task.ID, attempt.ID, task.DriverID); err == nil || controlErrorKind(err) != "attestation_conflict" {
		t.Fatalf("changed file error = %v", err)
	}
	stored, _ := state.Attempt(attempt.ID)
	if stored.LandedProven || stored.ReleaseState != "held" {
		t.Fatalf("changed file landed or released = %+v", stored)
	}
}

func reportRecoveryControlFixture(t *testing.T) (Service, *store.Store, model.Task, model.Attempt, model.AttemptCheckpoint, []byte) {
	t.Helper()
	return reportRecoveryDestinationFixture(t, false)
}

func TestReportRecoveryUsesAssignedRunDestination(t *testing.T) {
	service, state, task, attempt, checkpoint, _ := reportRecoveryDestinationFixture(t, true)
	defer state.Close()
	attested, err := service.AttestReportRecovery(task.ID, attempt.ID, task.DriverID, "handoff failed", attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil || attested.CanonicalReportPath != attempt.ReportPath {
		t.Fatalf("attestation=%+v err=%v", attested, err)
	}
	result, err := service.VerifyAttempt(task.ID, attempt.ID, task.DriverID)
	if err != nil || !result.Landed || result.VerifiedArtifact == nil || result.VerifiedArtifact.OriginalRef != "report:"+attempt.ReportPath {
		t.Fatalf("verification=%+v err=%v", result, err)
	}
}

func reportRecoveryDestinationFixture(t *testing.T, bound bool) (Service, *store.Store, model.Task, model.Attempt, model.AttemptCheckpoint, []byte) {
	t.Helper()
	base := t.TempDir()
	repoRoot := filepath.Join(base, "repo")
	worktreeRoot := filepath.Join(base, "worktrees")
	dataDir := filepath.Join(base, "data")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repoRoot, "init", "-b", "main")
	runLocalGit(t, repoRoot, "config", "user.name", "Test")
	runLocalGit(t, repoRoot, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repoRoot, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repoRoot, "add", "base.txt")
	runLocalGit(t, repoRoot, "commit", "-m", "base")
	state, err := store.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: repoRoot, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Recover report", DriverID: "driver:test", RepoID: repo.ID,
		FeatureKey: "recover-report", Objective: "recover complete report", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	manager := worktree.NewNative(worktreeRoot)
	path, err := manager.AllocatePath(repo, attempt)
	if err != nil {
		t.Fatal(err)
	}
	branch := worktree.BranchName(attempt)
	baseCommit := runLocalGit(t, repoRoot, "rev-parse", "HEAD")
	if err := state.BeginNativeAllocation(attempt.ID, path); err != nil {
		t.Fatal(err)
	}
	if err := manager.Add(repo, path, branch, baseCommit); err != nil {
		t.Fatal(err)
	}
	identity, err := manager.Inspect(repo, path, branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureNativeWorkspace(attempt.ID, model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: "session",
		Path: path, GitDir: identity.GitDir, CommonDir: identity.CommonDir, Branch: branch}); err != nil {
		t.Fatal(err)
	}
	if err := state.SetAttemptBaseCommit(attempt.ID, baseCommit); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"write report"}, model.WorkspaceFacts{HeadCommit: identity.Head}); err != nil {
		t.Fatal(err)
	}
	reportBody := []byte("complete recovered report\n")
	reportPath := filepath.Join(dataDir, task.ID, "report.md")
	if bound {
		reportPath, err = state.AssignReportDestination(task.ID, attempt.ID, generation, dataDir)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, reportBody, 0o600); err != nil {
		t.Fatal(err)
	}
	checkpointBody := model.Checkpoint{SchemaVersion: 1, Summary: "report complete", Completed: []string{"canonical report written"}, NextSteps: []string{"emit done"}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "report complete", Checkpoint: &checkpointBody}, 1,
		model.WorkspaceFacts{HeadCommit: identity.Head}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordControlFailureMessage(attempt.ID, generation, "terminal handoff failed"); err != nil {
		t.Fatal(err)
	}
	task, _ = state.Task(task.ID)
	attempt, _ = state.Attempt(attempt.ID)
	checkpoint, _ := state.LatestCheckpoint(attempt.ID)
	service := New(config.Config{DataDir: dataDir, WorktreeRoot: worktreeRoot}, state)
	service.environment = environmentCapability{get: func(string) string { return "" }}
	if task.Status != model.TaskStatusBlocked || attempt.Status != model.AttemptStatusBlocked || checkpoint.Producer != "worker" || !strings.Contains(attempt.FailureReason, "terminal handoff failed") {
		t.Fatalf("fixture task=%+v attempt=%+v checkpoint=%+v", task, attempt, checkpoint)
	}
	return service, state, task, attempt, checkpoint, reportBody
}
