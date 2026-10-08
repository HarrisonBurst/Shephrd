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
	"time"

	"shephrd/internal/brief"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestCLIAnnotationCrashReplacementPRLoopE2E(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "shephrd")
	binDir := filepath.Join(root, "bin")
	databasePath := filepath.Join(root, "state.db")
	configPath := filepath.Join(root, "config.toml")
	dataDir := filepath.Join(root, "data")
	worktree := filepath.Join(root, "worktree")
	for _, path := range []string{binDir, worktree} {
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
	sealedCommit := strings.Repeat("a", 40)
	approvedPayload := "Apply approved review findings for sealed commit " + sealedCommit + ": finding 2 rejects stale revisions; finding 5 preserves explicit driver control"
	nextAction := "worker send the exact approved-finding payload, then require re-review"
	expectedPrompt := approvedPayload + "\n" + brief.ProjectGuidance("", true)
	pi := fmt.Sprintf(`#!/bin/sh
for prompt do :; done
expected='%s'
if [ "$prompt" != "$expected" ]; then
  exit 90
fi
applied='%s'
printf '%%s\n' '{"type":"session","id":"replacement-session"}'
printf '%%s\n' "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"<shephrd-event>{\\\"type\\\":\\\"checkpoint\\\",\\\"payload\\\":\\\"exact follow-up applied\\\",\\\"checkpoint\\\":{\\\"schema_version\\\":1,\\\"summary\\\":\\\"exact follow-up applied\\\",\\\"completed\\\":[\\\"Applied exact approved payload: $applied\\\"],\\\"next_steps\\\":[\\\"request re-review\\\"],\\\"decisions\\\":[],\\\"changed_paths\\\":[],\\\"checks\\\":[],\\\"blockers\\\":[]}}</shephrd-event>\"}],\"stopReason\":\"stop\"}}"
printf '%%s\n' "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"<shephrd-event>{\\\"type\\\":\\\"question\\\",\\\"payload\\\":\\\"Re-review required after applying: $applied\\\"}</shephrd-event>\"}],\"stopReason\":\"stop\"}}"
printf '%%s\n' '{"type":"agent_end"}'
`, strings.ReplaceAll(expectedPrompt, "'", `'\''`), approvedPayload)
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(pi), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := fmt.Sprintf("default_harness = \"pi\"\nworker_runtime = \"headless\"\ndatabase_path = %q\ndata_dir = %q\nworktree_root = %q\n", databasePath, dataDir, filepath.Join(root, "worktrees"))
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(root, "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	oldOwner := "driver:pi:crashed"
	newOwner := "driver:pi:replacement"
	review, err := state.CreateTask(model.Task{Title: "Test task", RepoID: repo.ID, FeatureKey: "pr-loop-review", DriverID: oldOwner,
		Objective: "Review sealed implementation", AcceptanceCriteria: "report material findings", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := state.CreateTask(model.Task{Title: "Test task", RepoID: repo.ID, FeatureKey: "pr-loop", DriverID: oldOwner,
		Objective: "Implement reviewed change", AcceptanceCriteria: "review approved", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(implementation.ID, "pi", "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "prior-session", worktree, "lease-pr-loop", "shephrd/pr-loop"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"implement"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "review-ready", NextSteps: []string{"driver reviews sealed commit " + sealedCommit}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "review-ready", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "review sealed commit " + sealedCommit}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, implementation.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	list, err := state.CreatePlan("pr-loop-roadmap", oldOwner)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "Implementation anchor"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	removable, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "Tentative remediation"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	item, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "Re-review replacement commit"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	reviewBody := "Review of sealed commit " + sealedCommit + "\n\nFinding 2: reject stale revisions.\nFinding 5: preserve explicit driver control.\n"
	reviewArtifact := completeCLIReport(t, databasePath, dataDir, review.ID, reviewBody)
	environment := append(os.Environ(), "SHEPHRD_CONFIG="+configPath, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "PI_SESSION_ID=", "SHEPHRD_PI_WATCHER_ENABLED=0", "SHEPHRD_EXECUTABLE="+binary)
	run := func(args ...string) ([]byte, []byte, error) {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = environment
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	}
	runOK := func(args ...string) []byte {
		t.Helper()
		stdout, stderr, err := run(args...)
		if err != nil {
			t.Fatalf("shephrd %v: %s: %v", args, stderr, err)
		}
		return stdout
	}
	var removableDecision model.Annotation
	if err := json.Unmarshal(runOK("plan", "annotate", list.ID, removable.ID, "Tentative remediation can be removed", "--driver-id", oldOwner, "--json"), &removableDecision); err != nil {
		t.Fatal(err)
	}
	runOK("plan", "edit", list.ID, removable.ID, "--objective", "Revised tentative remediation", "--driver-id", oldOwner, "--json")
	runOK("plan", "rm", list.ID, removable.ID, "--driver-id", oldOwner, "--json")
	var afterRemoval model.Plan
	if err := json.Unmarshal(runOK("plan", "show", list.ID, "--driver-id", oldOwner, "--json"), &afterRemoval); err != nil {
		t.Fatal(err)
	}
	if len(afterRemoval.Items) != 2 || afterRemoval.Items[0].ID != anchor.ID || afterRemoval.Items[0].Position != 1 || afterRemoval.Items[1].ID != item.ID || afterRemoval.Items[1].Position != 2 {
		t.Fatalf("list after decided item removal = %+v", afterRemoval.Items)
	}
	var removedHistory []model.Annotation
	if err := json.Unmarshal(runOK("plan", "annotations", list.ID, removable.ID, "--driver-id", oldOwner, "--json"), &removedHistory); err != nil || len(removedHistory) != 1 || removedHistory[0].ID != removableDecision.ID || removedHistory[0].PlanID != list.ID {
		t.Fatalf("removed item history = %+v err=%v", removedHistory, err)
	}
	var taskDecision model.Annotation
	if err := json.Unmarshal(runOK("task", "annotate", implementation.ID, approvedPayload, "--reason", "approved from verified review artifact "+reviewArtifact.ID, "--next-action", nextAction, "--expect-revision", "0", "--driver-id", oldOwner, "--json"), &taskDecision); err != nil {
		t.Fatal(err)
	}
	if taskDecision.Revision != 1 || taskDecision.DriverID != oldOwner {
		t.Fatalf("task decision = %+v", taskDecision)
	}
	var itemDecision model.Annotation
	if err := json.Unmarshal(runOK("plan", "annotate", list.ID, item.ID, "Re-review is required", "--next-action", "create a fresh review task", "--driver-id", oldOwner, "--json"), &itemDecision); err != nil {
		t.Fatal(err)
	}
	var listed []model.Annotation
	if err := json.Unmarshal(runOK("task", "annotations", implementation.ID, "--json"), &listed); err != nil || len(listed) != 1 || listed[0].ID != taskDecision.ID {
		t.Fatalf("task decision list = %+v err=%v", listed, err)
	}
	var shown model.Plan
	if err := json.Unmarshal(runOK("plan", "show", list.ID, "--driver-id", oldOwner, "--json"), &shown); err != nil {
		t.Fatal(err)
	}
	var shownItem *model.PlanItem
	for index := range shown.Items {
		if shown.Items[index].ID == item.ID {
			shownItem = &shown.Items[index]
		}
	}
	if len(shown.Items) != 2 || shownItem == nil || shownItem.LatestAnnotation == nil || shownItem.LatestAnnotation.ID != itemDecision.ID {
		t.Fatalf("plan annotation head = %+v", shown.Items)
	}
	var itemHistory []model.Annotation
	if err := json.Unmarshal(runOK("plan", "annotations", list.ID, item.ID, "--driver-id", oldOwner, "--json"), &itemHistory); err != nil || len(itemHistory) != 1 || itemHistory[0].ID != itemDecision.ID {
		t.Fatalf("item decision list = %+v err=%v", itemHistory, err)
	}
	_, conflictBody, conflictErr := run("task", "annotate", implementation.ID, "stale writer", "--expect-revision", "0", "--driver-id", oldOwner, "--json")
	if conflictErr == nil {
		t.Fatal("same-revision conflict succeeded")
	}
	var conflict map[string]string
	if err := json.Unmarshal(conflictBody, &conflict); err != nil || conflict["error_kind"] != "annotation_revision_conflict" {
		t.Fatalf("conflict response = %s err=%v", conflictBody, err)
	}
	runOK("task", "adopt", implementation.ID, "--driver-id", oldOwner, "--new-driver-id", newOwner, "--json")
	runOK("task", "adopt", review.ID, "--driver-id", oldOwner, "--new-driver-id", newOwner, "--json")
	runOK("plan", "adopt", list.ID, "--driver-id", oldOwner, "--new-driver-id", newOwner, "--json")
	var adoptedRemovedHistory []model.Annotation
	if err := json.Unmarshal(runOK("plan", "annotations", list.ID, removable.ID, "--driver-id", newOwner, "--json"), &adoptedRemovedHistory); err != nil || len(adoptedRemovedHistory) != 1 || adoptedRemovedHistory[0].DriverID != oldOwner {
		t.Fatalf("adopted removed-item history = %+v err=%v", adoptedRemovedHistory, err)
	}
	if _, stderr, err := run("task", "annotate", implementation.ID, "old owner write", "--expect-revision", "1", "--driver-id", oldOwner, "--json"); err == nil || !strings.Contains(string(stderr), "owned by") {
		t.Fatalf("old owner append err=%v stderr=%s", err, stderr)
	}
	var recovered model.TaskDetail
	if err := json.Unmarshal(runOK("task", "inspect", implementation.ID, "--json"), &recovered); err != nil {
		t.Fatal(err)
	}
	if len(recovered.Annotations) != 1 || recovered.Annotations[0].Judgment != approvedPayload || recovered.Annotations[0].Reason != "approved from verified review artifact "+reviewArtifact.ID || recovered.Annotations[0].NextAction != nextAction {
		t.Fatalf("recovered decision = %+v", recovered.Annotations)
	}
	foundSealedQuestion := false
	for _, message := range recovered.Messages {
		if message.Type == "question" && message.Payload == "review sealed commit "+sealedCommit {
			foundSealedQuestion = true
		}
	}
	if !foundSealedQuestion {
		t.Fatalf("sealed commit was not recovered from task inspect: %+v", recovered.Messages)
	}
	recoveredPayload := recovered.Annotations[0].Judgment
	runOK("worker", "send", implementation.ID, recoveredPayload, "--json")
	deadline := time.Now().Add(5 * time.Second)
	var resumedCheckpoint model.AttemptCheckpoint
	var messages []model.Message
	for time.Now().Before(deadline) {
		state, err = store.Open(databasePath)
		if err == nil {
			currentAttempt, attemptErr := state.Attempt(attempt.ID)
			if attemptErr == nil && currentAttempt.RunnerPID == 0 {
				resumedCheckpoint, err = state.LatestCheckpoint(attempt.ID)
				if err == nil && resumedCheckpoint.Producer == "worker" {
					messages, err = state.Messages(implementation.ID)
					if err == nil {
						if _, err = state.VerifiedArtifact(reviewArtifact.ID); err == nil {
							state.Close()
							break
						}
					}
				}
			}
			state.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resumedCheckpoint.Producer != "worker" {
		t.Fatal("replacement worker runner did not persist its checkpoint")
	}
	applied := "Applied exact approved payload: " + approvedPayload
	if len(resumedCheckpoint.Completed) != 1 || resumedCheckpoint.Completed[0] != applied {
		t.Fatalf("resumed checkpoint did not prove exact payload application: %+v", resumedCheckpoint)
	}
	foundFollowUp, foundDerivedQuestion := false, false
	for _, message := range messages {
		if message.Type == "follow-up" && message.Payload == recoveredPayload {
			foundFollowUp = true
		}
		if message.Type == "question" && message.Payload == "Re-review required after applying: "+approvedPayload {
			foundDerivedQuestion = true
		}
	}
	if !foundFollowUp || !foundDerivedQuestion {
		t.Fatalf("exact follow-up evidence missing: follow-up=%t question=%t messages=%+v", foundFollowUp, foundDerivedQuestion, messages)
	}
	var replacementDecision model.Annotation
	if err := json.Unmarshal(runOK("task", "annotate", implementation.ID, "Exact approved payload applied; re-review remains required", "--reason", "worker checkpoint "+fmt.Sprint(resumedCheckpoint.Revision)+" proved the recovered payload", "--next-action", "create a fresh review task for the replacement commit", "--expect-revision", "1", "--driver-id", newOwner, "--json"), &replacementDecision); err != nil {
		t.Fatal(err)
	}
	if replacementDecision.Revision != 2 || replacementDecision.DriverID != newOwner || replacementDecision.NextAction != "create a fresh review task for the replacement commit" {
		t.Fatalf("replacement decision = %+v", replacementDecision)
	}
}
