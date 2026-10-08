package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"shephrd/internal/control"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/worktree"
)

func TestCLIReportRecoveryE2E(t *testing.T) {
	root := t.TempDir()
	repoRoot := filepath.Join(root, "repo")
	worktreeRoot := filepath.Join(root, "worktrees")
	dataDir := filepath.Join(root, "data")
	databasePath := filepath.Join(root, "state.db")
	binary := filepath.Join(root, "shephrd")
	configPath := filepath.Join(root, "config.toml")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate project")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = projectRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s: %v", output, err)
	}
	runGitE2E(t, repoRoot, "init", "-b", "main")
	runGitE2E(t, repoRoot, "config", "user.name", "Test")
	runGitE2E(t, repoRoot, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repoRoot, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitE2E(t, repoRoot, "add", "base.txt")
	runGitE2E(t, repoRoot, "commit", "-m", "base")
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: repoRoot, DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Recover report", DriverID: "driver:pi:e2e-report", RepoID: repo.ID,
		FeatureKey: "report-recovery-e2e", Objective: "recover report", Deliverable: "report"})
	attempt, _ := state.BeginAttempt(task.ID, "pi", "")
	manager := worktree.NewNative(worktreeRoot)
	path, _ := manager.AllocatePath(repo, attempt)
	branch := worktree.BranchName(attempt)
	base := runGitE2E(t, repoRoot, "rev-parse", "HEAD")
	if err := state.BeginNativeAllocation(attempt.ID, path); err != nil {
		t.Fatal(err)
	}
	if err := manager.Add(repo, path, branch, base); err != nil {
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
	generation, _ := state.ReserveRunGeneration(attempt.ID)
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"write report"}, model.WorkspaceFacts{HeadCommit: identity.Head}); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dataDir, task.ID, "report.md")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, []byte("canonical report\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpointBody := model.Checkpoint{SchemaVersion: 1, Summary: "report complete", Completed: []string{"canonical report written"}, NextSteps: []string{"emit done"}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "report complete", Checkpoint: &checkpointBody}, 1, model.WorkspaceFacts{HeadCommit: identity.Head}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordControlFailureMessage(attempt.ID, generation, "terminal protocol failed after report write"); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ := state.LatestCheckpoint(attempt.ID)
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	configBody := fmt.Sprintf(`default_harness = "pi"
worker_runtime = "headless"
database_path = %q
data_dir = %q
worktree_root = %q

[wake]
enabled = false
default_batch = 10
max_batch = 20
claim_ttl = "5m"
claim_ttl_min = "30s"
claim_ttl_max = "30m"
driver_id = ""

[notifications]
enabled = false
details = false
task_per_minute = 2
global_per_minute = 10

[pi_watcher]
enabled = false
poll_min = "1s"
poll_max = "15s"
`, databasePath, dataDir, worktreeRoot)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "SHEPHRD_CONFIG="+configPath, "PI_SESSION_ID=e2e-report",
		"SHEPHRD_ATTEMPT_ID=", "SHEPHRD_RUN_GENERATION=", "SHEPHRD_BRIDGE_ATTEMPT_ID=", "SHEPHRD_CLAUDE_BRIDGE_ATTEMPT_ID=")
	run := func(args ...string) []byte {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = environment
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("shephrd %v: %s: %v", args, stderr.String(), err)
		}
		return stdout.Bytes()
	}
	runFailure := func(args ...string) map[string]string {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = environment
		var stderr bytes.Buffer
		command.Stderr = &stderr
		if err := command.Run(); err == nil {
			t.Fatalf("shephrd %v unexpectedly succeeded", args)
		}
		var diagnostic map[string]string
		if err := json.Unmarshal(stderr.Bytes(), &diagnostic); err != nil {
			t.Fatalf("diagnostic %q: %v", stderr.String(), err)
		}
		return diagnostic
	}
	runHumanFailure := func(args ...string) string {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = environment
		var stderr bytes.Buffer
		command.Stderr = &stderr
		if err := command.Run(); err == nil {
			t.Fatalf("shephrd %v unexpectedly succeeded", args)
		}
		return stderr.String()
	}
	var obligations model.AttentionSnapshot
	if err := json.Unmarshal(run("task", "obligations", "--driver-id", task.DriverID, "--json"), &obligations); err != nil {
		t.Fatal(err)
	}
	if len(obligations.Items) != 1 || obligations.Items[0].Kind != "report_recovery_available" || len(obligations.Items[0].RecoveryCommands) != 1 || !strings.Contains(obligations.Items[0].RecoveryCommands[0], attempt.ID) {
		t.Fatalf("obligations = %+v", obligations.Items)
	}
	diagnostic := runFailure("task", "verify-delivery", task.ID, "--attempt", attempt.ID, "--driver-id", task.DriverID, "--json")
	if diagnostic["error_kind"] != "report_recovery_attestation_required" || !strings.Contains(diagnostic["recovery_command"], attempt.ID) {
		t.Fatalf("verify diagnostic = %+v", diagnostic)
	}
	diagnostic = runFailure("task", "attest-report-recovery", task.ID, "--attempt", attempt.ID, "--run-generation", fmt.Sprint(generation+1),
		"--checkpoint-revision", fmt.Sprint(checkpoint.Revision), "--checkpoint-cursor", fmt.Sprint(checkpoint.SourceCursor), "--reason", "handoff failed", "--json")
	if diagnostic["error_kind"] != "stale_generation" || !strings.Contains(diagnostic["recovery_command"], fmt.Sprintf("--run-generation %d", generation)) {
		t.Fatalf("diagnostic = %+v", diagnostic)
	}
	var attested control.AttestReportRecoveryResult
	if err := json.Unmarshal(run("task", "attest-report-recovery", task.ID, "--attempt", attempt.ID, "--run-generation", fmt.Sprint(generation),
		"--checkpoint-revision", fmt.Sprint(checkpoint.Revision), "--checkpoint-cursor", fmt.Sprint(checkpoint.SourceCursor), "--reason", "handoff failed", "--json"), &attested); err != nil {
		t.Fatal(err)
	}
	if attested.AttestationID == "" || attested.Idempotent || attested.NextCommand == "" || attested.SHA256 == "" || attested.AttemptID != attempt.ID {
		t.Fatalf("attested = %+v", attested)
	}
	verifyCommand := fmt.Sprintf("shephrd task verify-delivery %s --attempt %s --driver-id %s --json", task.ID, attempt.ID, model.ShellQuote(task.DriverID))
	retryCommand := fmt.Sprintf("shephrd worker retry %s --json", task.ID)
	diagnostic = runFailure("worker", "relaunch", task.ID, "--json")
	if diagnostic["error_kind"] != "report_recovery_continuation_forbidden" || diagnostic["verify_command"] != verifyCommand || diagnostic["retry_command"] != retryCommand {
		t.Fatalf("relaunch diagnostic = %+v", diagnostic)
	}
	human := runHumanFailure("worker", "send", task.ID, "continue")
	if !strings.Contains(human, verifyCommand) || !strings.Contains(human, retryCommand) {
		t.Fatalf("send diagnostic = %q", human)
	}
	var verified map[string]any
	if err := json.Unmarshal(run("task", "verify-delivery", task.ID, "--attempt", attempt.ID, "--driver-id", task.DriverID, "--json"), &verified); err != nil {
		t.Fatal(err)
	}
	if verified["landed"] != true || verified["completion_provenance"] != model.CompletionProvenanceReportRecovery {
		t.Fatalf("verified = %+v", verified)
	}
	var detail model.TaskDetail
	if err := json.Unmarshal(run("task", "inspect", task.ID, "--json"), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Task.ClaimedDone || detail.Task.CompletionProvenance != model.CompletionProvenanceReportRecovery || len(detail.ReportRecoveryAttestations) != 1 || detail.Attempts[0].ReleaseState != "released" {
		t.Fatalf("detail = %+v", detail)
	}
}
