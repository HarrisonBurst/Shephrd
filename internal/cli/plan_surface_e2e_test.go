package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

type planSurfaceFixture struct {
	binary      string
	repoRoot    string
	dataDir     string
	database    string
	environment []string
}

func newPlanSurfaceFixture(t *testing.T, piScript string) *planSurfaceFixture {
	t.Helper()
	root := t.TempDir()
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
	binary := filepath.Join(root, "shephrd")
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
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("plan surface e2e\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base"}} {
		command = exec.Command("git", args...)
		command.Dir = repoRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	if piScript != "" {
		if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(piScript), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "config.toml")
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
`, filepath.Join(root, "state.db"), filepath.Join(root, "data"))
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(),
		"SHEPHRD_CONFIG="+configPath,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PI_SESSION_ID=",
		"SHEPHRD_EXECUTABLE="+binary,
		"SHEPHRD_PI_WATCHER_ENABLED=0",
	)
	return &planSurfaceFixture{binary: binary, repoRoot: repoRoot, dataDir: filepath.Join(root, "data"), database: filepath.Join(root, "state.db"), environment: environment}
}

func (f *planSurfaceFixture) run(t *testing.T, extra map[string]string, args ...string) []byte {
	t.Helper()
	stdout, stderr, err := f.output(t, extra, args...)
	if err != nil {
		t.Fatalf("shephrd %v: %s: %v", args, stderr, err)
	}
	return stdout
}

func (f *planSurfaceFixture) runFailure(t *testing.T, extra map[string]string, args ...string) (map[string]string, string) {
	t.Helper()
	stdout, stderr, err := f.output(t, extra, args...)
	if err == nil {
		t.Fatalf("shephrd %v unexpectedly succeeded: %s", args, stdout)
	}
	var diagnostic map[string]string
	_ = json.Unmarshal(stderr, &diagnostic)
	return diagnostic, string(stderr)
}

func (f *planSurfaceFixture) output(t *testing.T, extra map[string]string, args ...string) ([]byte, []byte, error) {
	t.Helper()
	environment := f.environment
	for key, value := range extra {
		environment = append(environment, key+"="+value)
	}
	command := exec.Command(f.binary, args...)
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (f *planSurfaceFixture) taskCount(t *testing.T) int {
	t.Helper()
	state, err := store.Open(f.database)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	tasks, err := state.Tasks(store.TaskFilter{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(tasks)
}

func directSurfacePiScript() string {
	return `#!/bin/sh
set -eu
prompt=""
for arg in "$@"; do prompt="$prompt $arg"; done
task_id=$(printf '%s\n' "$prompt" | grep -oE 'task_[0-9a-f]{12}' | head -1)
[ -n "$task_id" ] || { echo "assignment prompt has no task id" >&2; exit 1; }
data_dir=$(sed -n 's/^data_dir = "\(.*\)".*/\1/p' "$SHEPHRD_CONFIG")
[ -n "$data_dir" ] || { echo "config has no data_dir" >&2; exit 1; }
report=$(printf '%s\n' "$prompt" | sed -n 's/.*Write the investigation report to \(.*\) and use artifact.*/\1/p')
mkdir -p "$(dirname "$report")"
printf 'DIRECT SURFACE E2E REPORT\n' > "$report"
printf '%s\n' '{"type":"session","id":"direct-surface-session"}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"direct surface report ready\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"direct surface report ready\",\"completed\":[\"report written\"],\"next_steps\":[\"emit done\"],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>"}],"stopReason":"stop"}}'
printf '%s\n' "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"<shephrd-event>{\\\"type\\\":\\\"done\\\",\\\"payload\\\":\\\"direct complete\\\",\\\"artifact\\\":\\\"report:$report\\\"}</shephrd-event>\"}],\"stopReason\":\"stop\"}}"
printf '%s\n' '{"type":"agent_end"}'
`
}

func TestCLIDirectTaskOrdinaryPathE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, directSurfacePiScript())
	owner := map[string]string{"PI_SESSION_ID": "direct-surface"}
	fixture.run(t, owner, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	var producer model.Task
	if err := json.Unmarshal(fixture.run(t, owner, "task", "create", "--title", "Direct report producer", "--repo", "demo", "--feature", "direct-producer", "--deliverable", "report", "--acceptance", "report complete", "Produce the direct report", "--json"), &producer); err != nil {
		t.Fatal(err)
	}
	fixture.run(t, owner, "worker", "spawn", producer.ID, "--harness", "pi", "--runtime", "headless", "--json")
	waitForCLITask(t, fixture.database, producer.ID, func(task model.Task) bool {
		return task.Status == model.TaskStatusDone && task.Landed && !task.ProcessAlive
	})
	var detail model.TaskDetail
	if err := json.Unmarshal(fixture.run(t, owner, "task", "inspect", producer.ID, "--json"), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Attempts) != 1 {
		t.Fatalf("attempts = %+v", detail.Attempts)
	}
	state, err := store.Open(fixture.database)
	if err != nil {
		t.Fatal(err)
	}
	doneMessage, err := state.AcceptedDoneMessage(producer.ID, detail.Attempts[0].ID)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	artifact, err := state.VerifiedArtifactForDoneMessage(doneMessage.ID)
	plans, planErr := state.Plans("driver:pi:direct-surface")
	state.Close()
	if err != nil || artifact == nil {
		t.Fatalf("artifact = %+v err=%v", artifact, err)
	}
	if planErr != nil || len(plans) != 0 {
		t.Fatalf("direct work used planning state: plans=%+v err=%v", plans, planErr)
	}
	var successor model.Task
	if err := json.Unmarshal(fixture.run(t, owner, "task", "create", "--title", "Direct successor", "--repo", "demo", "--feature", "direct-successor", "--deliverable", "report", "--acceptance", "applied", "--with-report-from", producer.ID, "Apply the direct report findings", "--json"), &successor); err != nil {
		t.Fatal(err)
	}
	var successorDetail model.TaskDetail
	if err := json.Unmarshal(fixture.run(t, owner, "task", "inspect", successor.ID, "--json"), &successorDetail); err != nil {
		t.Fatal(err)
	}
	if len(successorDetail.Inputs) != 1 || successorDetail.Inputs[0].ArtifactID != artifact.ID || successorDetail.Inputs[0].ProducerTaskID != producer.ID {
		t.Fatalf("direct report input = %+v artifact=%s", successorDetail.Inputs, artifact.ID)
	}
}

func TestCLIPlanRestartSurvivesDriverRestartE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, "")
	before := map[string]string{"PI_SESSION_ID": "plan-restart"}
	fixture.run(t, before, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	var plan model.Plan
	if err := json.Unmarshal(fixture.run(t, before, "plan", "create", "restart", "--json"), &plan); err != nil {
		t.Fatal(err)
	}
	var producer, successor model.PlanItem
	if err := json.Unmarshal(fixture.run(t, before, "plan", "add", "--title", "Investigate", plan.ID, "Investigate", "--repo", "demo", "--feature", "restart-producer", "--acceptance", "report complete", "--deliverable", "report", "--position", "1", "--json"), &producer); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture.run(t, before, "plan", "add", "--title", "Implement", plan.ID, "Implement", "--repo", "demo", "--feature", "restart-successor", "--acceptance", "checks pass", "--position", "2", "--json"), &successor); err != nil {
		t.Fatal(err)
	}
	fixture.run(t, before, "plan", "requires", "add", plan.ID, successor.ID, producer.ID, "--json")
	fixture.run(t, before, "plan", "report", "add", plan.ID, successor.ID, producer.ID, "--json")
	var dispatch model.PlanDispatch
	if err := json.Unmarshal(fixture.run(t, before, "plan", "dispatch", plan.ID, producer.ID, "--json"), &dispatch); err != nil {
		t.Fatal(err)
	}
	assertQueuedWithoutAttempt(t, fixture.database, dispatch.Task.ID)
	completeCLIReport(t, fixture.database, fixture.dataDir, dispatch.Task.ID, "RESTART E2E REPORT\n")
	fixture.run(t, before, "plan", "report", "select", plan.ID, successor.ID, producer.ID, "--json")
	fixture.run(t, before, "plan", "annotate", plan.ID, successor.ID, "Restart must not lose this judgment", "--next-action", "dispatch after restart", "--json")
	tasksBefore := fixture.taskCount(t)

	var shown model.Plan
	if err := json.Unmarshal(fixture.run(t, before, "plan", "show", plan.ID, "--json"), &shown); err != nil {
		t.Fatal(err)
	}
	shownSuccessor := findCLIPlanItem(t, shown, successor.ID)
	if shownSuccessor.DispatchedTaskID != "" || !shownSuccessor.Readiness.Ready || len(shownSuccessor.Reports) != 1 || shownSuccessor.Reports[0].ArtifactID == "" {
		t.Fatalf("restarted successor = %+v", shownSuccessor)
	}
	if shownSuccessor.LatestAnnotation == nil || shownSuccessor.LatestAnnotation.Judgment != "Restart must not lose this judgment" || shownSuccessor.LatestAnnotation.DriverID != "driver:pi:plan-restart" {
		t.Fatalf("restarted annotation = %+v", shownSuccessor.LatestAnnotation)
	}
	if fixture.taskCount(t) != tasksBefore {
		t.Fatalf("restart mutated tasks: before=%d after=%d", tasksBefore, fixture.taskCount(t))
	}
	var listed []model.Plan
	if err := json.Unmarshal(fixture.run(t, before, "plan", "ls", "--json"), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != plan.ID || listed[0].DriverID != "driver:pi:plan-restart" {
		t.Fatalf("restarted list = %+v", listed)
	}
}

func TestCLIPlanAdoptionTransfersOwnershipE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, "")
	from := map[string]string{"PI_SESSION_ID": "adopt-a"}
	to := map[string]string{"PI_SESSION_ID": "adopt-b"}
	fixture.run(t, from, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	var plan model.Plan
	if err := json.Unmarshal(fixture.run(t, from, "plan", "create", "adoption", "--json"), &plan); err != nil {
		t.Fatal(err)
	}
	var producer, successor model.PlanItem
	if err := json.Unmarshal(fixture.run(t, from, "plan", "add", "--title", "Investigate", plan.ID, "Investigate", "--repo", "demo", "--feature", "adopt-producer", "--acceptance", "report complete", "--deliverable", "report", "--position", "1", "--json"), &producer); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture.run(t, from, "plan", "add", "--title", "Implement", plan.ID, "Implement", "--repo", "demo", "--feature", "adopt-successor", "--acceptance", "checks pass", "--position", "2", "--json"), &successor); err != nil {
		t.Fatal(err)
	}
	fixture.run(t, from, "plan", "requires", "add", plan.ID, successor.ID, producer.ID, "--json")
	fixture.run(t, from, "plan", "report", "add", plan.ID, successor.ID, producer.ID, "--json")
	var dispatch model.PlanDispatch
	if err := json.Unmarshal(fixture.run(t, from, "plan", "dispatch", plan.ID, producer.ID, "--json"), &dispatch); err != nil {
		t.Fatal(err)
	}
	completeCLIReport(t, fixture.database, fixture.dataDir, dispatch.Task.ID, "ADOPTION E2E REPORT\n")
	fixture.run(t, from, "plan", "annotate", plan.ID, successor.ID, "Adopted successor stays ready", "--driver-id", "driver:pi:adopt-a", "--json")
	var adopted model.Plan
	if err := json.Unmarshal(fixture.run(t, from, "plan", "adopt", plan.ID, "--driver-id", "driver:pi:adopt-a", "--new-driver-id", "driver:pi:adopt-b", "--json"), &adopted); err != nil {
		t.Fatal(err)
	}
	if adopted.DriverID != "driver:pi:adopt-b" {
		t.Fatalf("adopted owner = %s", adopted.DriverID)
	}
	var history []model.Annotation
	if err := json.Unmarshal(fixture.run(t, to, "plan", "annotations", plan.ID, successor.ID, "--json"), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].DriverID != "driver:pi:adopt-a" || history[0].Judgment != "Adopted successor stays ready" {
		t.Fatalf("adopted annotation history = %+v", history)
	}
	if _, stderr := fixture.runFailure(t, from, "plan", "edit", plan.ID, successor.ID, "--title", "old owner edit", "--driver-id", "driver:pi:adopt-a"); !strings.Contains(stderr, "does not exist for driver") {
		t.Fatalf("old owner edit stderr = %s", stderr)
	}
	var adoptedDispatch model.PlanDispatch
	if err := json.Unmarshal(fixture.run(t, to, "plan", "dispatch", plan.ID, successor.ID, "--json"), &adoptedDispatch); err != nil {
		t.Fatal(err)
	}
	if adoptedDispatch.Task.ID == "" || adoptedDispatch.Item.Reports[0].ArtifactID == "" {
		t.Fatalf("new owner dispatch = %+v", adoptedDispatch)
	}
	assertQueuedWithoutAttempt(t, fixture.database, adoptedDispatch.Task.ID)
	if _, stderr := fixture.runFailure(t, from, "plan", "dispatch", plan.ID, successor.ID, "--driver-id", "driver:pi:adopt-a"); !strings.Contains(stderr, "does not exist for driver") {
		t.Fatalf("old owner dispatch stderr = %s", stderr)
	}
}

func TestCLIPlanAmbiguityFailsClosedE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, "")
	owner := map[string]string{"PI_SESSION_ID": "ambig"}
	fixture.run(t, owner, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	var plan model.Plan
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "create", "ambiguity", "--json"), &plan); err != nil {
		t.Fatal(err)
	}
	var producer, successor model.PlanItem
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "add", "--title", "Investigate", plan.ID, "Investigate", "--repo", "demo", "--feature", "ambig-producer", "--acceptance", "report complete", "--deliverable", "report", "--position", "1", "--json"), &producer); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "add", "--title", "Implement", plan.ID, "Implement", "--repo", "demo", "--feature", "ambig-successor", "--acceptance", "checks pass", "--position", "2", "--json"), &successor); err != nil {
		t.Fatal(err)
	}
	fixture.run(t, owner, "plan", "requires", "add", plan.ID, successor.ID, producer.ID, "--json")
	fixture.run(t, owner, "plan", "report", "add", plan.ID, successor.ID, producer.ID, "--json")
	var dispatch model.PlanDispatch
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "dispatch", plan.ID, producer.ID, "--json"), &dispatch); err != nil {
		t.Fatal(err)
	}
	completeCLIReport(t, fixture.database, fixture.dataDir, dispatch.Task.ID, "AMBIGUOUS E2E REPORT\n")
	tasksBefore := fixture.taskCount(t)

	var disclosed []model.PlanSummary
	if err := json.Unmarshal(fixture.run(t, nil, "plan", "ls", "--all-drivers", "--json"), &disclosed); err != nil {
		t.Fatal(err)
	}
	if len(disclosed) != 1 || disclosed[0].ID != plan.ID || disclosed[0].Ownership != "unowned" {
		t.Fatalf("ownerless disclosure = %+v", disclosed)
	}

	state, err := store.Open(fixture.database)
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.Task(dispatch.Task.ID)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	artifact, err := state.EligibleVerifiedReport(dispatch.Task.ID)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	if _, err := state.AcceptedDoneMessage(dispatch.Task.ID, task.CurrentAttemptID); err != nil {
		state.Close()
		t.Fatal(err)
	}
	attempt, err := state.Attempt(task.CurrentAttemptID)
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", fixture.database+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// Duplicate the producer's eligible verified report so no selection can be unique.
	if _, err := database.Exec(`INSERT INTO messages(task_id, attempt_id, direction, type, payload, artifact_ref, stale, wake, source_cursor, run_generation, checkpoint_json, created_at) VALUES(?, ?, 'worker-to-driver', 'done', 'duplicate lineage', ?, 0, 1, ?, ?, '', ?)`,
		dispatch.Task.ID, attempt.ID, artifact.OriginalRef, attempt.Cursor+1, attempt.RunGeneration, time.Now().UTC().Format("2006-01-02T15:04:05.000Z")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO verified_artifacts(id, producer_task_id, producer_attempt_id, done_message_id, kind, original_ref, sha256, size_bytes, snapshot_path, verified_at, accepted_event_id, accepted_event_name, accepted_event_version)
		SELECT 'artifact_dup0000000000', v.producer_task_id, v.producer_attempt_id, m.id, v.kind, v.original_ref, v.sha256, v.size_bytes, v.snapshot_path, v.verified_at, '', '', 0
		FROM verified_artifacts v JOIN messages m ON m.task_id=v.producer_task_id WHERE m.type='done' AND m.payload='duplicate lineage'`); err != nil {
		t.Fatal(err)
	}
	if _, stderr := fixture.runFailure(t, owner, "plan", "report", "select", plan.ID, successor.ID, producer.ID); !strings.Contains(stderr, "no unique eligible verified report artifact") {
		t.Fatalf("ambiguous select stderr = %s", stderr)
	}
	if _, stderr := fixture.runFailure(t, owner, "plan", "dispatch", plan.ID, successor.ID); !strings.Contains(stderr, "no unique eligible verified report artifact") {
		t.Fatalf("ambiguous dispatch stderr = %s", stderr)
	}
	var shown model.Plan
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "show", plan.ID, "--json"), &shown); err != nil {
		t.Fatal(err)
	}
	shownSuccessor := findCLIPlanItem(t, shown, successor.ID)
	if shownSuccessor.DispatchedTaskID != "" || len(shownSuccessor.Reports) != 1 || shownSuccessor.Reports[0].ArtifactID != "" {
		t.Fatalf("ambiguous dispatch mutated plan state: %+v", shownSuccessor)
	}
	if fixture.taskCount(t) != tasksBefore {
		t.Fatalf("ambiguous dispatch created tasks: before=%d after=%d", tasksBefore, fixture.taskCount(t))
	}
}

func TestCLIPlanFailClosedSurfacesE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, "")
	owner := map[string]string{"PI_SESSION_ID": "fail-closed"}
	stranger := map[string]string{"PI_SESSION_ID": "fail-closed-stranger"}
	fixture.run(t, owner, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	var plan, other model.Plan
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "create", "fail-closed", "--json"), &plan); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "create", "other", "--json"), &other); err != nil {
		t.Fatal(err)
	}
	var x, y, foreign model.PlanItem
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "add", "--title", "First", plan.ID, "First", "--repo", "demo", "--feature", "fc-x", "--acceptance", "done", "--position", "1", "--json"), &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "add", "--title", "Second", plan.ID, "Second", "--repo", "demo", "--feature", "fc-y", "--acceptance", "done", "--position", "2", "--json"), &y); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "add", "--title", "Foreign", other.ID, "Foreign", "--repo", "demo", "--feature", "fc-foreign", "--acceptance", "done", "--json"), &foreign); err != nil {
		t.Fatal(err)
	}
	fixture.run(t, owner, "plan", "requires", "add", plan.ID, x.ID, y.ID, "--json")
	if _, stderr := fixture.runFailure(t, owner, "plan", "requires", "add", plan.ID, y.ID, x.ID); !strings.Contains(stderr, "cycle") {
		t.Fatalf("cycle stderr = %s", stderr)
	}
	if _, stderr := fixture.runFailure(t, owner, "plan", "requires", "add", plan.ID, y.ID, foreign.ID); !strings.Contains(stderr, "same plan") {
		t.Fatalf("cross-plan stderr = %s", stderr)
	}
	var dispatch model.PlanDispatch
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "dispatch", plan.ID, y.ID, "--json"), &dispatch); err != nil {
		t.Fatal(err)
	}
	if _, stderr := fixture.runFailure(t, owner, "plan", "dispatch", plan.ID, y.ID); !strings.Contains(stderr, "already dispatched") {
		t.Fatalf("re-dispatch stderr = %s", stderr)
	}
	if _, stderr := fixture.runFailure(t, stranger, "plan", "dispatch", plan.ID, x.ID); !strings.Contains(stderr, "does not exist for driver") {
		t.Fatalf("stranger dispatch stderr = %s", stderr)
	}
	if _, stderr := fixture.runFailure(t, owner, "plan", "dispatch", plan.ID, "plan_item_nonexistent"); !strings.Contains(stderr, "does not exist in plan") {
		t.Fatalf("missing item stderr = %s", stderr)
	}
	var shown model.Plan
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "show", plan.ID, "--json"), &shown); err != nil {
		t.Fatal(err)
	}
	shownX := findCLIPlanItem(t, shown, x.ID)
	shownY := findCLIPlanItem(t, shown, y.ID)
	if shownX.DispatchedTaskID != "" || shownY.DispatchedTaskID != dispatch.Task.ID || len(shownX.Prerequisites) != 1 {
		t.Fatalf("fail-closed plan state = x:%+v y:%+v", shownX, shownY)
	}
	if fixture.taskCount(t) != 1 {
		t.Fatalf("fail-closed created unexpected tasks: %d", fixture.taskCount(t))
	}
}

func TestCLIPlanContinuationBudgetBoundsDispatchE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, "")
	owner := map[string]string{"PI_SESSION_ID": "budget"}
	fixture.run(t, owner, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	var plan model.Plan
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "create", "budget", "--json"), &plan); err != nil {
		t.Fatal(err)
	}
	producerIDs := make([]string, 0, model.MaxTaskReportInputs+1)
	for index := 1; index <= model.MaxTaskReportInputs+1; index++ {
		var producer model.PlanItem
		if err := json.Unmarshal(fixture.run(t, owner, "plan", "add", "--title", fmt.Sprintf("Producer %d", index), plan.ID, fmt.Sprintf("Produce report %d", index), "--repo", "demo", "--feature", fmt.Sprintf("budget-producer-%d", index), "--acceptance", "report complete", "--deliverable", "report", "--json"), &producer); err != nil {
			t.Fatal(err)
		}
		producerIDs = append(producerIDs, producer.ID)
	}
	successorArgs := func(ids []string) []string {
		args := []string{"plan", "add", "--title", "At budget", plan.ID, "Synthesize the reports", "--repo", "demo", "--feature", "budget-successor", "--acceptance", "done", "--json"}
		for _, id := range ids {
			args = append(args, "--requires", id)
		}
		for _, id := range ids {
			args = append(args, "--report", id)
		}
		return args
	}
	if _, stderr := fixture.runFailure(t, owner, successorArgs(producerIDs)...); !strings.Contains(stderr, fmt.Sprintf("at most %d relations", model.MaxTaskReportInputs)) {
		t.Fatalf("over-budget creation stderr = %s", stderr)
	}
	var successor model.PlanItem
	if err := json.Unmarshal(fixture.run(t, owner, successorArgs(producerIDs[:model.MaxTaskReportInputs])...), &successor); err != nil {
		t.Fatal(err)
	}
	if len(successor.Prerequisites) != model.MaxTaskReportInputs || len(successor.Reports) != model.MaxTaskReportInputs {
		t.Fatalf("budget successor = %+v", successor)
	}
	fixture.run(t, owner, "plan", "requires", "add", plan.ID, successor.ID, producerIDs[model.MaxTaskReportInputs], "--json")
	if _, stderr := fixture.runFailure(t, owner, "plan", "report", "add", plan.ID, successor.ID, producerIDs[model.MaxTaskReportInputs]); !strings.Contains(stderr, fmt.Sprintf("at most %d report inputs", model.MaxTaskReportInputs)) {
		t.Fatalf("over-budget report add stderr = %s", stderr)
	}
	var shown model.Plan
	if err := json.Unmarshal(fixture.run(t, owner, "plan", "show", plan.ID, "--json"), &shown); err != nil {
		t.Fatal(err)
	}
	shownSuccessor := findCLIPlanItem(t, shown, successor.ID)
	if shownSuccessor.Readiness.Ready || len(shownSuccessor.Reports) != model.MaxTaskReportInputs || len(shownSuccessor.Prerequisites) != model.MaxTaskReportInputs+1 || hasCLIReason(shownSuccessor.Readiness, "too_many_reports") {
		t.Fatalf("budget readiness = %+v", shownSuccessor)
	}
	if _, stderr := fixture.runFailure(t, owner, "plan", "dispatch", plan.ID, successor.ID); !strings.Contains(stderr, "not ready") || strings.Contains(stderr, "too_many_reports") {
		t.Fatalf("budget dispatch stderr = %s", stderr)
	}
	if fixture.taskCount(t) != 0 {
		t.Fatalf("budget dispatch created tasks: %d", fixture.taskCount(t))
	}
}
