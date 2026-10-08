package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
)

const eventIngestionBranch = "shephrd/task-event-ingestion"

func eventIngestionFixture(t *testing.T, databasePath string) (*Store, model.Task, model.Attempt, int) {
	t.Helper()
	state, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "event-ingestion", Objective: "ingest events", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tree", "lease", eventIngestionBranch); err != nil {
		t.Fatal(err)
	}
	return state, task, attempt, prepareAttempt(t, state, attempt)
}

func TestAddEventForRunCommitsMessageTransitionAndNotificationTogether(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
	recordWorkerCheckpoint(t, state, attempt, generation, 1, []string{"answer"})

	message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "Which option?"}, 2, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	storedTask, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedAttempt, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if message.Stale || !message.Wake || storedTask.Status != model.TaskStatusWaiting || storedAttempt.Status != model.AttemptStatusWaiting || storedAttempt.Cursor != 2 {
		t.Fatalf("message=%+v task=%+v attempt=%+v", message, storedTask, storedAttempt)
	}
	if len(notifications) != 1 || notifications[0].MessageID != message.ID || notifications[0].WorkerRunGeneration != generation || notifications[0].SourceCursor != 2 {
		t.Fatalf("notifications=%+v message=%+v", notifications, message)
	}
}

func TestAddEventForRunWrongGenerationIsStaleWithoutProjectionChanges(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))

	message, err := state.AddEventForRun(attempt.ID, generation+1, model.Event{Type: "progress", Payload: "late generation"}, 1, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	storedTask, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedAttempt, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !message.Stale || message.RunGeneration != generation+1 || storedTask.Status != model.TaskStatusWorking || storedAttempt.Status != model.AttemptStatusStarting || storedAttempt.Cursor != 0 || len(notifications) != 0 {
		t.Fatalf("message=%+v task=%+v attempt=%+v notifications=%+v", message, storedTask, storedAttempt, notifications)
	}
}

func TestAddEventForRunChecksCheckpointBeforeTerminalBranchFence(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))

	message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "branch:shephrd/wrong"}, 1, model.WorkspaceFacts{})
	if !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("error=%v", err)
	}
	if message.Direction != "system" || message.Type != "blocked" || !message.Wake || !strings.Contains(message.Payload, "requires a valid checkpoint") || strings.Contains(message.Payload, "exact attempt branch") {
		t.Fatalf("message=%+v", message)
	}
	storedTask, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedTask.Status != model.TaskStatusBlocked || storedTask.ClaimedDone || storedTask.ArtifactRef != "" {
		t.Fatalf("task=%+v", storedTask)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].MessageID != message.ID || notifications[0].Kind != "blocked" {
		t.Fatalf("notifications=%+v", notifications)
	}
}

func TestAddEventForRunNotificationFailureRollsBackEventAndTransition(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
	recordWorkerCheckpoint(t, state, attempt, generation, 1, []string{"answer"})
	if _, err := state.db.Exec(`CREATE TRIGGER fail_event_notification BEFORE INSERT ON driver_notifications BEGIN SELECT RAISE(ABORT, 'forced notification failure'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "Which option?"}, 2, model.WorkspaceFacts{}); err == nil {
		t.Fatal("AddEventForRun succeeded when notification publication failed")
	}
	storedTask, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedAttempt, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := state.Messages(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedTask.Status != model.TaskStatusWorking || storedAttempt.Status != model.AttemptStatusStarting || storedAttempt.Cursor != 1 {
		t.Fatalf("task=%+v attempt=%+v", storedTask, storedAttempt)
	}
	if len(messages) != 2 || messages[0].Type != "checkpoint" || messages[1].Type != "checkpoint" || len(notifications) != 0 {
		t.Fatalf("messages=%+v notifications=%+v", messages, notifications)
	}
}

func TestAddEventForRunProtocolProjectionFailureRollsBackMessagesAndNotification(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := state.db.Exec(`CREATE TRIGGER fail_protocol_projection BEFORE UPDATE OF status ON tasks BEGIN SELECT RAISE(ABORT, 'forced protocol projection failure'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "report:/tmp/report.md"}, 1, model.WorkspaceFacts{}); err == nil {
		t.Fatal("AddEventForRun succeeded when protocol projection failed")
	}
	storedTask, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedAttempt, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := state.Messages(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedTask.Status != model.TaskStatusWorking || storedAttempt.Status != model.AttemptStatusStarting || storedAttempt.Cursor != 0 {
		t.Fatalf("task=%+v attempt=%+v", storedTask, storedAttempt)
	}
	if len(messages) != 1 || messages[0].Direction != "system" || messages[0].Type != "checkpoint" || len(notifications) != 0 {
		t.Fatalf("messages=%+v notifications=%+v", messages, notifications)
	}
}

func TestRepairableCheckpointRejectionAuditsWithoutWakeThenAcceptsCorrection(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "progress", Payload: "working"}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	request := EventRepairRequest{ID: "repair_exact", CandidateHash: strings.Repeat("a", 64)}
	candidate := model.Event{Type: "done", Payload: "done", Artifact: "report:/tmp/report.md"}
	message, diagnostic, err := state.AddEventForRunRepairable(attempt.ID, generation, candidate, 2, model.WorkspaceFacts{}, request)
	if err != nil {
		t.Fatal(err)
	}
	storedTask, _ := state.Task(task.ID)
	storedAttempt, _ := state.Attempt(attempt.ID)
	notifications, _ := state.Notifications(task.ID)
	if diagnostic == nil || diagnostic.Code != adapter.DiagnosticCheckpointRequired || !diagnostic.RequiresCheckpoint {
		t.Fatalf("diagnostic=%+v", diagnostic)
	}
	if message.Type != "protocol-repair" || message.Wake || message.Stale || message.SourceCursor != 2 || storedTask.Status != model.TaskStatusWorking || storedTask.ArtifactRef != "" || storedAttempt.Status != model.AttemptStatusWorking || storedAttempt.Cursor != 2 || len(notifications) != 0 {
		t.Fatalf("message=%+v task=%+v attempt=%+v notifications=%+v", message, storedTask, storedAttempt, notifications)
	}
	recordWorkerCheckpoint(t, state, attempt, generation, 3, []string{})
	accepted, repair, err := state.AddEventForRunRepairable(attempt.ID, generation, candidate, 4, model.WorkspaceFacts{}, request)
	if err != nil || repair != nil || accepted.Stale || !accepted.Wake {
		t.Fatalf("accepted=%+v repair=%+v err=%v", accepted, repair, err)
	}
	storedTask, _ = state.Task(task.ID)
	notifications, _ = state.Notifications(task.ID)
	if storedTask.Status != model.TaskStatusDone || storedTask.ArtifactRef != candidate.Artifact || len(notifications) != 1 {
		t.Fatalf("task=%+v notifications=%+v", storedTask, notifications)
	}
}

func TestRepairableStoreDiagnosticsRemainTyped(t *testing.T) {
	t.Run("checkpoint next steps", func(t *testing.T) {
		state, _, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
		recordWorkerCheckpoint(t, state, attempt, generation, 1, []string{})
		request := EventRepairRequest{ID: "repair_next_steps", CandidateHash: strings.Repeat("1", 64)}
		_, diagnostic, err := state.AddEventForRunRepairable(attempt.ID, generation, model.Event{Type: "question", Payload: "choose"}, 2, model.WorkspaceFacts{}, request)
		if err != nil || diagnostic == nil || diagnostic.Code != adapter.DiagnosticCheckpointNextSteps || !diagnostic.RequiresCheckpoint {
			t.Fatalf("diagnostic=%+v err=%v", diagnostic, err)
		}
	})
	t.Run("branch artifact", func(t *testing.T) {
		state, _, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
		if _, err := state.db.Exec(`UPDATE tasks SET deliverable='code' WHERE id=?`, attempt.TaskID); err != nil {
			t.Fatal(err)
		}
		recordWorkerCheckpoint(t, state, attempt, generation, 1, []string{})
		request := EventRepairRequest{ID: "repair_artifact", CandidateHash: strings.Repeat("2", 64)}
		_, diagnostic, err := state.AddEventForRunRepairable(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "branch:shephrd/wrong"}, 2, model.WorkspaceFacts{}, request)
		if err != nil || diagnostic == nil || diagnostic.Code != adapter.DiagnosticBranchArtifact || diagnostic.RequiresCheckpoint {
			t.Fatalf("diagnostic=%+v err=%v", diagnostic, err)
		}
	})
}

func TestSecondRepairableRejectionBlocksAtomically(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "progress", Payload: "working"}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	request := EventRepairRequest{ID: "repair_exact", CandidateHash: strings.Repeat("b", 64)}
	diagnostic := adapter.Diagnostic{Code: adapter.DiagnosticSchemaUnknownField, Phase: "adapter", Field: "question", Message: `invalid worker event JSON: json: unknown field "question"`}
	if _, repair, err := state.RecordAdapterRejectionForRun(attempt.ID, generation, 2, request, diagnostic); err != nil || repair == nil {
		t.Fatalf("repair=%+v err=%v", repair, err)
	}
	message, repair, err := state.RecordAdapterRejectionForRun(attempt.ID, generation, 3, request, diagnostic)
	if !errors.Is(err, ErrProtocolViolation) || repair != nil || message.Type != "blocked" || !message.Wake || !strings.Contains(message.Payload, "shephrd worker relaunch") || !strings.Contains(message.Payload, "shephrd worker retry") {
		t.Fatalf("message=%+v repair=%+v err=%v", message, repair, err)
	}
	storedTask, _ := state.Task(task.ID)
	storedAttempt, _ := state.Attempt(attempt.ID)
	notifications, _ := state.Notifications(task.ID)
	if storedTask.Status != model.TaskStatusBlocked || storedAttempt.Status != model.AttemptStatusBlocked || len(notifications) != 1 || notifications[0].MessageID != message.ID {
		t.Fatalf("task=%+v attempt=%+v notifications=%+v", storedTask, storedAttempt, notifications)
	}
}

func TestAcceptedTerminalIsNeverRepairable(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
	recordWorkerCheckpoint(t, state, attempt, generation, 1, []string{"ask"})
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "choose"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	request := EventRepairRequest{ID: "repair_exact", CandidateHash: strings.Repeat("f", 64)}
	diagnostic := adapter.Diagnostic{Code: adapter.DiagnosticJSONSyntax, Phase: "adapter", Message: "invalid JSON"}
	if _, _, err := state.RecordAdapterRejectionForRun(attempt.ID, generation, 3, request, diagnostic); !errors.Is(err, ErrStaleRun) {
		t.Fatalf("error=%v", err)
	}
	storedTask, _ := state.Task(task.ID)
	messages, _ := state.Messages(task.ID)
	for _, message := range messages {
		if message.Type == "protocol-repair" || message.Type == "blocked" {
			t.Fatalf("accepted terminal was changed by repair: %+v", messages)
		}
	}
	if storedTask.Status != model.TaskStatusWaiting {
		t.Fatalf("task=%+v", storedTask)
	}
}

func TestRepairAuditFailureRollsBackWorkingState(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "progress", Payload: "working"}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.db.Exec(`CREATE TRIGGER fail_repair_audit BEFORE INSERT ON messages WHEN NEW.type='protocol-repair' BEGIN SELECT RAISE(ABORT, 'forced repair audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	request := EventRepairRequest{ID: "repair_exact", CandidateHash: strings.Repeat("c", 64)}
	diagnostic := adapter.Diagnostic{Code: adapter.DiagnosticJSONSyntax, Phase: "adapter", Message: "invalid JSON"}
	if _, _, err := state.RecordAdapterRejectionForRun(attempt.ID, generation, 2, request, diagnostic); err == nil {
		t.Fatal("repair audit unexpectedly committed")
	}
	storedTask, _ := state.Task(task.ID)
	storedAttempt, _ := state.Attempt(attempt.ID)
	messages, _ := state.Messages(task.ID)
	if storedTask.Status != model.TaskStatusWorking || storedAttempt.Status != model.AttemptStatusWorking || storedAttempt.Cursor != 1 || len(messages) != 2 {
		t.Fatalf("task=%+v attempt=%+v messages=%+v", storedTask, storedAttempt, messages)
	}
}

func TestDriverStopWinsOverLateRepairFailure(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "progress", Payload: "working"}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	request := EventRepairRequest{ID: "repair_exact", CandidateHash: strings.Repeat("d", 64)}
	diagnostic := adapter.Diagnostic{Code: adapter.DiagnosticJSONSyntax, Phase: "adapter", Message: "invalid JSON"}
	if _, _, err := state.RecordAdapterRejectionForRun(attempt.ID, generation, 2, request, diagnostic); err != nil {
		t.Fatal(err)
	}
	if err := state.Stop(task.ID, "driver stop"); err != nil {
		t.Fatal(err)
	}
	late, err := state.RecordRunnerDeathMessage(attempt.ID, generation, "repair timeout")
	if err != nil {
		t.Fatal(err)
	}
	storedTask, _ := state.Task(task.ID)
	storedAttempt, _ := state.Attempt(attempt.ID)
	if !late.Stale || late.Wake || storedTask.Status != model.TaskStatusStopped || storedAttempt.Status != model.AttemptStatusStopped {
		t.Fatalf("late=%+v task=%+v attempt=%+v", late, storedTask, storedAttempt)
	}
}

func TestConcurrentCorrectedTerminalIngestionAcceptsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, task, attempt, generation := eventIngestionFixture(t, path)
	if _, err := first.AddEventForRun(attempt.ID, generation, model.Event{Type: "progress", Payload: "working"}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	request := EventRepairRequest{ID: "repair_exact", CandidateHash: strings.Repeat("e", 64)}
	diagnostic := adapter.Diagnostic{Code: adapter.DiagnosticJSONSyntax, Phase: "adapter", Message: "invalid JSON"}
	if _, _, err := first.RecordAdapterRejectionForRun(attempt.ID, generation, 2, request, diagnostic); err != nil {
		t.Fatal(err)
	}
	recordWorkerCheckpoint(t, first, attempt, generation, 3, []string{})
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	stores := []*Store{first, second}
	messages := make([]model.Message, len(stores))
	errors := make([]error, len(stores))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range stores {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			messages[index], _, errors[index] = stores[index].AddEventForRunRepairable(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "report:/tmp/report.md"}, 4, model.WorkspaceFacts{}, request)
		}()
	}
	close(start)
	wait.Wait()
	accepted, stale := 0, 0
	for index, err := range errors {
		if err != nil {
			t.Fatalf("ingestion %d: %v", index, err)
		}
		if messages[index].Stale {
			stale++
		} else {
			accepted++
		}
	}
	storedTask, _ := first.Task(task.ID)
	notifications, _ := first.Notifications(task.ID)
	if accepted != 1 || stale != 1 || storedTask.Status != model.TaskStatusDone || len(notifications) != 1 {
		t.Fatalf("accepted=%d stale=%d task=%+v notifications=%+v messages=%+v", accepted, stale, storedTask, notifications, messages)
	}
}

func TestEventCandidateRepairRejectsAtomicallyThenAcceptsExactCorrection(t *testing.T) {
	state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "progress", Payload: "working"}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	request := EventRepairRequest{ID: "repair_candidate", CandidateHash: strings.Repeat("a", 64)}
	checkpoint := model.Event{Type: "checkpoint", Payload: "ready", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "ready", Completed: []string{}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}}
	wrongDone := model.Event{Type: "done", Payload: "done", Artifact: "branch:wrong"}
	messages, diagnostic, err := state.AddEventCandidateForRun(attempt.ID, generation, "session", []model.Event{checkpoint, wrongDone}, 2, model.WorkspaceFacts{}, request, false)
	if err != nil || diagnostic == nil || diagnostic.Code != adapter.DiagnosticArtifactContract || len(messages) != 1 || messages[0].Type != "protocol-repair" {
		t.Fatalf("messages=%+v diagnostic=%+v err=%v", messages, diagnostic, err)
	}
	storedCheckpoint, err := state.LatestCheckpoint(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedMessages, _ := state.Messages(task.ID)
	for _, message := range storedMessages {
		if message.Direction == "worker-to-driver" && (message.Type == "checkpoint" || message.Type == "done") {
			t.Fatalf("rejected candidate was partially ingested: %+v", storedMessages)
		}
	}
	if storedCheckpoint.Producer != "system" {
		t.Fatalf("checkpoint=%+v", storedCheckpoint)
	}
	correctedDone := model.Event{Type: "done", Payload: "done", Artifact: "report:/tmp/report.md"}
	accepted, diagnostic, err := state.AddEventCandidateForRun(attempt.ID, generation, "session", []model.Event{checkpoint, correctedDone}, 4, model.WorkspaceFacts{}, request, true)
	if err != nil || diagnostic != nil || len(accepted) != 2 || accepted[0].Type != "checkpoint" || accepted[1].Type != "done" {
		t.Fatalf("accepted=%+v diagnostic=%+v err=%v", accepted, diagnostic, err)
	}
	storedTask, _ := state.Task(task.ID)
	notifications, _ := state.Notifications(task.ID)
	if storedTask.Status != model.TaskStatusDone || storedTask.ArtifactRef != correctedDone.Artifact || len(notifications) != 1 || notifications[0].Kind != "done" {
		t.Fatalf("task=%+v notifications=%+v", storedTask, notifications)
	}
	replayed, _, err := state.AddEventCandidateForRun(attempt.ID, generation, "session", []model.Event{checkpoint, correctedDone}, 6, model.WorkspaceFacts{}, request, true)
	if !errors.Is(err, ErrStaleRun) || len(replayed) != 0 {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	messagesAfterReplay, _ := state.Messages(task.ID)
	doneCount := 0
	for _, message := range messagesAfterReplay {
		if message.Type == "done" && !message.Stale {
			doneCount++
		}
	}
	if doneCount != 1 {
		t.Fatalf("done_count=%d messages=%+v", doneCount, messagesAfterReplay)
	}
}

func TestEventCandidateRepairCursorMatchesRejectedEnvelopeIndex(t *testing.T) {
	for _, index := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("index %d", index), func(t *testing.T) {
			state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
			if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "progress", Payload: "working"}, 1, model.WorkspaceFacts{}); err != nil {
				t.Fatal(err)
			}
			events := make([]model.Event, 0, index+1)
			if index >= 1 {
				events = append(events, model.Event{Type: "progress", Payload: "candidate progress"})
			}
			if index >= 2 {
				events = append(events, model.Event{Type: "checkpoint", Payload: "candidate checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "candidate checkpoint", Completed: []string{}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
			}
			events = append(events, model.Event{Type: "done", Payload: "wrong terminal", Artifact: "branch:wrong"})
			request := EventRepairRequest{ID: fmt.Sprintf("repair_index_%d", index), CandidateHash: strings.Repeat(strconv.Itoa(index+1), 64)}
			messages, diagnostic, err := state.AddEventCandidateForRun(attempt.ID, generation, "session", events, 2, model.WorkspaceFacts{}, request, false)
			wantCursor := int64(2 + index)
			if err != nil || diagnostic == nil || len(messages) != 1 || messages[0].Type != "protocol-repair" || messages[0].SourceCursor != wantCursor {
				t.Fatalf("messages=%+v diagnostic=%+v err=%v", messages, diagnostic, err)
			}
			storedAttempt, _ := state.Attempt(attempt.ID)
			storedMessages, _ := state.Messages(task.ID)
			candidateParts := 0
			for _, message := range storedMessages {
				if message.Direction == "worker-to-driver" && (message.Payload == "candidate progress" || message.Payload == "candidate checkpoint" || message.Payload == "wrong terminal") {
					candidateParts++
				}
			}
			if storedAttempt.Cursor != wantCursor || candidateParts != 0 {
				t.Fatalf("attempt=%+v candidate_parts=%d messages=%+v", storedAttempt, candidateParts, storedMessages)
			}
		})
	}
}

func TestEventCandidateCorrectionFencesIdentitySessionCursorGenerationAndTerminal(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Store, model.Task, model.Attempt, int, *EventRepairRequest, *string, *int64)
		stale  bool
	}{
		{name: "candidate hash mismatch", mutate: func(_ *Store, _ model.Task, _ model.Attempt, _ int, request *EventRepairRequest, _ *string, _ *int64) {
			request.CandidateHash = strings.Repeat("b", 64)
		}},
		{name: "repair id mismatch", mutate: func(_ *Store, _ model.Task, _ model.Attempt, _ int, request *EventRepairRequest, _ *string, _ *int64) {
			request.ID = "repair_other"
		}},
		{name: "session mismatch", mutate: func(_ *Store, _ model.Task, _ model.Attempt, _ int, _ *EventRepairRequest, session *string, _ *int64) {
			*session = "other"
		}, stale: true},
		{name: "cursor replay", mutate: func(_ *Store, _ model.Task, _ model.Attempt, _ int, _ *EventRepairRequest, _ *string, cursor *int64) {
			*cursor = 2
		}, stale: true},
		{name: "stale generation", mutate: func(state *Store, _ model.Task, attempt model.Attempt, generation int, _ *EventRepairRequest, _ *string, _ *int64) {
			if _, err := state.db.Exec(`UPDATE attempts SET run_generation=? WHERE id=?`, generation+1, attempt.ID); err != nil {
				t.Fatal(err)
			}
		}, stale: true},
		{name: "current attempt changed", mutate: func(state *Store, task model.Task, _ model.Attempt, _ int, _ *EventRepairRequest, _ *string, _ *int64) {
			if _, err := state.db.Exec(`UPDATE tasks SET current_attempt_id='' WHERE id=?`, task.ID); err != nil {
				t.Fatal(err)
			}
		}, stale: true},
		{name: "post terminal", mutate: func(state *Store, task model.Task, attempt model.Attempt, generation int, _ *EventRepairRequest, _ *string, _ *int64) {
			recordWorkerCheckpoint(t, state, attempt, generation, 3, []string{"ask"})
			if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "choose"}, 4, model.WorkspaceFacts{}); err != nil {
				t.Fatal(err)
			}
			_ = task
		}, stale: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
			if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "progress", Payload: "working"}, 1, model.WorkspaceFacts{}); err != nil {
				t.Fatal(err)
			}
			request := EventRepairRequest{ID: "repair_exact", CandidateHash: strings.Repeat("a", 64)}
			diagnostic := adapter.Diagnostic{Code: adapter.DiagnosticJSONSyntax, Phase: "adapter", Message: "invalid JSON"}
			if _, _, err := state.RecordAdapterRejectionForRun(attempt.ID, generation, 2, request, diagnostic); err != nil {
				t.Fatal(err)
			}
			session := "session"
			cursor := int64(5)
			test.mutate(state, task, attempt, generation, &request, &session, &cursor)
			checkpoint := model.Event{Type: "checkpoint", Payload: "ready", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "ready", Completed: []string{}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}}
			done := model.Event{Type: "done", Payload: "done", Artifact: "report:/tmp/report.md"}
			accepted, _, err := state.AddEventCandidateForRun(attempt.ID, generation, session, []model.Event{checkpoint, done}, cursor, model.WorkspaceFacts{}, request, true)
			if err == nil || len(accepted) != 0 || test.stale && !errors.Is(err, ErrStaleRun) {
				t.Fatalf("accepted=%+v err=%v", accepted, err)
			}
			messages, _ := state.Messages(task.ID)
			for _, message := range messages {
				if message.Direction == "worker-to-driver" && (message.Type == "checkpoint" || message.Type == "done") && message.SourceCursor >= cursor {
					t.Fatalf("failed correction was partially ingested: %+v", messages)
				}
			}
		})
	}
}

func TestEventCandidateCorrectionRejectsAmbiguousMultipleAndDuplicateEnvelopesWithoutPartialIngestion(t *testing.T) {
	for _, test := range []struct {
		name   string
		events []model.Event
	}{
		{name: "missing terminal", events: []model.Event{{Type: "checkpoint", Payload: "ready", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "ready", Completed: []string{}, NextSteps: []string{"finish"}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}}}},
		{name: "multiple envelopes", events: []model.Event{{Type: "progress", Payload: "extra"}, {Type: "checkpoint", Payload: "ready", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "ready", Completed: []string{}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}}, {Type: "done", Payload: "done", Artifact: "report:/tmp/report.md"}}},
		{name: "duplicate terminal", events: []model.Event{{Type: "checkpoint", Payload: "ready", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "ready", Completed: []string{}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}}, {Type: "done", Payload: "done", Artifact: "report:/tmp/report.md"}, {Type: "failed", Payload: "duplicate"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, task, attempt, generation := eventIngestionFixture(t, filepath.Join(t.TempDir(), "state.db"))
			if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "progress", Payload: "working"}, 1, model.WorkspaceFacts{}); err != nil {
				t.Fatal(err)
			}
			request := EventRepairRequest{ID: "repair_exact", CandidateHash: strings.Repeat("a", 64)}
			diagnostic := adapter.Diagnostic{Code: adapter.DiagnosticJSONSyntax, Phase: "adapter", Message: "invalid JSON"}
			if _, _, err := state.RecordAdapterRejectionForRun(attempt.ID, generation, 2, request, diagnostic); err != nil {
				t.Fatal(err)
			}
			accepted, _, err := state.AddEventCandidateForRun(attempt.ID, generation, "session", test.events, 3, model.WorkspaceFacts{}, request, true)
			if err == nil || len(accepted) != 0 {
				t.Fatalf("accepted=%+v err=%v", accepted, err)
			}
			messages, _ := state.Messages(task.ID)
			for _, message := range messages {
				if message.Direction == "worker-to-driver" && (message.Type == "checkpoint" || message.Type == "done" || message.Type == "failed") {
					t.Fatalf("ambiguous correction was partially ingested: %+v", messages)
				}
			}
		})
	}
}

func TestConcurrentTerminalEventIngestionAcceptsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, task, attempt, generation := eventIngestionFixture(t, path)
	recordWorkerCheckpoint(t, first, attempt, generation, 1, []string{})
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })

	stores := []*Store{first, second}
	messages := make([]model.Message, len(stores))
	errors := make([]error, len(stores))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range stores {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			messages[index], errors[index] = stores[index].AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "report:/tmp/report.md"}, 2, model.WorkspaceFacts{})
		}()
	}
	close(start)
	wait.Wait()

	accepted, stale := 0, 0
	for index, err := range errors {
		if err != nil {
			t.Fatalf("ingestion %d: %v", index, err)
		}
		if messages[index].Stale {
			stale++
		} else {
			accepted++
		}
	}
	storedTask, err := first.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := first.Notifications(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || stale != 1 || storedTask.Status != model.TaskStatusDone || !storedTask.ClaimedDone || len(notifications) != 1 {
		t.Fatalf("accepted=%d stale=%d task=%+v notifications=%+v messages=%+v", accepted, stale, storedTask, notifications, messages)
	}
}
