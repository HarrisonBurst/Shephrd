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

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestCLIHeadlessCompoundRecoveryE2E(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("compound recovery\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "add", "README.md")
	command.Dir = repoRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s: %v", output, err)
	}
	command = exec.Command("git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	command.Dir = repoRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s: %v", output, err)
	}
	harnessScript := `#!/usr/bin/env python3
import os, re, sys, time
prompt = ' '.join(sys.argv[1:])
path = re.search(r'Write the investigation report to (.+?) and use artifact', prompt).group(1)
legacy = os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(path))), 'report.md')
def emit(source):
    if os.path.exists(legacy) and not os.path.exists(path):
        with open(path, 'xb') as target: target.write(open(legacy, 'rb').read())
    sys.stdout.write(open(source).read().replace(legacy, path))
    sys.stdout.flush()
if os.environ.get('SHEPHRD_COMPOUND_PREFIX_STREAM'):
    emit(os.environ['SHEPHRD_COMPOUND_PREFIX_STREAM'])
    open(os.environ['SHEPHRD_COMPOUND_PAUSED'], 'w').close()
    while not os.path.exists(os.environ['SHEPHRD_COMPOUND_RESUME']): time.sleep(.01)
    emit(os.environ['SHEPHRD_COMPOUND_SUFFIX_STREAM'])
elif open(os.environ['SHEPHRD_COMPOUND_MODE']).read() == 'compound':
    emit(os.environ['SHEPHRD_COMPOUND_STREAM'])
else:
    emit(os.environ['SHEPHRD_PLAIN_STREAM'])
`
	for _, harness := range []string{"pi", "codex"} {
		if err := os.WriteFile(filepath.Join(binDir, harness), []byte(harnessScript), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configBody := fmt.Sprintf(`default_harness = "pi"
worker_runtime = "headless"
database_path = %q
data_dir = %q
worktree_root = %q

[wake]
enabled = true
default_batch = 10
max_batch = 20
claim_ttl = "5m"
claim_ttl_min = "30s"
claim_ttl_max = "30m"
driver_id = "driver:compound-e2e"

[notifications]
enabled = false
details = false
task_per_minute = 2
global_per_minute = 10

[pi_watcher]
enabled = false
poll_min = "1s"
poll_max = "15s"
`, databasePath, dataDir, filepath.Join(root, "worktrees"))
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := compoundCLIEnvironment(os.Environ(), map[string]string{
		"PATH":               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PI_SESSION_ID":      "",
		"SHEPHRD_CONFIG":     configPath,
		"SHEPHRD_EXECUTABLE": binary,
	})
	run := func(extra map[string]string, args ...string) []byte {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = compoundCLIEnvironment(environment, extra)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("shephrd %v: %s: %v", args, stderr.String(), err)
		}
		return stdout.Bytes()
	}
	run(nil, "repo", "add", repoRoot, "--name", "compound", "--json")
	for _, test := range []struct {
		harness, recovery string
	}{{"pi", "relaunch"}, {"codex", "retry"}} {
		t.Run(test.harness+"/"+test.recovery, func(t *testing.T) {
			var task model.Task
			if err := json.Unmarshal(run(nil, "task", "create", "--title", "Test task", "--repo", "compound", "--feature", test.harness+"-"+test.recovery, "--deliverable", "report", "--driver-id", "driver:compound-e2e", "compound recovery", "--json"), &task); err != nil {
				t.Fatal(err)
			}
			modePath := filepath.Join(root, task.ID+"-mode")
			plainPath := filepath.Join(root, task.ID+"-plain.jsonl")
			compoundPath := filepath.Join(root, task.ID+"-compound.jsonl")
			reportPath := filepath.Join(dataDir, task.ID, "report.md")
			reportBody := bytes.Repeat([]byte("R"), 46818)
			if err := os.WriteFile(modePath, []byte("plain"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plainPath, compoundCLINativeStream(t, test.harness, "ordinary assistant text"), 0o600); err != nil {
				t.Fatal(err)
			}
			checkpoint := compoundCLIEnvelope(t, model.Event{Type: "checkpoint", Payload: "recovery checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "recovery checkpoint", Completed: []string{"report ready"}, NextSteps: []string{"emit done"}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
			done := compoundCLIEnvelope(t, model.Event{Type: "done", Payload: "recovery complete", Artifact: "report:" + reportPath})
			if err := os.WriteFile(compoundPath, compoundCLINativeStream(t, test.harness, checkpoint+"\n"+done), 0o600); err != nil {
				t.Fatal(err)
			}
			extra := map[string]string{"SHEPHRD_COMPOUND_MODE": modePath, "SHEPHRD_PLAIN_STREAM": plainPath, "SHEPHRD_COMPOUND_STREAM": compoundPath}
			run(extra, "worker", "spawn", task.ID, "--harness", test.harness, "--runtime", "headless", "--json")
			waitForCLITask(t, databasePath, task.ID, func(task model.Task) bool { return task.Status == model.TaskStatusBlocked && !task.ProcessAlive })
			if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(reportPath, reportBody, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(modePath, []byte("compound"), 0o600); err != nil {
				t.Fatal(err)
			}
			run(extra, "worker", test.recovery, task.ID, "--json")
			waitForCLITask(t, databasePath, task.ID, func(task model.Task) bool {
				return task.Status == model.TaskStatusDone && task.Landed && !task.ProcessAlive
			})
			var detail model.TaskDetail
			if err := json.Unmarshal(run(nil, "task", "inspect", task.ID, "--json"), &detail); err != nil {
				t.Fatal(err)
			}
			if test.recovery == "relaunch" {
				if len(detail.Attempts) != 1 || detail.Attempts[0].RunGeneration != 2 {
					t.Fatalf("relaunch attempts=%+v", detail.Attempts)
				}
			} else if len(detail.Attempts) != 2 || detail.Attempts[0].Status != model.AttemptStatusSuperseded || detail.Attempts[1].Number != 2 {
				t.Fatalf("retry attempts=%+v", detail.Attempts)
			}
			attempt := detail.Attempts[len(detail.Attempts)-1]
			checkpointCursor, doneCursor, doneCount := int64(0), int64(0), 0
			for _, message := range detail.Messages {
				if message.AttemptID != attempt.ID || message.RunGeneration != attempt.RunGeneration || message.Stale {
					continue
				}
				switch message.Type {
				case "checkpoint":
					if message.Direction == "worker-to-driver" {
						checkpointCursor = message.SourceCursor
					}
				case "done":
					doneCursor = message.SourceCursor
					doneCount++
				}
			}
			if checkpointCursor < 1 || doneCursor != checkpointCursor+1 || attempt.Cursor <= doneCursor || doneCount != 1 {
				t.Fatalf("checkpoint=%d done=%d attempt=%+v messages=%+v", checkpointCursor, doneCursor, attempt, detail.Messages)
			}
			doneNotifications := 0
			for _, notification := range detail.Notifications {
				if notification.Kind == "done" && notification.AttemptID == attempt.ID && notification.WorkerRunGeneration == attempt.RunGeneration {
					doneNotifications++
				}
			}
			if doneNotifications != 1 {
				t.Fatalf("notifications=%+v", detail.Notifications)
			}
			state, err := store.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			doneMessage, err := state.AcceptedDoneMessage(task.ID, attempt.ID)
			if err != nil {
				state.Close()
				t.Fatal(err)
			}
			artifact, err := state.VerifiedArtifactForDoneMessage(doneMessage.ID)
			state.Close()
			if err != nil || artifact == nil {
				t.Fatalf("artifact=%+v err=%v", artifact, err)
			}
			snapshot, err := os.ReadFile(artifact.SnapshotPath)
			if err != nil || !bytes.Equal(snapshot, reportBody) {
				t.Fatalf("snapshot bytes=%d err=%v", len(snapshot), err)
			}
		})
	}
	for _, harness := range []string{"pi", "codex"} {
		t.Run(harness+"/question-then-done", func(t *testing.T) {
			var task model.Task
			if err := json.Unmarshal(run(nil, "task", "create", "--title", "Test task", "--repo", "compound", "--feature", harness+"-question-done", "--deliverable", "report", "--driver-id", "driver:compound-e2e", "question then done", "--json"), &task); err != nil {
				t.Fatal(err)
			}
			prefixPath := filepath.Join(root, task.ID+"-question-prefix.jsonl")
			suffixPath := filepath.Join(root, task.ID+"-done-suffix.jsonl")
			pausedPath := filepath.Join(root, task.ID+"-paused")
			resumePath := filepath.Join(root, task.ID+"-resume")
			t.Cleanup(func() { _ = os.WriteFile(resumePath, []byte("resume"), 0o600) })
			reportPath := filepath.Join(dataDir, task.ID, "report.md")
			reportBody := bytes.Repeat([]byte("R"), 46818)
			if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(reportPath, reportBody, 0o600); err != nil {
				t.Fatal(err)
			}
			checkpoint := compoundCLIEnvelope(t, model.Event{Type: "checkpoint", Payload: "question checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "question checkpoint", Completed: []string{"question ready"}, NextSteps: []string{"ask question"}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
			question := compoundCLIEnvelope(t, model.Event{Type: "question", Payload: "review needed"})
			freshCheckpoint := compoundCLIEnvelope(t, model.Event{Type: "checkpoint", Payload: "done checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "done checkpoint", Completed: []string{"question answered"}, NextSteps: []string{"emit done"}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
			done := compoundCLIEnvelope(t, model.Event{Type: "done", Payload: "question flow complete", Artifact: "report:" + reportPath})
			if err := os.WriteFile(prefixPath, compoundCLINativeChunk(t, harness, []string{checkpoint, question}, true, false), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(suffixPath, compoundCLINativeChunk(t, harness, []string{freshCheckpoint, done}, false, true), 0o600); err != nil {
				t.Fatal(err)
			}
			extra := map[string]string{"SHEPHRD_COMPOUND_PREFIX_STREAM": prefixPath, "SHEPHRD_COMPOUND_SUFFIX_STREAM": suffixPath, "SHEPHRD_COMPOUND_PAUSED": pausedPath, "SHEPHRD_COMPOUND_RESUME": resumePath}
			run(extra, "worker", "spawn", task.ID, "--harness", harness, "--runtime", "headless", "--json")
			waitForCLITask(t, databasePath, task.ID, func(task model.Task) bool {
				return task.Status == model.TaskStatusWaiting && task.ProcessAlive
			})
			waitForCLIPath(t, pausedPath)
			var paused model.TaskDetail
			if err := json.Unmarshal(run(nil, "task", "inspect", task.ID, "--json"), &paused); err != nil {
				t.Fatal(err)
			}
			attempt := paused.Attempts[len(paused.Attempts)-1]
			const driverID = "driver:compound-e2e"
			driverGeneration := "generation:" + harness + "-question-done"
			var drain model.NotificationDrain
			if err := json.Unmarshal(run(nil, "wake", "drain", task.ID, "--driver-id", driverID, "--driver-generation", driverGeneration, "--limit", "10", "--json"), &drain); err != nil {
				t.Fatal(err)
			}
			if len(drain.Notifications) != 1 || drain.Notifications[0].Kind != "question" || drain.Notifications[0].SourceCursor != 3 || drain.Notifications[0].State != model.NotificationClaimed || drain.Notifications[0].DeliveryAttempts != 1 || drain.Notifications[0].ClaimToken == "" {
				t.Fatalf("question drain=%+v", drain)
			}
			questionNotification := drain.Notifications[0]
			var receipt model.NotificationAckReceipt
			if err := json.Unmarshal(run(nil, "wake", "ack", "--claim-token", questionNotification.ClaimToken, "--json"), &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.SchemaVersion != model.NotificationAckSchemaVersion || receipt.NotificationID != questionNotification.NotificationID || receipt.MessageID != questionNotification.MessageID || receipt.HandlingID == "" || receipt.ConsumerID != driverID || receipt.DriverGeneration != driverGeneration || receipt.Idempotent {
				t.Fatalf("question receipt=%+v", receipt)
			}
			handlingID := receipt.HandlingID
			if err := os.WriteFile(resumePath, []byte("resume"), 0o600); err != nil {
				t.Fatal(err)
			}
			attempt = waitForCLIRelease(t, databasePath, attempt.ID)
			var detail model.TaskDetail
			if err := json.Unmarshal(run(nil, "task", "inspect", task.ID, "--json"), &detail); err != nil {
				t.Fatal(err)
			}
			var eventTypes []string
			var cursors []int64
			for _, message := range detail.Messages {
				if message.AttemptID == attempt.ID && message.RunGeneration == attempt.RunGeneration && message.Direction == "worker-to-driver" && !message.Stale {
					eventTypes = append(eventTypes, message.Type)
					cursors = append(cursors, message.SourceCursor)
				}
			}
			if detail.Task.Status != model.TaskStatusDone || !detail.Task.Landed || detail.Task.ProcessAlive || detail.Task.ArtifactRef != "report:"+attempt.ReportPath || attempt.Status != model.AttemptStatusDone || attempt.Cursor != 6 || attempt.ReleaseState != "released" || attempt.ReleasedAt == nil || strings.Join(eventTypes, ",") != "progress,checkpoint,question,checkpoint,done" || fmt.Sprint(cursors) != "[1 2 3 4 5]" {
				t.Fatalf("task=%+v attempt=%+v event_types=%v cursors=%v messages=%+v", detail.Task, attempt, eventTypes, cursors, detail.Messages)
			}
			if len(detail.Notifications) != 2 || detail.Notifications[0].NotificationID != questionNotification.NotificationID || detail.Notifications[0].State != model.NotificationAcknowledged || detail.Notifications[0].DeliveryAttempts != 1 || detail.Notifications[0].HandlingID != handlingID || detail.Notifications[0].AckedAt == nil || detail.Notifications[1].Kind != "done" || detail.Notifications[1].SourceCursor != 5 || detail.Notifications[1].State != model.NotificationPending || detail.Notifications[1].DeliveryAttempts != 0 || detail.Notifications[1].SupersededAt != nil || detail.Notifications[1].AckedAt != nil {
				t.Fatalf("notifications=%+v", detail.Notifications)
			}
			state, err := store.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			questionLogs, err := state.NotificationDeliveryLogs(detail.Notifications[0].NotificationID, 10)
			if err != nil {
				state.Close()
				t.Fatal(err)
			}
			doneLogs, err := state.NotificationDeliveryLogs(detail.Notifications[1].NotificationID, 10)
			if err != nil {
				state.Close()
				t.Fatal(err)
			}
			doneMessage, err := state.AcceptedDoneMessage(task.ID, attempt.ID)
			if err != nil {
				state.Close()
				t.Fatal(err)
			}
			artifact, err := state.VerifiedArtifactForDoneMessage(doneMessage.ID)
			state.Close()
			if err != nil || artifact == nil {
				t.Fatalf("artifact=%+v err=%v", artifact, err)
			}
			if len(questionLogs) != 2 || questionLogs[0].Operation != "claim" || questionLogs[1].Operation != "ack" || len(doneLogs) != 0 {
				t.Fatalf("question_logs=%+v done_logs=%+v", questionLogs, doneLogs)
			}
			if artifact.ProducerTaskID != task.ID || artifact.ProducerAttemptID != attempt.ID || artifact.DoneMessageID != doneMessage.ID || artifact.OriginalRef != "report:"+attempt.ReportPath || artifact.SHA256 != "996579b5c432a6124af06c43814ab3e698642faa0ed4a4070e503cbd44d8706d" || artifact.SizeBytes != int64(len(reportBody)) {
				t.Fatalf("artifact=%+v done=%+v", artifact, doneMessage)
			}
			snapshot, err := os.ReadFile(artifact.SnapshotPath)
			if err != nil || !bytes.Equal(snapshot, reportBody) {
				t.Fatalf("snapshot bytes=%d err=%v", len(snapshot), err)
			}
			var doneDrain model.NotificationDrain
			if err := json.Unmarshal(run(nil, "wake", "drain", task.ID, "--driver-id", driverID, "--driver-generation", driverGeneration, "--limit", "10", "--json"), &doneDrain); err != nil {
				t.Fatal(err)
			}
			if len(doneDrain.Notifications) != 1 || doneDrain.Notifications[0].NotificationID != detail.Notifications[1].NotificationID || doneDrain.Notifications[0].Kind != "done" || doneDrain.Notifications[0].State != model.NotificationClaimed || doneDrain.Notifications[0].DeliveryAttempts != 1 || doneDrain.Notifications[0].ClaimToken == "" {
				t.Fatalf("post-release done drain=%+v", doneDrain)
			}
			doneNotification := doneDrain.Notifications[0]
			var doneReceipt model.NotificationAckReceipt
			if err := json.Unmarshal(run(nil, "wake", "ack", "--claim-token", doneNotification.ClaimToken, "--json"), &doneReceipt); err != nil {
				t.Fatal(err)
			}
			if doneReceipt.SchemaVersion != model.NotificationAckSchemaVersion || doneReceipt.NotificationID != doneNotification.NotificationID || doneReceipt.MessageID != doneNotification.MessageID || doneReceipt.HandlingID == "" || doneReceipt.Idempotent {
				t.Fatalf("done receipt=%+v", doneReceipt)
			}
			doneHandlingID := doneReceipt.HandlingID
			var repeatedDoneReceipt model.NotificationAckReceipt
			if err := json.Unmarshal(run(nil, "wake", "ack", "--claim-token", doneNotification.ClaimToken, "--json"), &repeatedDoneReceipt); err != nil {
				t.Fatal(err)
			}
			if !repeatedDoneReceipt.Idempotent || repeatedDoneReceipt.HandlingID != doneHandlingID {
				t.Fatalf("repeated done receipt=%+v", repeatedDoneReceipt)
			}
			var finalDrain model.NotificationDrain
			if err := json.Unmarshal(run(nil, "wake", "drain", task.ID, "--driver-id", driverID, "--driver-generation", driverGeneration, "--limit", "10", "--json"), &finalDrain); err != nil {
				t.Fatal(err)
			}
			if len(finalDrain.Notifications) != 0 {
				t.Fatalf("final drain=%+v", finalDrain)
			}
			if err := json.Unmarshal(run(nil, "task", "inspect", task.ID, "--json"), &detail); err != nil {
				t.Fatal(err)
			}
			if detail.Notifications[1].State != model.NotificationAcknowledged || detail.Notifications[1].DeliveryAttempts != 1 || detail.Notifications[1].HandlingID != doneHandlingID || detail.Notifications[1].AckedAt == nil || detail.Notifications[1].SupersededAt != nil {
				t.Fatalf("acknowledged done notification=%+v", detail.Notifications[1])
			}
			state, err = store.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			doneLogs, err = state.NotificationDeliveryLogs(doneNotification.NotificationID, 10)
			state.Close()
			if err != nil || len(doneLogs) != 2 || doneLogs[0].Operation != "claim" || doneLogs[1].Operation != "ack" {
				t.Fatalf("post-release done logs=%+v err=%v", doneLogs, err)
			}
		})
	}
}

func compoundCLIEnvironment(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	for _, value := range environment {
		key, _, _ := strings.Cut(value, "=")
		if _, replaced := replacements[key]; !replaced {
			result = append(result, value)
		}
	}
	for key, value := range replacements {
		result = append(result, key+"="+value)
	}
	return result
}

func waitForCLIPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("path %s did not appear", path)
}

func waitForCLIRelease(t *testing.T, databasePath, attemptID string) model.Attempt {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state, err := store.Open(databasePath)
		if err == nil {
			attempt, attemptErr := state.Attempt(attemptID)
			state.Close()
			if attemptErr == nil && attempt.ReleasedAt != nil {
				return attempt
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("attempt %s was not released", attemptID)
	return model.Attempt{}
}

func compoundCLIEnvelope(t *testing.T, event model.Event) string {
	t.Helper()
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return "<shephrd-event>" + string(encoded) + "</shephrd-event>"
}

func compoundCLINativeStream(t *testing.T, harness string, texts ...string) []byte {
	t.Helper()
	return compoundCLINativeChunk(t, harness, texts, true, true)
}

func compoundCLINativeChunk(t *testing.T, harness string, texts []string, acknowledge, terminal bool) []byte {
	t.Helper()
	var records []any
	switch harness {
	case "pi":
		if acknowledge {
			records = append(records, map[string]any{"type": "session", "id": "compound-pi-session"})
		}
		for _, text := range texts {
			records = append(records, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}, "stopReason": "stop"}})
		}
		if terminal {
			records = append(records, map[string]any{"type": "agent_end"})
		}
	case "codex":
		if acknowledge {
			records = append(records, map[string]any{"type": "thread.started", "thread_id": "compound-codex-session"})
		}
		for _, text := range texts {
			records = append(records, map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": text}})
		}
		if terminal {
			records = append(records, map[string]any{"type": "turn.completed"})
		}
	default:
		t.Fatalf("unknown harness %q", harness)
	}
	var stream bytes.Buffer
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		stream.Write(encoded)
		stream.WriteByte('\n')
	}
	return stream.Bytes()
}
