package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"shephrd/internal/control"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestSubdriverCoordinationSequencingE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, directSurfacePiScript())
	fixture.environment = compoundCLIEnvironment(fixture.environment, map[string]string{
		"SHEPHRD_DATA_DIR": fixture.dataDir, "SHEPHRD_WORKER_RUNTIME": "headless", "SHEPHRD_WORKER": "",
		"SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
		"SHEPHRD_COORDINATOR_ID": "", "SHEPHRD_COORDINATOR_GENERATION": "", "SHEPHRD_COORDINATOR_TOKEN": "",
	})
	main := map[string]string{"PI_SESSION_ID": "coordination-main"}
	fixture.run(t, main, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	request := func(key, original, context, lead string) model.SubdriverRequest {
		t.Helper()
		var r model.SubdriverRequest
		if err := json.Unmarshal(fixture.run(t, main, "subdriver", "handoff", original, "--repo", "demo", "--context", context, "--lead-request", lead, "--key", key, "--queue", "--json"), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := request("reverse", "Assign bounded local work", "No publication authority", "")
	state, err := store.Open(fixture.database)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	f, err := state.ReserveSubdriver(first.SubdriverID, 0, "pi", "fixture", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.StartSubdriver(f, os.Getpid(), "fixture-session"); err != nil {
		t.Fatal(err)
	}
	owner := map[string]string{"SHEPHRD_SUBDRIVER_ID": f.ID, "SHEPHRD_SUBDRIVER_GENERATION": strconv.Itoa(f.Generation), "SHEPHRD_SUBDRIVER_TOKEN": f.Token}
	ret := func(r model.SubdriverRequest, key, kind, text string) model.SubdriverEvent {
		t.Helper()
		var e model.SubdriverEvent
		if err := json.Unmarshal(fixture.run(t, owner, "subdriver", "return", r.ID, text, "--key", key, "--kind", kind, "--json"), &e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	assertState := func(r model.SubdriverRequest, want string) {
		t.Helper()
		var latest model.SubdriverRequest
		if err := json.Unmarshal(fixture.run(t, main, "subdriver", "request", r.ID, "--json"), &latest); err != nil {
			t.Fatal(err)
		}
		if latest.State != want {
			t.Errorf("request %s state = %s, want %s", r.Key, latest.State, want)
		}
	}
	dispatchArgs := func(r model.SubdriverRequest, key string) []string {
		return []string{"subdriver", "dispatch", r.ID, "Write a bounded local report", "--key", key, "--feature", r.Key + ":" + key, "--deliverable", "report", "--acceptance", "Local report only", "--json"}
	}
	dispatch := func(r model.SubdriverRequest, key string) model.Task {
		t.Helper()
		var task model.Task
		if err := json.Unmarshal(fixture.run(t, owner, dispatchArgs(r, key)...), &task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	t.Run("result-before-dispatch", func(t *testing.T) {
		ret(first, "complete", "result", "Assignment result published before dispatch")
		_, diagnostic := fixture.runFailure(t, owner, dispatchArgs(first, "new")...)
		if !strings.Contains(diagnostic, "request is done, not open") || fixture.taskCount(t) != 0 {
			t.Fatalf("reverse-order dispatch: %s", diagnostic)
		}
		assertState(first, "done")
		page, err := state.SubdriverPage(f.ID, 0)
		if err != nil || len(page.Workers) != 0 {
			t.Fatalf("rejected dispatch linked work: %+v %v", page, err)
		}
	})
	t.Run("late-returns", func(t *testing.T) {
		for _, kind := range []string{"result", "question", "blocker", "handoff"} {
			t.Run(kind, func(t *testing.T) {
				r := request("late-"+kind, "Finish this request", "Local only", "")
				ret(r, "complete", "result", "Complete")
				e := ret(r, "correction", kind, "Correction: no follow-up or hold was delivered")
				replay := ret(r, "correction", kind, e.Payload)
				if !reflect.DeepEqual(e, replay) {
					t.Fatalf("return replay changed event: %+v %+v", e, replay)
				}
				assertState(r, "done")
				_, diagnostic := fixture.runFailure(t, main, "subdriver", "reply", r.ID, "Continue", "--reply-to", fmt.Sprint(e.ID), "--key", "late-reply", "--json")
				if kind != "result" && !strings.Contains(diagnostic, "create a new correlated handoff") {
					t.Errorf("late reply diagnostic: %s", diagnostic)
				}
				notices, err := state.Notifications("")
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, n := range notices {
					if n.SubdriverEventID == e.ID {
						count++
						if n.RequestID != r.ID || n.TargetDriverID != r.DriverID || n.Kind != "subdriver-"+kind {
							t.Fatalf("late return lost correlation: %+v", n)
						}
					}
				}
				if count != 1 {
					t.Fatalf("late return notification count = %d", count)
				}
			})
		}
	})
	t.Run("correlated-open-continuation", func(t *testing.T) {
		original := "  Actual user continuation\nKeep the existing local-only task contract.\n"
		context := "Prior request " + first.ID + "; no undelivered hold or new publication permission"
		continued := request("continuation", original, context, first.ID)
		replay := request("continuation", original, context, first.ID)
		if continued.ID != replay.ID || continued.State != "open" || continued.LeadRequestID != first.ID || continued.Original != original || continued.Context != context {
			t.Fatalf("correlated intake changed contract: %+v %+v", continued, replay)
		}
		task := dispatch(continued, "work")
		ret(continued, "complete", "result", "Complete")
		if again := dispatch(continued, "work"); again.ID != task.ID {
			t.Fatal("completed request dispatch replay duplicated task")
		}
		ret(continued, "correction", "handoff", "Further work needs a new intake")
		assertState(continued, "done")
		_, diagnostic := fixture.runFailure(t, owner, dispatchArgs(continued, "new")...)
		if !strings.Contains(diagnostic, "request is done, not open") {
			t.Errorf("terminal dispatch fence: %s", diagnostic)
		}
		latest, err := state.Task(task.ID)
		attempts, attemptErr := state.Attempts(task.ID)
		if err != nil || attemptErr != nil || latest.Status != model.TaskStatusQueued || len(attempts) != 0 || latest.AcceptanceCriteria != "Local report only" {
			t.Fatalf("conversational completion changed worker: %+v %+v %v %v", latest, attempts, err, attemptErr)
		}
	})
	t.Run("ordinary-lead-reply", func(t *testing.T) {
		lead := request("lead", "Coordinate bounded work", "Local only", "")
		handoff := ret(lead, "proposal", "handoff", "Ask main for a scoped sibling result")
		assertState(lead, "waiting")
		sibling := request("sibling", "Answer the scoped question", "Local only", lead.ID)
		ret(sibling, "complete", "result", "Sibling result")
		args := []string{"subdriver", "reply", lead.ID, "Actual sibling result and user reply", "--reply-to", fmt.Sprint(handoff.ID), "--key", "relay", "--json"}
		var reply, replay model.SubdriverEvent
		if err := json.Unmarshal(fixture.run(t, main, args...), &reply); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(fixture.run(t, main, args...), &replay); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(reply, replay) || reply.ReplyTo != handoff.ID {
			t.Fatalf("lead reply replay: %+v %+v", reply, replay)
		}
		assertState(lead, "open")
		dispatch(lead, "after-relay")
	})
	t.Run("worker-done-is-not-request-result", func(t *testing.T) {
		r := request("worker", "Produce a local report", "No publication", "")
		task := dispatch(r, "report")
		fixture.run(t, owner, "task", "inspect", task.ID, "--json")
		fixture.run(t, owner, "worker", "spawn", task.ID, "--json")
		waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool { return task.Status == model.TaskStatusDone && !task.ProcessAlive })
		assertState(r, "open")
		ret(r, "complete", "result", "Local report complete")
		assertState(r, "done")
		var detail model.TaskDetail
		if err := json.Unmarshal(fixture.run(t, owner, "task", "inspect", task.ID, "--json"), &detail); err != nil {
			t.Fatal(err)
		}
		waitForCLIRelease(t, fixture.database, detail.Task.CurrentAttemptID)
		before := inspectContinuation(t, fixture, main, task.ID)
		fixture.runFailure(t, owner, "worker", "send", task.ID, "Do not revive the terminal attempt", "--json")
		after := inspectContinuation(t, fixture, main, task.ID)
		if !reflect.DeepEqual(before.Task, after.Task) || !reflect.DeepEqual(before.Attempts, after.Attempts) || !reflect.DeepEqual(before.Messages, after.Messages) {
			t.Fatal("terminal send changed completed worker")
		}
	})
}

func TestCLIWorkerBusySendIsNotDeliveredE2E(t *testing.T) {
	root := t.TempDir()
	script := strings.Replace(directSurfacePiScript(), "set -eu\n", "set -eu\nprintf ready > \"$SHEPHRD_TEST_BUSY_ROOT/ready\"\nremaining=750\nwhile [ ! -f \"$SHEPHRD_TEST_BUSY_ROOT/release\" ]; do\n  [ \"$remaining\" -gt 0 ] || exit 97\n  remaining=$((remaining - 1))\n  sleep .02\ndone\n", 1)
	fixture := newPlanSurfaceFixture(t, script)
	fixture.environment = compoundCLIEnvironment(fixture.environment, map[string]string{
		"SHEPHRD_DATA_DIR": fixture.dataDir, "SHEPHRD_WORKER_RUNTIME": "headless", "SHEPHRD_WORKER": "",
		"SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
		"SHEPHRD_COORDINATOR_ID": "", "SHEPHRD_COORDINATOR_GENERATION": "", "SHEPHRD_COORDINATOR_TOKEN": "",
		"SHEPHRD_TEST_BUSY_ROOT": root,
	})
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(root, "release"), nil, 0600) })
	owner := map[string]string{"PI_SESSION_ID": "busy-main"}
	fixture.run(t, owner, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	var task model.Task
	if err := json.Unmarshal(fixture.run(t, owner, "task", "create", "--repo", "demo", "--feature", "busy", "--deliverable", "report", "Write a local report", "--json"), &task); err != nil {
		t.Fatal(err)
	}
	var spawned control.SpawnResult
	if err := json.Unmarshal(fixture.run(t, owner, "worker", "spawn", task.ID, "--json"), &spawned); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake Pi did not reach controlled busy state")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := inspectContinuation(t, fixture, owner, task.ID)
	stdout, stderr, err := fixture.output(t, owner, "worker", "send", task.ID, "Hold all remote writes", "--json")
	if err == nil || len(stdout) != 0 {
		t.Fatalf("busy send returned a success receipt: %s %s %v", stdout, stderr, err)
	}
	for _, want := range []string{"worker is busy", "not queued or delivered", "no hold"} {
		if !strings.Contains(string(stderr), want) {
			t.Errorf("busy rejection missing %q: %s", want, stderr)
		}
	}
	after := inspectContinuation(t, fixture, owner, task.ID)
	if !reflect.DeepEqual(before.Task, after.Task) || !reflect.DeepEqual(before.Attempts, after.Attempts) || !reflect.DeepEqual(before.Messages, after.Messages) {
		t.Fatalf("busy rejection changed task, run or messages: before=%+v after=%+v", before, after)
	}
	files, err := filepath.Glob(filepath.Join(fixture.dataDir, task.ID, "follow-up-*.md"))
	if err != nil || len(files) != 0 {
		t.Fatalf("busy rejection persisted follow-up files: %v %v", files, err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool { return task.Status == model.TaskStatusDone && !task.ProcessAlive })
}
