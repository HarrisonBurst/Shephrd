package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"shephrd/internal/adapter"
	"shephrd/internal/control"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func continuationPiScript() string {
	return `#!/usr/bin/env python3
import json, os, re, sys
prompt = ' '.join(sys.argv[1:])
path = re.search(r'Write the investigation report to (.+?) and use artifact', prompt).group(1)
mode = os.environ.get('SHEPHRD_REPORT_MODE', 'blocked')
body = 'accepted blocked run1 report\n' if mode == 'blocked' else 'continuation report\n'
if mode == 'done' and os.environ.get('SHEPHRD_REPORT_DRAFT'):
    body = 'Reviewed in the current run\n' + open(os.environ['SHEPHRD_REPORT_DRAFT']).read()
collision = os.path.exists(path)
if not collision:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, 'x') as f: f.write(body)
checkpoint = {'schema_version':1, 'summary':'report preserved', 'completed':[], 'next_steps':['continue'], 'decisions':[], 'changed_paths':[], 'checks':[], 'blockers':[]}
terminal = {'type': 'blocked' if mode == 'blocked' else ('question' if collision or mode == 'waiting' else 'done'), 'payload': 'destination collision' if collision else 'report ready'}
if mode != 'waiting': terminal['artifact'] = 'report:'+os.environ.get('SHEPHRD_REPORT_REF', path)
text = ''.join('<shephrd-event>'+json.dumps(e)+'</shephrd-event>' for e in [{'type':'checkpoint','payload':'report preserved','checkpoint':checkpoint}, terminal])
print(json.dumps({'type':'session','id':'continuation-session'}))
print(json.dumps({'type':'message_end','message':{'role':'assistant','content':[{'type':'text','text':text}],'stopReason':'stop'}}))
print(json.dumps({'type':'agent_end'}))
`
}

func TestCLIReportSameAttemptContinuationE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, continuationPiScript())
	owner := map[string]string{"PI_SESSION_ID": "report-continuation"}
	fixture.run(t, owner, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	var task model.Task
	if err := json.Unmarshal(fixture.run(t, owner, "task", "create", "--repo", "demo", "--feature", "continuation", "--deliverable", "report", "Preserve prior report", "--json"), &task); err != nil {
		t.Fatal(err)
	}
	var first control.SpawnResult
	if err := json.Unmarshal(fixture.run(t, owner, "worker", "spawn", task.ID, "--json"), &first); err != nil {
		t.Fatal(err)
	}
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool { return task.Status == model.TaskStatusBlocked && !task.ProcessAlive })
	prior := first.Attempt.ReportPath
	body, err := os.ReadFile(prior)
	if err != nil {
		t.Fatal(err)
	}
	before := inspectContinuation(t, fixture, owner, task.ID)
	owner["SHEPHRD_REPORT_MODE"] = "waiting"
	var second control.SpawnResult
	if err := json.Unmarshal(fixture.run(t, owner, "worker", "relaunch", task.ID, "--json"), &second); err != nil {
		t.Fatal(err)
	}
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool {
		return (task.Status == model.TaskStatusDone || task.Status == model.TaskStatusWaiting) && !task.ProcessAlive
	})
	brief, err := os.ReadFile(second.BriefPath)
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := os.ReadFile(prior)
	if err != nil || string(preserved) != string(body) {
		t.Fatalf("prior report changed: %q err=%v", preserved, err)
	}
	if first.Attempt.ID != second.Attempt.ID || second.Attempt.RunGeneration != 2 {
		t.Fatalf("continuation replaced attempt: first=%+v second=%+v", first.Attempt, second.Attempt)
	}
	if strings.Contains(string(brief), "Write the investigation report to "+prior+" and") {
		t.Fatalf("same-attempt run2 still instructs worker to overwrite accepted blocked run1 destination %s; prior bytes preserved by fixture refusing overwrite", prior)
	}
	if second.Attempt.ReportPath == prior || !strings.Contains(string(brief), "report:"+second.Attempt.ReportPath) {
		t.Fatalf("run2 destination mismatch: %+v", second.Attempt)
	}
	owner["SHEPHRD_REPORT_MODE"] = "done"
	fixture.run(t, owner, "worker", "send", task.ID, "Finish using this run's destination", "--json")
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool {
		return task.Status == model.TaskStatusDone && task.Landed && !task.ProcessAlive
	})
	waitForCLIRelease(t, fixture.database, first.Attempt.ID)
	after := inspectContinuation(t, fixture, owner, task.ID)
	if len(after.Attempts) != 1 || after.Attempts[0].ID != first.Attempt.ID || after.Attempts[0].RunGeneration != 3 || after.Task.ArtifactRef != "report:"+after.Attempts[0].ReportPath {
		t.Fatalf("continuation identity = %+v", after)
	}
	assertContinuationPreserved(t, before, after, prior, body)
	assertContinuationProof(t, fixture, owner, after)
	t.Logf("same attempt %s: run1=%s run2=%s run3=%s; original SHA-256=%x unchanged", first.Attempt.ID, prior, second.Attempt.ReportPath, after.Attempts[0].ReportPath, sha256.Sum256(body))
}

func inspectContinuation(t *testing.T, fixture *planSurfaceFixture, owner map[string]string, taskID string) model.TaskDetail {
	t.Helper()
	var detail model.TaskDetail
	if err := json.Unmarshal(fixture.run(t, owner, "task", "inspect", taskID, "--json"), &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

func assertContinuationPreserved(t *testing.T, before, after model.TaskDetail, path string, body []byte) {
	t.Helper()
	preserved, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(preserved) != sha256.Sum256(body) {
		t.Fatalf("prior report bytes changed: %v", err)
	}
	for _, prior := range before.Messages {
		found := false
		for _, current := range after.Messages {
			if current.ID == prior.ID {
				found = reflect.DeepEqual(prior, current)
			}
		}
		if !found {
			t.Fatalf("prior message changed or missing: %+v", prior)
		}
	}
}

func assertContinuationProof(t *testing.T, fixture *planSurfaceFixture, owner map[string]string, detail model.TaskDetail) {
	t.Helper()
	state, err := store.Open(fixture.database)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	attempt := detail.Attempts[len(detail.Attempts)-1]
	done, err := state.AcceptedDone(attempt.ID, attempt.RunGeneration)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := state.VerifiedArtifactForDoneMessage(done.ID)
	if err != nil || artifact == nil || artifact.OriginalRef != detail.Task.ArtifactRef {
		t.Fatalf("verified artifact=%+v error=%v", artifact, err)
	}
	proof, err := state.LandingProof(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(strings.TrimPrefix(artifact.OriginalRef, "report:"))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(body)) != artifact.SHA256 {
		t.Fatalf("artifact hash mismatch: %+v err=%v", artifact, err)
	}
	snapshot, err := os.ReadFile(artifact.SnapshotPath)
	if err != nil || string(snapshot) != string(body) {
		t.Fatalf("snapshot mismatch: %v", err)
	}
	if attempt.ReleaseState == "held" {
		_, diagnostic := fixture.runFailure(t, owner, "task", "verify-delivery", detail.Task.ID, "--json")
		if !strings.Contains(diagnostic, "residual workspace changes") {
			t.Fatalf("expected dirty workspace release refusal: %s", diagnostic)
		}
	} else {
		fixture.run(t, owner, "task", "verify-delivery", detail.Task.ID, "--json")
	}
	again, err := state.VerifiedArtifactForDoneMessage(done.ID)
	if err != nil || !reflect.DeepEqual(artifact, again) {
		t.Fatalf("immutable artifact changed: %+v err=%v", again, err)
	}
	proofAgain, err := state.LandingProof(attempt.ID)
	if err != nil || !reflect.DeepEqual(proof, proofAgain) {
		t.Fatalf("immutable proof changed: %+v err=%v", proofAgain, err)
	}
}

func TestCLIReportLegacyWaitingUpgradeE2E(t *testing.T) {
	legacy := os.Getenv("SHEPHRD_TEST_LEGACY_CLI")
	if legacy == "" {
		t.Skip("set SHEPHRD_TEST_LEGACY_CLI to exercise an old executable in disposable state")
	}
	fixture := newPlanSurfaceFixture(t, continuationPiScript())
	candidate := fixture.binary
	fixture.binary = legacy
	owner := map[string]string{"PI_SESSION_ID": "report-upgrade", "SHEPHRD_EXECUTABLE": legacy}
	fixture.run(t, owner, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	var task model.Task
	if err := json.Unmarshal(fixture.run(t, owner, "task", "create", "--repo", "demo", "--feature", "upgrade", "--deliverable", "report", "Preserve legacy report", "--json"), &task); err != nil {
		t.Fatal(err)
	}
	fixture.run(t, owner, "worker", "spawn", task.ID, "--json")
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool { return task.Status == model.TaskStatusBlocked && !task.ProcessAlive })
	prior := filepath.Join(fixture.dataDir, task.ID, "report.md")
	body, err := os.ReadFile(prior)
	if err != nil {
		t.Fatal(err)
	}
	owner["SHEPHRD_REPORT_MODE"] = "waiting"
	for run := 2; run <= 3; run++ {
		var result control.SpawnResult
		if err := json.Unmarshal(fixture.run(t, owner, "worker", "relaunch", task.ID, "--json"), &result); err != nil {
			t.Fatal(err)
		}
		waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool { return task.Status == model.TaskStatusWaiting && !task.ProcessAlive })
		brief, err := os.ReadFile(result.BriefPath)
		if err != nil || !strings.Contains(string(brief), "Write the investigation report to "+prior+" and") {
			t.Fatalf("legacy collision not reproduced: %s err=%v", brief, err)
		}
	}
	before := inspectContinuation(t, fixture, owner, task.ID)
	attempt := before.Attempts[0]
	draft := filepath.Join(attempt.WorktreePath, ".data", "continuation-evidence", "continuation-report-draft.md")
	if err := os.MkdirAll(filepath.Dir(draft), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(draft, []byte("unsubmitted draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.binary = candidate
	owner["SHEPHRD_EXECUTABLE"] = candidate
	owner["SHEPHRD_REPORT_MODE"] = "done"
	owner["SHEPHRD_REPORT_DRAFT"] = draft
	fixture.run(t, owner, "worker", "send", task.ID, "Review the unsubmitted draft and write the current run report; preserve earlier reports", "--json")
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool {
		return task.Status == model.TaskStatusDone && task.Landed && !task.ProcessAlive
	})
	state, err := store.Open(fixture.database)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool {
		current, err := state.Attempt(attempt.ID)
		log, logErr := os.ReadFile(filepath.Join(fixture.dataDir, task.ID, "attempt-1-runner.log"))
		return err == nil && current.ReleaseState == "held" && logErr == nil && strings.Contains(string(log), "residual workspace changes")
	})
	after := inspectContinuation(t, fixture, owner, task.ID)
	if len(after.Attempts) != 1 || after.Attempts[0].ID != attempt.ID || after.Attempts[0].RunGeneration != 4 || after.Attempts[0].ReleaseState != "held" {
		t.Fatalf("upgraded continuation lost identity or dirty workspace: %+v", after.Attempts)
	}
	assertContinuationPreserved(t, before, after, prior, body)
	preservedDraft, err := os.ReadFile(draft)
	if err != nil || string(preservedDraft) != "unsubmitted draft\n" {
		t.Fatalf("draft changed: %q err=%v", preservedDraft, err)
	}
	assertContinuationProof(t, fixture, owner, after)
	t.Logf("old executable collision reproduced in runs 2 and 3; candidate send advanced same attempt %s from waiting run3 to verified run4 at %s; old SHA-256=%x and draft preserved", attempt.ID, after.Attempts[0].ReportPath, sha256.Sum256(body))
}

func TestCLIReportStaleDestinationE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, continuationPiScript())
	owner := map[string]string{"PI_SESSION_ID": "report-stale"}
	fixture.run(t, owner, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	var task model.Task
	if err := json.Unmarshal(fixture.run(t, owner, "task", "create", "--repo", "demo", "--feature", "stale", "--deliverable", "report", "Reject prior report destination", "--json"), &task); err != nil {
		t.Fatal(err)
	}
	var first control.SpawnResult
	if err := json.Unmarshal(fixture.run(t, owner, "worker", "spawn", task.ID, "--json"), &first); err != nil {
		t.Fatal(err)
	}
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool { return task.Status == model.TaskStatusBlocked && !task.ProcessAlive })
	before := inspectContinuation(t, fixture, owner, task.ID)
	body, err := os.ReadFile(first.Attempt.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	owner["SHEPHRD_REPORT_MODE"] = "done"
	owner["SHEPHRD_REPORT_REF"] = first.Attempt.ReportPath
	fixture.run(t, owner, "worker", "relaunch", task.ID, "--json")
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool { return task.Status == model.TaskStatusBlocked && !task.ProcessAlive })
	after := inspectContinuation(t, fixture, owner, task.ID)
	if after.Task.ClaimedDone || after.Task.Landed || len(after.Attempts) != 1 || after.Attempts[0].RunGeneration != 2 {
		t.Fatalf("stale report accepted: %+v", after)
	}
	rejected := false
	for _, message := range after.Messages {
		if message.RunGeneration == 2 && message.Type == "protocol-repair" && strings.Contains(message.Payload, adapter.DiagnosticArtifactContract) {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("missing artifact rejection: %+v", after.Messages)
	}
	assertContinuationPreserved(t, before, after, first.Attempt.ReportPath, body)
	t.Log("run2 refused run1 artifact without replacing prior bytes, reference or message history")
}
