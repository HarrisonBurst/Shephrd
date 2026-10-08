package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestSubdriverCompatibilityCLIEndToEnd(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "shephrd")
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", output, err)
	}
	db := filepath.Join(root, "state.db")
	cfg := filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, "database_path = %q\ndata_dir = %q\n", db, filepath.Join(root, "data")), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(model.Repo{Name: "fixture", Path: root, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.HandoffSubdriver(repo.ID, "", "driver:main", "request", "Original intake", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.ReserveSubdriver(r.SubdriverID, 0, "pi", "fixture", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartSubdriver(f, os.Getpid(), "fixture"); err != nil {
		t.Fatal(err)
	}
	other, err := s.HandoffSubdriver("", "Other scope", "driver:main", "other", "Other intake", "", "")
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.DispatchSubdriverWorker(f, r.ID, "child", model.Task{FeatureKey: "child", Objective: "Scoped report", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := s.BeginAttempt(child.ID, "pi", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, s, attempt.ID, "worker-session", filepath.Join(root, "workspace"), "lease", "shephrd/fixture"); err != nil {
		t.Fatal(err)
	}
	run, err := s.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSystemCheckpoint(attempt.ID, run, "fixture", []string{"report"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "checkpoint", NextSteps: []string{"report"}}
	if _, err := s.AddEventForRun(attempt.ID, run, model.Event{Type: "checkpoint", Payload: "checkpoint", Checkpoint: &checkpoint}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEventForRun(attempt.ID, run, model.Event{Type: "blocked", Payload: "Fixture blocked"}, 3, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	drain, err := s.DrainNotifications("", child.DriverID, "coordinator:1", 1)
	if err != nil || len(drain.Notifications) != 1 {
		t.Fatalf("drain: %+v %v", drain, err)
	}
	n := drain.Notifications[0]
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "claude"), "#!/bin/sh\nexit 0\n")
	environment := compoundCLIEnvironment(os.Environ(), map[string]string{
		"PATH":           bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SHEPHRD_CONFIG": cfg, "SHEPHRD_WORKER": "", "PI_SESSION_ID": "",
		"SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
		"SHEPHRD_COORDINATOR_ID": "", "SHEPHRD_COORDINATOR_GENERATION": "", "SHEPHRD_COORDINATOR_TOKEN": "",
	})
	call := func(overrides map[string]string, ok bool, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, append(args, "--json")...)
		cmd.Env = compoundCLIEnvironment(environment, overrides)
		output, err := cmd.CombinedOutput()
		if (err == nil) != ok {
			t.Fatalf("%v: %v %s", args, err, output)
		}
		return output
	}
	for _, command := range []string{"subdriver", "coordinator"} {
		help := call(nil, true, command, "inspect", "--help")
		if !strings.Contains(string(help), "shephrd subdriver inspect") || strings.Contains(string(help), "<coordinator-id>") {
			t.Fatalf("noncanonical help: %s", help)
		}
		var page map[string]json.RawMessage
		if err := json.Unmarshal(call(nil, true, command, "inspect", f.ID), &page); err != nil {
			t.Fatal(err)
		}
		if string(page["subdriver"]) != string(page["coordinator"]) || len(page["subdriver"]) == 0 {
			t.Fatalf("compatibility page: %s", page)
		}
		for _, prefix := range []string{"SHEPHRD_SUBDRIVER_", "SHEPHRD_COORDINATOR_"} {
			env := map[string]string{prefix + "ID": f.ID, prefix + "GENERATION": strconv.Itoa(f.Generation), prefix + "TOKEN": f.Token}
			call(env, true, command, "request", r.ID)
			call(env, false, command, "request", other.ID)
			call(env, false, command, "inspect", other.SubdriverID)
			call(env, false, command, "handoff", "recursive", "--general-context", "not allowed", "--key", "recursive", "--queue")
			call(env, false, "wake", "drain")
			call(env, false, "wake", "pump", "--driver-id", "driver:main")
			if output := call(env, false, "workspace", "release", child.ID, "--discard"); !strings.Contains(string(output), "discard authorization remains with main") {
				t.Fatalf("discard scope fence: %s", output)
			}
			call(env, true, command, "return", r.ID, "Choose a format", "--kind", "question", "--key", "same-question")
			call(env, true, "wake", "renew", "--claim-token", n.ClaimToken)
			call(env, false, "wake", "ack", "--claim-token", n.ClaimToken, "--driver-id", "driver:other")
			call(env, false, "wake", "ack", "--claim-token", n.ClaimToken, "--driver-generation", "coordinator:2")
			env[prefix+"GENERATION"] = "2"
			call(env, false, "wake", "pump", "--driver-id", "driver:main")
			call(env, false, "wake", "ack", "--claim-token", n.ClaimToken)
			call(env, false, command, "return", r.ID, "stale", "--kind", "result", "--key", "stale")
			env[prefix+"GENERATION"] = "1"
			env["SHEPHRD_WORKER"] = "1"
			call(env, false, command, "request", r.ID)
			call(env, false, "wake", "ack", "--claim-token", n.ClaimToken)
		}
		call(map[string]string{"SHEPHRD_WORKER": "1"}, false, command, "request", r.ID)
		call(map[string]string{"SHEPHRD_WORKER": "1"}, false, "wake", "pump", "--driver-id", "driver:main")
	}
	both := map[string]string{}
	for _, prefix := range []string{"SHEPHRD_SUBDRIVER_", "SHEPHRD_COORDINATOR_"} {
		both[prefix+"ID"], both[prefix+"GENERATION"], both[prefix+"TOKEN"] = f.ID, "1", f.Token
	}
	call(both, true, "coordinator", "request", r.ID)
	for _, command := range []string{"subdriver", "coordinator"} {
		for _, args := range [][]string{{f.ID, "--generation", "2", "--token", f.Token}, {other.SubdriverID, "--generation", "1", "--token", f.Token}, {f.ID, "--generation", "1", "--token", "wrong"}} {
			output := call(both, false, append([]string{command, "_run"}, args...)...)
			if !strings.Contains(string(output), "runner arguments conflict") {
				t.Fatalf("runner identity conflict: %s", output)
			}
		}
	}
	for _, field := range []string{"ID", "GENERATION", "TOKEN"} {
		key := "SHEPHRD_COORDINATOR_" + field
		original := both[key]
		for _, invalid := range []string{"conflicting", ""} {
			both[key] = invalid
			call(both, false, "subdriver", "request", r.ID)
			call(both, false, "coordinator", "_run", f.ID, "--generation", "1", "--token", f.Token)
			call(both, false, "wake", "ack", "--claim-token", n.ClaimToken)
			call(both, false, "wake", "pump", "--driver-id", "driver:main")
		}
		both[key] = original
	}
	for _, prefix := range []string{"SHEPHRD_SUBDRIVER_", "SHEPHRD_COORDINATOR_"} {
		call(map[string]string{prefix + "TOKEN": f.Token}, false, "task", "inspect", child.ID)
	}
	for _, owner := range []string{child.DriverID, "subdriver:" + f.ID} {
		call(nil, false, "wake", "drain", "--driver-id", owner)
		call(nil, false, "wake", "pump", "--driver-id", owner)
		call(nil, false, "subdriver", "handoff", "recursive", "--general-context", "none", "--driver-id", owner, "--key", "recursive", "--queue")
	}
	call(both, true, "wake", "ack", "--claim-token", n.ClaimToken, "--handling-id", "one")
	var ack model.NotificationAckReceipt
	if err := json.Unmarshal(call(both, true, "wake", "ack", "--claim-token", n.ClaimToken, "--handling-id", "one"), &ack); err != nil || !ack.Idempotent {
		t.Fatalf("ack replay: %+v %v", ack, err)
	}
	var returned model.NotificationDrain
	if err := json.Unmarshal(call(nil, true, "wake", "drain", "--driver-id", "driver:main", "--driver-generation", "main:one"), &returned); err != nil || len(returned.Notifications) != 1 {
		t.Fatalf("return replay: %+v %v", returned, err)
	}
	q := returned.Notifications[0]
	if q.Kind != "subdriver-question" || q.SubdriverID != f.ID || q.SubdriverEventID == 0 {
		t.Fatalf("noncanonical return: %+v", q)
	}
	for _, legacy := range []bool{false, true} {
		args := []string{"subdriver", "notification", q.NotificationID}
		if legacy {
			args = append(args, "--legacy-coordinator-json")
		}
		var notice model.DriverNotification
		if err := json.Unmarshal(call(nil, true, args...), &notice); err != nil {
			t.Fatal(err)
		}
		accepted := false
		switch notice.Kind {
		case "coordinator-question", "coordinator-result", "coordinator-blocker", "coordinator-handoff":
			accepted = true
		}
		if accepted != legacy || notice.NotificationID != q.NotificationID || notice.ClaimToken != q.ClaimToken || notice.DriverGeneration != q.DriverGeneration || notice.ClaimOwner != q.ClaimOwner || notice.State != model.NotificationClaimed || notice.AckedAt != nil {
			t.Fatalf("strict-kind consumer transition changed claim or acceptance: legacy=%t %+v", legacy, notice)
		}
	}
	legacy := call(nil, true, "wake", "renew", "--claim-token", q.ClaimToken, "--driver-id", "driver:main", "--legacy-coordinator-json")
	if !strings.Contains(string(legacy), `"kind":"coordinator-question"`) || !strings.Contains(string(legacy), `"coordinator_event_id":`) || !strings.Contains(string(legacy), `"subdriver_event_id":`) {
		t.Fatalf("legacy consumer output: %s", legacy)
	}
	call(both, false, "wake", "ack", "--claim-token", q.ClaimToken)
	call(nil, true, "wake", "ack", "--claim-token", q.ClaimToken, "--driver-id", "driver:main")
	stored, err := s.Task(child.ID)
	if err != nil || stored.DriverID != child.DriverID || stored.Landed || stored.DiscardAuthorized {
		t.Fatalf("rename changed lifecycle or ownership: %+v %v", stored, err)
	}
}
