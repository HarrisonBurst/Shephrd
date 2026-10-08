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

func TestCLIPlanEditingPrerequisitesTwoReportsDispatchAndExplicitRerunE2E(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	databasePath := filepath.Join(root, "state.db")
	configPath := filepath.Join(root, "config.toml")
	binary := filepath.Join(root, "shephrd")
	binDir := filepath.Join(root, "bin")
	repoRoot := filepath.Join(root, "repo")
	for _, path := range []string{binDir, repoRoot} {
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
	command := exec.Command("git", "init", "-b", "main")
	command.Dir = repoRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", output, err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base"}} {
		command = exec.Command("git", args...)
		command.Dir = repoRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	pi := `#!/bin/bash
set -eu
printf '%s\n' '{"type":"session","id":"retry-session"}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"retry checkpoint\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"retry checkpoint\",\"completed\":[],\"next_steps\":[\"driver decides\"],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>"}],"stopReason":"stop"}}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"blocked\",\"payload\":\"explicit rerun complete\"}</shephrd-event>"}],"stopReason":"stop"}}'
printf '%s\n' '{"type":"agent_end"}'
`
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(pi), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := fmt.Sprintf(`default_harness = "pi"
worker_runtime = "headless"
database_path = %q
data_dir = %q

[wake]
enabled = true
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
`, databasePath, dataDir)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "SHEPHRD_CONFIG="+configPath, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "PI_SESSION_ID=plan-e2e", "SHEPHRD_PI_WATCHER_ENABLED=0")
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
	run("repo", "add", repoRoot, "--name", "demo", "--json")
	var plan model.Plan
	if err := json.Unmarshal(run("plan", "create", "release", "--json"), &plan); err != nil {
		t.Fatal(err)
	}
	var implementation model.PlanItem
	if err := json.Unmarshal(run("plan", "add", "--title", "Implement the release", plan.ID, "Implement release", "--description", "Use both reports", "--json"), &implementation); err != nil {
		t.Fatal(err)
	}
	if implementation.Readiness.Ready || !hasCLIReason(implementation.Readiness, "repo_required") {
		t.Fatalf("initial readiness = %+v", implementation.Readiness)
	}
	if err := json.Unmarshal(run("plan", "edit", plan.ID, implementation.ID, "--title", "Ship the release", "--repo", "demo", "--feature", "implement-release", "--acceptance", "checks pass", "--json"), &implementation); err != nil {
		t.Fatal(err)
	}
	var first, second model.PlanItem
	if err := json.Unmarshal(run("plan", "add", "--title", "Investigate the first risk", plan.ID, "First investigation", "--repo", "demo", "--feature", "first-report", "--acceptance", "report complete", "--deliverable", "report", "--position", "1", "--json"), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(run("plan", "add", "--title", "Investigate the remaining gap", plan.ID, "Gap investigation", "--repo", "demo", "--feature", "gap-report", "--acceptance", "report complete", "--deliverable", "report", "--position", "2", "--json"), &second); err != nil {
		t.Fatal(err)
	}
	for _, prerequisite := range []model.PlanItem{first, second} {
		run("plan", "requires", "add", plan.ID, implementation.ID, prerequisite.ID, "--json")
		run("plan", "report", "add", plan.ID, implementation.ID, prerequisite.ID, "--json")
	}
	var firstDispatch, secondDispatch model.PlanDispatch
	if err := json.Unmarshal(run("plan", "dispatch", plan.ID, first.ID, "--json"), &firstDispatch); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(run("plan", "dispatch", plan.ID, second.ID, "--json"), &secondDispatch); err != nil {
		t.Fatal(err)
	}
	assertQueuedWithoutAttempt(t, databasePath, firstDispatch.Task.ID)
	assertQueuedWithoutAttempt(t, databasePath, secondDispatch.Task.ID)
	if firstDispatch.Task.Title != first.Title || secondDispatch.Task.Title != second.Title {
		t.Fatalf("dispatch titles changed: first=%q/%q second=%q/%q", first.Title, firstDispatch.Task.Title, second.Title, secondDispatch.Task.Title)
	}
	firstArtifact := completeCLIReport(t, databasePath, dataDir, firstDispatch.Task.ID, "FIRST_E2E_REPORT\n")
	secondArtifact := completeCLIReport(t, databasePath, dataDir, secondDispatch.Task.ID, "SECOND_E2E_REPORT\n")
	run("plan", "report", "move", plan.ID, implementation.ID, second.ID, "--position", "1", "--json")
	var persisted model.Plan
	if err := json.Unmarshal(run("plan", "show", plan.ID, "--json"), &persisted); err != nil {
		t.Fatal(err)
	}
	implementation = findCLIPlanItem(t, persisted, implementation.ID)
	if implementation.Title != "Ship the release" {
		t.Fatalf("plan show title = %q", implementation.Title)
	}
	if implementation.Readiness.Ready || implementation.Reports[0].ArtifactID != "" || implementation.Reports[1].ArtifactID != "" || !hasCLIReason(implementation.Readiness, "report_not_selected") {
		t.Fatalf("unselected implementation = %+v", implementation)
	}
	var implementationDispatch model.PlanDispatch
	if err := json.Unmarshal(run("plan", "dispatch", plan.ID, implementation.ID, "--json"), &implementationDispatch); err != nil {
		t.Fatal(err)
	}
	assertQueuedWithoutAttempt(t, databasePath, implementationDispatch.Task.ID)
	if implementationDispatch.Task.Title != implementation.Title || implementationDispatch.Item.Reports[0].ArtifactID != secondArtifact.ID || implementationDispatch.Item.Reports[1].ArtifactID != firstArtifact.ID {
		t.Fatalf("implementation dispatch = %+v", implementationDispatch)
	}
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := state.ReportInputs(implementationDispatch.Task.ID)
	state.Close()
	if err != nil || len(inputs) != 2 || inputs[0].ArtifactID != secondArtifact.ID || inputs[1].ArtifactID != firstArtifact.ID {
		t.Fatalf("ordinary task inputs = %+v err=%v", inputs, err)
	}
	var direct model.Task
	if err := json.Unmarshal(run("task", "create", "--title", "Implement both reports", "--repo", "demo", "--feature", "direct-multi", "--acceptance", "done",
		"--with-report-from", firstDispatch.Task.ID, "--with-report-from", secondDispatch.Task.ID, "Direct multi-report task", "--json"), &direct); err != nil {
		t.Fatal(err)
	}
	var directDetail model.TaskDetail
	if err := json.Unmarshal(run("task", "inspect", direct.ID, "--json"), &directDetail); err != nil {
		t.Fatal(err)
	}
	if directDetail.Task.Title != direct.Title || direct.Title != "Implement both reports" {
		t.Fatalf("task inspect title = %q/%q", directDetail.Task.Title, direct.Title)
	}
	if len(directDetail.Inputs) != 2 || directDetail.Inputs[0].ArtifactID != firstArtifact.ID || directDetail.Inputs[1].ArtifactID != secondArtifact.ID {
		t.Fatalf("repeated --with-report-from order = %+v", directDetail.Inputs)
	}

	var spawnedItem model.PlanItem
	if err := json.Unmarshal(run("plan", "add", plan.ID, "Spawn composed work", "--repo", "demo", "--feature", "spawn-composed", "--acceptance", "worker starts", "--deliverable", "report", "--json"), &spawnedItem); err != nil {
		t.Fatal(err)
	}
	var spawnedDispatch model.PlanDispatch
	if err := json.Unmarshal(run("plan", "dispatch", plan.ID, spawnedItem.ID, "--spawn", "--harness", "pi", "--runtime", "headless", "--json"), &spawnedDispatch); err != nil {
		t.Fatal(err)
	}
	if spawnedDispatch.Spawn == nil || spawnedDispatch.Spawn.Task.ID != spawnedDispatch.Task.ID || spawnedDispatch.Spawn.Attempt.TaskID != spawnedDispatch.Task.ID || spawnedDispatch.Spawn.Attempt.ID == "" || spawnedDispatch.Task.Title != "Spawn composed work" {
		t.Fatalf("composed dispatch = %+v", spawnedDispatch)
	}

	var future model.PlanItem
	if err := json.Unmarshal(run("plan", "add", "--title", "Test item", plan.ID, "Future implementation", "--repo", "demo", "--feature", "future", "--acceptance", "done", "--json"), &future); err != nil {
		t.Fatal(err)
	}
	run("plan", "requires", "add", plan.ID, future.ID, first.ID, "--json")
	run("plan", "report", "add", plan.ID, future.ID, first.ID, "--json")
	run("plan", "report", "select", plan.ID, future.ID, first.ID, "--json")
	var recovery control.SpawnResult
	if err := json.Unmarshal(run("worker", "retry", firstDispatch.Task.ID, "--harness", "pi", "--json"), &recovery); err != nil {
		t.Fatal(err)
	}
	if recovery.Attempt.Number != 2 || recovery.Task.ID != firstDispatch.Task.ID {
		t.Fatalf("recovery = %+v", recovery)
	}
	if err := json.Unmarshal(run("plan", "show", plan.ID, "--json"), &persisted); err != nil {
		t.Fatal(err)
	}
	future = findCLIPlanItem(t, persisted, future.ID)
	if !future.Reports[0].Stale || !hasCLIReason(future.Readiness, "report_selection_stale") || future.Readiness.Ready {
		t.Fatalf("future readiness after explicit rerun = %+v", future)
	}
	if err := json.Unmarshal(run("task", "inspect", direct.ID, "--json"), &directDetail); err != nil {
		t.Fatal(err)
	}
	if len(directDetail.Inputs) != 2 || directDetail.Inputs[0].ArtifactID != firstArtifact.ID || directDetail.Inputs[1].ArtifactID != secondArtifact.ID {
		t.Fatalf("dispatched successor inputs changed after producer retry: %+v", directDetail.Inputs)
	}

	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	otherPlan, err := state.CreatePlan("other driver plan", "driver:other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddPlanItem(otherPlan.ID, model.PlanItem{Title: "Other future work", Objective: "Other"}, model.PlanItemRelations{}); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	var summaries []model.PlanSummary
	if err := json.Unmarshal(run("plan", "ls", "--all-drivers", "--json"), &summaries); err != nil {
		t.Fatal(err)
	}
	byPlan := make(map[string]model.PlanSummary, len(summaries))
	for _, summary := range summaries {
		byPlan[summary.ID] = summary
	}
	if byPlan[plan.ID].Ownership != "owned" || byPlan[plan.ID].DriverID != "driver:pi:plan-e2e" || byPlan[plan.ID].DispatchedTaskCount == 0 || len(byPlan[plan.ID].DispatchedTasks) == 0 {
		t.Fatalf("owned all-driver summary = %+v", byPlan[plan.ID])
	}
	if byPlan[otherPlan.ID].Ownership != "unowned" || byPlan[otherPlan.ID].DriverID != "driver:other" {
		t.Fatalf("unowned all-driver summary = %+v", byPlan[otherPlan.ID])
	}
	humanPlans := string(run("plan", "ls", "--all-drivers"))
	for _, expected := range []string{plan.ID, "release", "owner=driver:pi:plan-e2e", "[owned]", otherPlan.ID, "other driver plan", "owner=driver:other", "[unowned]", "task=" + firstDispatch.Task.ID} {
		if !strings.Contains(humanPlans, expected) {
			t.Fatalf("all-driver human output omitted %q:\n%s", expected, humanPlans)
		}
	}
	var obligations model.AttentionSnapshot
	if err := json.Unmarshal(run("task", "obligations", "--all-drivers", "--json"), &obligations); err != nil {
		t.Fatal(err)
	}
	obligationPlans := make(map[string]string, len(obligations.Plans.Summaries))
	for _, summary := range obligations.Plans.Summaries {
		obligationPlans[summary.ID] = summary.DriverID
	}
	if obligationPlans[plan.ID] != "driver:pi:plan-e2e" || obligationPlans[otherPlan.ID] != "driver:other" {
		t.Fatalf("all-driver obligations omitted exact plan owners: %+v", obligations.Plans.Summaries)
	}
	humanObligations := string(run("task", "obligations", "--all-drivers"))
	for _, expected := range []string{"PLANS", plan.ID, "owner=driver:pi:plan-e2e", otherPlan.ID, "owner=driver:other"} {
		if !strings.Contains(humanObligations, expected) {
			t.Fatalf("all-driver obligations omitted %q:\n%s", expected, humanObligations)
		}
	}
}

func completeCLIReport(t *testing.T, databasePath, dataDir, taskID, body string) model.VerifiedArtifact {
	t.Helper()
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	attempt, err := state.BeginAttempt(taskID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", "/tree", "lease-"+attempt.ID, "branch-"+attempt.ID); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"finish"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "done", NextSteps: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "done", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dataDir, taskID, "report.md")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "report:" + reportPath}, 2, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(body))
	hexDigest := hex.EncodeToString(digest[:])
	snapshotPath := filepath.Join(dataDir, "artifacts", "sha256", hexDigest+".md")
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath, []byte(body), 0o400); err != nil {
		t.Fatal(err)
	}
	artifact, err := state.SetVerifiedReportDelivery(taskID, attempt.ID, message.ID, model.VerifiedArtifact{Kind: "report", OriginalRef: "report:" + reportPath,
		SHA256: hexDigest, SizeBytes: int64(len(body)), SnapshotPath: snapshotPath}, "verified")
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func assertQueuedWithoutAttempt(t *testing.T, databasePath, taskID string) {
	t.Helper()
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	task, err := state.Task(taskID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := state.Attempts(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "queued" || task.CurrentAttemptID != "" || task.ProcessAlive || len(attempts) != 0 {
		t.Fatalf("dispatch performed worker control: task=%+v attempts=%+v", task, attempts)
	}
}

func findCLIPlanItem(t *testing.T, plan model.Plan, itemID string) model.PlanItem {
	t.Helper()
	for _, item := range plan.Items {
		if item.ID == itemID {
			return item
		}
	}
	t.Fatalf("item %s missing from %+v", itemID, plan)
	return model.PlanItem{Title: "Test item"}
}

func hasCLIReason(readiness model.PlanReadiness, code string) bool {
	for _, reason := range readiness.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}
