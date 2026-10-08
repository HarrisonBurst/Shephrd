package store

import (
	"strings"
	"sync"
	"testing"
	"time"

	"shephrd/internal/model"
)

func settlementTerminal(t *testing.T, state *Store, attempt model.Attempt, kind string, facts model.WorkspaceFacts) model.DriverNotification {
	t.Helper()
	if err := state.SetRunnerForRun(attempt.ID, attempt.RunGeneration, 43210, true); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: kind + " ready", NextSteps: []string{"finish"}}
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, facts); err != nil {
		t.Fatal(err)
	}
	event := model.Event{Type: kind, Payload: kind + " result"}
	if kind == "done" {
		event.Artifact = "branch:branch"
	}
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, event, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(attempt.TaskID)
	if err != nil || len(notifications) != 1 {
		t.Fatalf("terminal notifications = %+v, err = %v", notifications, err)
	}
	return notifications[0]
}

func acknowledgeSettlementTerminal(t *testing.T, state *Store, task model.Task) model.DriverNotification {
	t.Helper()
	drain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:settlement", 1, 30*time.Second)
	if err != nil || len(drain.Notifications) != 1 {
		t.Fatalf("terminal drain = %+v, err = %v", drain, err)
	}
	notification := drain.Notifications[0]
	if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: notification.NotificationID, ClaimToken: notification.ClaimToken, ConsumerID: task.DriverID, DriverGeneration: "generation:settlement", HandlingID: "handling:settlement"}); err != nil {
		t.Fatal(err)
	}
	return notification
}

func TestRunnerSettlementPublishesOneWakeForAcknowledgedTerminalResidue(t *testing.T) {
	for _, kind := range []string{"question", "done", "blocked", "failed"} {
		t.Run(kind, func(t *testing.T) {
			state, task, attempt := notificationFixture(t)
			defer state.Close()
			terminal := settlementTerminal(t, state, attempt, kind, model.WorkspaceFacts{})
			acknowledgeSettlementTerminal(t, state, task)
			if err := state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, "private runner diagnostic\nnot for presentation"); err != nil {
				t.Fatal(err)
			}
			if err := state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, ""); err != nil {
				t.Fatal(err)
			}
			notifications, err := state.Notifications(task.ID)
			if err != nil || len(notifications) != 2 {
				t.Fatalf("settlement notifications = %+v, err = %v", notifications, err)
			}
			settled := notifications[1]
			if notifications[0].NotificationID != terminal.NotificationID || settled.Kind != model.SettledMessageType || settled.State != model.NotificationPending || settled.WorkerRunGeneration != attempt.RunGeneration || settled.SourceCursor != terminal.SourceCursor || settled.Artifact != "" || len(settled.Payload) > maxSettledNotificationPayload || !strings.Contains(settled.Payload, "evidence, not authority") || !strings.Contains(settled.Payload, "task obligations --all-drivers") || strings.Contains(settled.Payload, "private runner diagnostic") {
				t.Fatalf("settled notification = %+v", settled)
			}
			messages, err := state.Messages(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			settledMessages := 0
			for _, message := range messages {
				if message.Type == model.SettledMessageType {
					settledMessages++
					if message.Direction != "system" || !message.Wake || message.Stale || message.ArtifactRef != "" {
						t.Fatalf("settled message = %+v", message)
					}
				}
			}
			if settledMessages != 1 {
				t.Fatalf("settled message count = %d", settledMessages)
			}
		})
	}
}

func TestConcurrentRunnerSettlementPublishesOnce(t *testing.T) {
	state, task, attempt := notificationFixture(t)
	defer state.Close()
	settlementTerminal(t, state, attempt, "done", model.WorkspaceFacts{})
	acknowledgeSettlementTerminal(t, state, task)
	var path string
	if err := state.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	stores := []*Store{state, second}
	errors := make([]error, len(stores))
	var wait sync.WaitGroup
	for index, current := range stores {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errors[index] = current.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, "")
		}()
	}
	wait.Wait()
	for _, err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil || len(notifications) != 2 || notifications[1].Kind != model.SettledMessageType {
		t.Fatalf("notifications = %+v, err = %v", notifications, err)
	}
}

func TestRunnerSettlementDoesNotDuplicatePresentTerminalWakeAcrossAckDeathOrderings(t *testing.T) {
	for _, stateName := range []string{"pending", "claimed"} {
		t.Run(stateName, func(t *testing.T) {
			state, task, attempt := notificationFixture(t)
			defer state.Close()
			terminal := settlementTerminal(t, state, attempt, "done", model.WorkspaceFacts{})
			var claimed model.DriverNotification
			if stateName == "claimed" {
				drain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:death-first", 1, 30*time.Second)
				if err != nil || len(drain.Notifications) != 1 {
					t.Fatalf("claim = %+v, err = %v", drain, err)
				}
				claimed = drain.Notifications[0]
			}
			if err := state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, ""); err != nil {
				t.Fatal(err)
			}
			if stateName == "claimed" {
				if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: claimed.NotificationID, ClaimToken: claimed.ClaimToken, ConsumerID: task.DriverID, DriverGeneration: "generation:death-first", HandlingID: "handling:death-first"}); err != nil {
					t.Fatal(err)
				}
				if err := state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, ""); err != nil {
					t.Fatal(err)
				}
			}
			notifications, err := state.Notifications(task.ID)
			if err != nil || len(notifications) != 1 || notifications[0].NotificationID != terminal.NotificationID || notifications[0].Kind != "done" {
				t.Fatalf("notifications = %+v, err = %v", notifications, err)
			}
		})
	}
}

func TestRunnerSettlementPublishesAfterTerminalSupersessionAndRollsBackAtomically(t *testing.T) {
	t.Run("superseded terminal", func(t *testing.T) {
		state, task, attempt := notificationFixture(t)
		defer state.Close()
		settlementTerminal(t, state, attempt, "done", model.WorkspaceFacts{})
		if _, err := state.SupersedeNotificationsForRun(attempt.ID, attempt.RunGeneration, "terminal disposition deferred"); err != nil {
			t.Fatal(err)
		}
		if err := state.FinishDeadRunnerForRun(attempt.ID, attempt.RunGeneration); err != nil {
			t.Fatal(err)
		}
		notifications, err := state.Notifications(task.ID)
		if err != nil || len(notifications) != 2 || notifications[0].State != model.NotificationSuperseded || notifications[1].Kind != model.SettledMessageType || !strings.Contains(notifications[1].Payload, "swept_dead") {
			t.Fatalf("notifications = %+v, err = %v", notifications, err)
		}
	})

	t.Run("notification failure", func(t *testing.T) {
		state, task, attempt := notificationFixture(t)
		defer state.Close()
		settlementTerminal(t, state, attempt, "done", model.WorkspaceFacts{})
		acknowledgeSettlementTerminal(t, state, task)
		if _, err := state.db.Exec(`CREATE TRIGGER fail_settled_message BEFORE INSERT ON messages WHEN NEW.type='settled' BEGIN SELECT RAISE(ABORT, 'forced settled failure'); END`); err != nil {
			t.Fatal(err)
		}
		if err := state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, ""); err == nil {
			t.Fatal("runner settlement succeeded when wake publication failed")
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
		if err != nil || !storedTask.ProcessAlive || storedAttempt.RunnerPID != 43210 || storedAttempt.ExitCode != nil || len(notifications) != 1 {
			t.Fatalf("task=%+v attempt=%+v notifications=%+v err=%v", storedTask, storedAttempt, notifications, err)
		}
	})
}

func TestSettledNotificationReleaseAndGenerationSupersessionSafety(t *testing.T) {
	t.Run("release preserves actionable question", func(t *testing.T) {
		state, task, attempt := notificationFixture(t)
		defer state.Close()
		settlementTerminal(t, state, attempt, "question", model.WorkspaceFacts{})
		acknowledgeSettlementTerminal(t, state, task)
		if err := state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, ""); err != nil {
			t.Fatal(err)
		}
		if err := state.AuthorizeDiscard(task.ID, attempt.ID); err != nil {
			t.Fatal(err)
		}
		releaseNotificationAttempt(t, state, attempt.ID)
		notifications, err := state.Notifications(task.ID)
		if err != nil || len(notifications) != 2 || notifications[1].State != model.NotificationPending {
			t.Fatalf("released notifications = %+v, err = %v", notifications, err)
		}
		drain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:released-settled", 1, 30*time.Second)
		if err != nil || len(drain.Notifications) != 1 || drain.Notifications[0].Kind != model.SettledMessageType {
			t.Fatalf("released drain = %+v, err = %v", drain, err)
		}
		settled := drain.Notifications[0]
		if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: settled.NotificationID, ClaimToken: settled.ClaimToken, ConsumerID: task.DriverID, DriverGeneration: "generation:released-settled", HandlingID: "handling:released-settled"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("new generation supersedes settled", func(t *testing.T) {
		state, task, attempt := notificationFixture(t)
		defer state.Close()
		settlementTerminal(t, state, attempt, "question", model.WorkspaceFacts{})
		acknowledgeSettlementTerminal(t, state, task)
		if err := state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := state.ReserveRunGeneration(attempt.ID); err != nil {
			t.Fatal(err)
		}
		notifications, err := state.Notifications(task.ID)
		if err != nil || len(notifications) != 2 || notifications[1].State != model.NotificationSuperseded || notifications[1].SupersedeReason != "worker run generation replaced" {
			t.Fatalf("notifications = %+v, err = %v", notifications, err)
		}
	})
}

func TestRunnerSettlementDoesNotBypassReportPresentability(t *testing.T) {
	state, task, attempt := notificationFixtureForDeliverable(t, "report")
	defer state.Close()
	if err := state.SetRunnerForRun(attempt.ID, attempt.RunGeneration, 43210, true); err != nil {
		t.Fatal(err)
	}
	recordWorkerCheckpoint(t, state, attempt, attempt.RunGeneration, 1, []string{"finish"})
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "done", Payload: "report ready", Artifact: "report:result.md"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if err := state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, ""); err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil || len(notifications) != 1 || notifications[0].Kind != "done" || notifications[0].State != model.NotificationPending {
		t.Fatalf("notifications = %+v, err = %v", notifications, err)
	}
	presentable, err := state.NotificationPresentable(notifications[0].NotificationID)
	if err != nil || presentable {
		t.Fatalf("presentable = %t, err = %v", presentable, err)
	}
	drain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:report-settlement", 10, 30*time.Second)
	if err != nil || len(drain.Notifications) != 0 {
		t.Fatalf("report drain = %+v, err = %v", drain, err)
	}
}

func TestRunnerSettlementStaysSilentWhenReleaseIsQuiescent(t *testing.T) {
	state, task, attempt := notificationFixture(t)
	defer state.Close()
	terminal := settlementTerminal(t, state, attempt, "done", model.WorkspaceFacts{HeadCommit: "sealed-commit"})
	checkpoint, err := state.LatestCheckpoint(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordLandingProof(attempt.ID, model.LandingProof{TaskID: task.ID, AttemptID: attempt.ID, RunGeneration: attempt.RunGeneration, Kind: "local_default_branch", SourceCommit: "sealed-commit", TargetRef: "refs/heads/main", TargetCommit: "landed-commit", CheckpointRevision: checkpoint.Revision, VerifiedAt: time.Now().UTC()}, "verified landing"); err != nil {
		t.Fatal(err)
	}
	releaseNotificationAttempt(t, state, attempt.ID)
	if err := state.FinishRunnerForRun(attempt.ID, attempt.RunGeneration, 0, ""); err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil || len(notifications) != 1 || notifications[0].NotificationID != terminal.NotificationID || notifications[0].State != model.NotificationSuperseded {
		t.Fatalf("notifications = %+v, err = %v", notifications, err)
	}
	drain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:quiescent", 10, 30*time.Second)
	if err != nil || len(drain.Notifications) != 0 {
		t.Fatalf("quiescent drain = %+v, err = %v", drain, err)
	}
}
