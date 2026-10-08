package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
)

func TestCLIExternalDeliveryAttestationFollowupSquashE2E(t *testing.T) {
	root := t.TempDir()
	repoRoot := filepath.Join(root, "repo")
	workerRoot := filepath.Join(root, "worker")
	binDir := filepath.Join(root, "bin")
	binary := filepath.Join(root, "shephrd")
	databasePath := filepath.Join(root, "state.db")
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "config.toml")
	releaseLog := filepath.Join(root, "release.log")
	for _, path := range []string{repoRoot, binDir, dataDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = projectRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Shephrd E2E binary: %s: %v", output, err)
	}
	observerBinary := filepath.Join(binDir, "shephrd-github-observer")
	observerBuild := exec.Command("go", "build", "-o", observerBinary, "./cmd/shephrd-github-observer")
	observerBuild.Dir = projectRoot
	if output, err := observerBuild.CombinedOutput(); err != nil {
		t.Fatalf("build GitHub observer E2E binary: %s: %v", output, err)
	}
	observerBody, err := os.ReadFile(observerBinary)
	if err != nil {
		t.Fatal(err)
	}
	observerSum := sha256.Sum256(observerBody)
	observerSHA := hex.EncodeToString(observerSum[:])
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	runGitE2E(t, repoRoot, "init", "-b", "main")
	runGitE2E(t, repoRoot, "config", "user.name", "Test")
	runGitE2E(t, repoRoot, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repoRoot, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitE2E(t, repoRoot, "add", "base.txt")
	runGitE2E(t, repoRoot, "commit", "-m", "base")
	base := runGitE2E(t, repoRoot, "rev-parse", "HEAD")
	runGitE2E(t, repoRoot, "worktree", "add", "-b", "shephrd/external-e2e", workerRoot, "main")
	if err := os.WriteFile(filepath.Join(workerRoot, "worker.txt"), []byte("worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitE2E(t, workerRoot, "add", "worker.txt")
	runGitE2E(t, workerRoot, "commit", "-m", "worker")
	sealed := runGitE2E(t, workerRoot, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repoRoot, "main-review.txt"), []byte("main review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitE2E(t, repoRoot, "add", "main-review.txt")
	runGitE2E(t, repoRoot, "commit", "-m", "main review")
	runGitE2E(t, repoRoot, "checkout", "-b", "external-review", sealed)
	if err := os.WriteFile(filepath.Join(repoRoot, "fix-1.txt"), []byte("fix 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitE2E(t, repoRoot, "add", "fix-1.txt")
	runGitE2E(t, repoRoot, "commit", "-m", "fix 1")
	fix1 := runGitE2E(t, repoRoot, "rev-parse", "HEAD")
	runGitE2E(t, repoRoot, "merge", "--no-ff", "-m", "merge main", "main")
	mergeMain := runGitE2E(t, repoRoot, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repoRoot, "fix-2.txt"), []byte("fix 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitE2E(t, repoRoot, "add", "fix-2.txt")
	runGitE2E(t, repoRoot, "commit", "-m", "fix 2")
	head := runGitE2E(t, repoRoot, "rev-parse", "HEAD")
	runGitE2E(t, repoRoot, "checkout", "main")
	runGitE2E(t, repoRoot, "merge", "--squash", "external-review")
	runGitE2E(t, repoRoot, "commit", "-m", "squash review")
	mergeCommit := runGitE2E(t, repoRoot, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repoRoot, "after.txt"), []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitE2E(t, repoRoot, "add", "after.txt")
	runGitE2E(t, repoRoot, "commit", "-m", "after merge")
	defaultHead := runGitE2E(t, repoRoot, "rev-parse", "HEAD")
	if command := exec.Command(realGit, "-C", repoRoot, "merge-base", "--is-ancestor", sealed, head); command.Run() != nil {
		t.Fatal("sealed worker commit is not an ancestor of final PR head fixture")
	}
	if command := exec.Command(realGit, "-C", repoRoot, "merge-base", "--is-ancestor", sealed, mergeCommit); command.Run() == nil {
		t.Fatal("squash merge fixture retained sealed commit ancestry")
	}
	if command := exec.Command(realGit, "-C", repoRoot, "merge-base", "--is-ancestor", mergeCommit, defaultHead); command.Run() != nil {
		t.Fatal("merge fixture is not reachable from default head")
	}
	runGitE2E(t, repoRoot, "remote", "add", "origin", "https://github.com/acme/demo.git")
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: repoRoot, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:pi:e2e-driver", RepoID: repo.ID, FeatureKey: "external-e2e", Objective: "external delivery", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", workerRoot, "lease-e2e", "shephrd/external-e2e"); err != nil {
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
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "done", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{HeadCommit: sealed}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "branch:shephrd/external-e2e"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	gitScript := fmt.Sprintf("#!/bin/bash\nif [[ \"${1:-}\" == ls-remote ]]; then exit 2; fi\nexec %q \"$@\"\n", realGit)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(gitScript), 0o700); err != nil {
		t.Fatal(err)
	}
	ghScript := fmt.Sprintf(`#!/bin/bash
set -eu
args="$*"
if [[ "$args" == *"graphql"* ]]; then
cat <<'JSON'
[{"data":{"repository":{"id":"repo-node","nameWithOwner":"acme/demo","url":"https://github.com/acme/demo","defaultBranchRef":{"name":"main","target":{"oid":"%s"}},"pullRequest":{"id":"pr-node","url":"https://github.com/acme/demo/pull/42","state":"MERGED","mergedAt":"2026-08-13T07:25:38Z","baseRefName":"main","headRefName":"external-review","headRefOid":"%s","baseRepository":{"nameWithOwner":"acme/demo"},"mergeCommit":{"oid":"%s"},"body":"SENSITIVE_PR_BODY","commits":{"nodes":[{"commit":{"oid":"%s"}},{"commit":{"oid":"%s"}},{"commit":{"oid":"%s"}},{"commit":{"oid":"%s"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}]
JSON
elif [[ "$args" == *"%s...%s"* ]]; then
printf '{"status":"ahead","merge_base_commit":{"sha":"%s"}}\n'
elif [[ "$args" == *"%s...%s"* ]]; then
printf '{"status":"ahead","merge_base_commit":{"sha":"%s"}}\n'
else
exit 2
fi
`, defaultHead, head, mergeCommit, sealed, fix1, mergeMain, head, sealed, head, sealed, mergeCommit, defaultHead, mergeCommit)
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(ghScript), 0o700); err != nil {
		t.Fatal(err)
	}
	treehouseScript := fmt.Sprintf("#!/bin/bash\nprintf '%%s\\n' \"$*\" >> %q\n", releaseLog)
	if err := os.WriteFile(filepath.Join(binDir, "treehouse"), []byte(treehouseScript), 0o700); err != nil {
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

[github_observation]
command = [%q]
sha256 = %q

[pi_watcher]
enabled = false
poll_min = "1s"
poll_max = "15s"
`, databasePath, dataDir, filepath.Dir(workerRoot), observerBinary, observerSHA)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "SHEPHRD_CONFIG="+configPath, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "PI_SESSION_ID=e2e-driver", "SHEPHRD_ATTEMPT_ID=", "SHEPHRD_RUN_GENERATION=", "SHEPHRD_BRIDGE_ATTEMPT_ID=", "SHEPHRD_CLAUDE_BRIDGE_ATTEMPT_ID=")
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
	diagnostic := runFailure("task", "attest-delivery", task.ID, "--pr", "https://github.com/acme/demo/pull/042", "--commit", sealed, "--json")
	if diagnostic["error_kind"] != "pr_url_invalid" || diagnostic["error"] == "" {
		t.Fatalf("diagnostic = %+v", diagnostic)
	}
	var ordinary map[string]any
	if err := json.Unmarshal(run("task", "verify-delivery", task.ID, "--json"), &ordinary); err != nil {
		t.Fatal(err)
	}
	if ordinary["landed"] != false {
		t.Fatalf("ordinary verification substituted external PR: %+v", ordinary)
	}
	var attested control.AttestDeliveryResult
	if err := json.Unmarshal(run("task", "attest-delivery", task.ID, "--pr", "https://github.com/acme/demo/pull/42", "--commit", sealed, "--json"), &attested); err != nil {
		t.Fatal(err)
	}
	if attested.LandingProven || attested.PRHeadCommit != head || attested.MergeCommit != mergeCommit || attested.CommitRelation != "ancestor" {
		t.Fatalf("attestation = %+v", attested)
	}
	if _, err := os.Stat(releaseLog); !os.IsNotExist(err) {
		t.Fatalf("attestation invoked Treehouse: %v", err)
	}
	var detail model.TaskDetail
	if err := json.Unmarshal(run("task", "inspect", task.ID, "--json"), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Task.ArtifactRef != "branch:shephrd/external-e2e" || detail.Task.Landed || len(detail.ExternalDeliveryAttestations) != 1 {
		t.Fatalf("detail after attestation = %+v attestations=%+v", detail.Task, detail.ExternalDeliveryAttestations)
	}
	digest := detail.ExternalDeliveryAttestations[0].EvidenceDigest
	if len(digest) != 64 || digest != strings.ToLower(digest) {
		t.Fatalf("attestation evidence digest = %q", digest)
	}
	var verified map[string]any
	if err := json.Unmarshal(run("task", "verify-delivery", task.ID, "--json"), &verified); err != nil {
		t.Fatal(err)
	}
	landing, _ := verified["landing"].(map[string]any)
	worktree, _ := verified["worktree"].(map[string]any)
	if verified["landed"] != true || landing["kind"] != "github_pr_attested_ancestry" || worktree["release_state"] != "released" {
		t.Fatalf("verified = %+v", verified)
	}
	run("task", "verify-delivery", task.ID, "--json")
	if _, err := os.Stat(workerRoot); !os.IsNotExist(err) {
		t.Fatalf("verified native worktree still exists: %v", err)
	}
	if _, err := os.Stat(releaseLog); !os.IsNotExist(err) {
		t.Fatalf("verification invoked retired Treehouse runtime: %v", err)
	}
	for _, path := range []string{databasePath, databasePath + "-wal"} {
		body, err := os.ReadFile(path)
		if err == nil && bytes.Contains(body, []byte("SENSITIVE_PR_BODY")) {
			t.Fatalf("private PR body persisted in %s", path)
		}
	}
	if status := runGitE2E(t, repoRoot, "status", "--porcelain"); status != "" {
		t.Fatalf("registered root mutated: %q", status)
	}
	if got := runGitE2E(t, repoRoot, "rev-parse", "main"); got != defaultHead {
		t.Fatalf("default branch changed: got %s want %s", got, defaultHead)
	}
}

func runGitE2E(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %s: %v", args, dir, output, err)
	}
	return strings.TrimSpace(string(output))
}
