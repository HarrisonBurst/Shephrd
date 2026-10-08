package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
)

func notificationFixture(t *testing.T) (*Store, model.Task, model.Attempt) {
	return notificationFixtureForDeliverable(t, "code")
}

func notificationFixtureForDeliverable(t *testing.T, deliverable string) (*Store, model.Task, model.Attempt) {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "repo"), DefaultBranch: "main"})
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Choose wake response", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "wake", Objective: "test wake", Deliverable: deliverable})
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		state.Close()
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tmp/tree", "lease", "branch"); err != nil {
		state.Close()
		t.Fatal(err)
	}
	attempt.RunGeneration = prepareAttempt(t, state, attempt)
	return state, task, attempt
}

func landedReportDoneNotification(t *testing.T, state *Store, task model.Task, attempt model.Attempt) model.DriverNotification {
	t.Helper()
	recordWorkerCheckpoint(t, state, attempt, attempt.RunGeneration, 1, []string{"finish"})
	message, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "done", Payload: "complete", Artifact: "report:result.md"}, 2, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetVerifiedReportDelivery(task.ID, attempt.ID, message.ID, model.VerifiedArtifact{
		Kind: "report", OriginalRef: message.ArtifactRef, SHA256: strings.Repeat("a", 64), SizeBytes: 1,
		SnapshotPath: filepath.Join(t.TempDir(), "result.md"),
	}, "verified report artifact"); err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil || len(notifications) != 1 {
		t.Fatalf("notifications = %+v, err = %v", notifications, err)
	}
	return notifications[0]
}

func releaseNotificationAttempt(t *testing.T, state *Store, attemptID string) {
	t.Helper()
	claimed, _, err := state.ClaimRelease(attemptID)
	if err != nil || !claimed {
		t.Fatalf("claim release = %t, err = %v", claimed, err)
	}
	if err := state.MarkReleased(attemptID); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationDrainAckReclaimAndFences(t *testing.T) {
	state, task, attempt := notificationFixture(t)
	defer state.Close()
	recordWorkerCheckpoint(t, state, attempt, attempt.RunGeneration, 1, []string{"choose"})
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "question", Payload: "choose"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	result, err := state.DrainNotifications(task.ID, "driver:test", "generation:1", 10, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Notifications) != 1 || result.Notifications[0].Kind != "question" || result.Notifications[0].TargetDriverID != "driver:test" ||
		result.Notifications[0].TaskTitle != task.Title || result.Notifications[0].FeatureKey != task.FeatureKey ||
		result.Notifications[0].TaskLabel != "demo/Choose wake response [wake]" {
		t.Fatalf("drain = %+v", result)
	}
	notice := result.Notifications[0]
	resolved, err := state.NotificationByClaimToken(notice.ClaimToken)
	if err != nil || resolved.NotificationID != notice.NotificationID || resolved.ClaimOwner != "driver:test" || resolved.DriverGeneration != "generation:1" {
		t.Fatalf("resolved claim = %+v, err = %v", resolved, err)
	}
	wrong := model.NotificationAckRequest{NotificationID: notice.NotificationID, ClaimToken: "wrong", ConsumerID: "driver:test", DriverGeneration: "generation:1", HandlingID: "handle:1"}
	if _, err := state.AckNotification(wrong); !errors.Is(err, ErrNotificationConflict) {
		t.Fatalf("wrong ack error = %v", err)
	}
	stored, err := state.Notification(notice.NotificationID)
	if err != nil || stored.State != model.NotificationClaimed {
		t.Fatalf("stored after wrong ack = %+v, err = %v", stored, err)
	}
	ack := model.NotificationAckRequest{NotificationID: notice.NotificationID, ClaimToken: notice.ClaimToken, ConsumerID: "driver:test", DriverGeneration: "generation:1", HandlingID: "handle:1"}
	receipt, err := state.AckNotification(ack)
	if err != nil || receipt.SchemaVersion != model.NotificationAckSchemaVersion || receipt.Idempotent {
		t.Fatalf("ack = %+v, err = %v", receipt, err)
	}
	resolved, err = state.NotificationByClaimToken(notice.ClaimToken)
	if err != nil || resolved.State != model.NotificationAcknowledged || resolved.HandlingID != ack.HandlingID {
		t.Fatalf("resolved acknowledged claim = %+v, err = %v", resolved, err)
	}
	repeat, err := state.AckNotification(ack)
	if err != nil || repeat.SchemaVersion != model.NotificationAckSchemaVersion || !repeat.Idempotent {
		t.Fatalf("repeat = %+v, err = %v", repeat, err)
	}
	stored, err = state.Notification(notice.NotificationID)
	if err != nil || stored.State != model.NotificationAcknowledged {
		t.Fatalf("stored after ack = %+v, err = %v", stored, err)
	}

	recordWorkerCheckpoint(t, state, attempt, attempt.RunGeneration, 3, []string{"choose again"})
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "question", Payload: "again"}, 4, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	claimed, err := state.DrainNotifications(task.ID, "driver:test", "generation:1", 1, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed.Notifications) != 1 {
		t.Fatalf("second drain = %+v", claimed)
	}
	if _, err := state.db.Exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, stamp(time.Now().UTC().Add(-time.Second)), claimed.Notifications[0].NotificationID); err != nil {
		t.Fatal(err)
	}
	other, err := state.DrainNotifications(task.ID, "driver:next", "generation:2", 1, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if other.Reclaimed != 0 || len(other.Notifications) != 0 {
		t.Fatalf("non-owner drain = %+v", other)
	}
	reclaimed, err := state.DrainNotifications(task.ID, "driver:test", "generation:2", 1, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Reclaimed != 1 || len(reclaimed.Notifications) != 1 || reclaimed.Notifications[0].NotificationID != claimed.Notifications[0].NotificationID || reclaimed.Notifications[0].ClaimToken == claimed.Notifications[0].ClaimToken {
		t.Fatalf("reclaimed = %+v, original = %+v", reclaimed, claimed)
	}
	oldClaim := model.NotificationAckRequest{NotificationID: claimed.Notifications[0].NotificationID, ClaimToken: claimed.Notifications[0].ClaimToken, ConsumerID: "driver:test", DriverGeneration: "generation:1", HandlingID: "handle:old"}
	if _, err := state.AckNotification(oldClaim); !errors.Is(err, ErrNotificationConflict) {
		t.Fatalf("old claim ack error = %v", err)
	}
}

func TestNotificationResolvedClaimRetainsFullMutationFences(t *testing.T) {
	newClaim := func(t *testing.T) (*Store, model.Task, model.DriverNotification) {
		t.Helper()
		state, task, attempt := notificationFixture(t)
		recordWorkerCheckpoint(t, state, attempt, attempt.RunGeneration, 1, []string{"ask"})
		if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "question", Payload: "resolve"}, 2, model.WorkspaceFacts{}); err != nil {
			state.Close()
			t.Fatal(err)
		}
		claimed, err := state.DrainNotifications(task.ID, task.DriverID, "generation:resolved", 1, 30*time.Second)
		if err != nil || len(claimed.Notifications) != 1 {
			state.Close()
			t.Fatalf("claim = %+v, err = %v", claimed, err)
		}
		return state, task, claimed.Notifications[0]
	}

	t.Run("concurrent handling IDs", func(t *testing.T) {
		state, _, claimed := newClaim(t)
		defer state.Close()
		resolved, err := state.NotificationByClaimToken(claimed.ClaimToken)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, handlingID := range []string{"handling:first", "handling:second"} {
			handlingID := handlingID
			go func() {
				<-start
				_, err := state.AckNotification(model.NotificationAckRequest{NotificationID: resolved.NotificationID, ClaimToken: resolved.ClaimToken, ConsumerID: resolved.ClaimOwner, DriverGeneration: resolved.DriverGeneration, HandlingID: handlingID})
				results <- err
			}()
		}
		close(start)
		succeeded, conflicted := 0, 0
		for range 2 {
			err := <-results
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrNotificationConflict):
				conflicted++
			default:
				t.Fatalf("concurrent ack error = %v", err)
			}
		}
		stored, err := state.Notification(claimed.NotificationID)
		if err != nil || succeeded != 1 || conflicted != 1 || stored.State != model.NotificationAcknowledged || stored.HandlingID == "" {
			t.Fatalf("succeeded=%d conflicted=%d notification=%+v err=%v", succeeded, conflicted, stored, err)
		}
		logs, err := state.NotificationDeliveryLogs(claimed.NotificationID, 10)
		if err != nil || len(logs) != 2 || logs[0].Operation != "claim" || logs[1].Operation != "ack" || logs[1].HandlingID != stored.HandlingID {
			t.Fatalf("logs = %+v, err = %v", logs, err)
		}
	})

	t.Run("expiry after resolution", func(t *testing.T) {
		state, _, claimed := newClaim(t)
		defer state.Close()
		resolved, err := state.NotificationByClaimToken(claimed.ClaimToken)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.db.Exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, stamp(time.Now().UTC().Add(-time.Second)), claimed.NotificationID); err != nil {
			t.Fatal(err)
		}
		_, err = state.AckNotification(model.NotificationAckRequest{NotificationID: resolved.NotificationID, ClaimToken: resolved.ClaimToken, ConsumerID: resolved.ClaimOwner, DriverGeneration: resolved.DriverGeneration, HandlingID: "handling:expired"})
		if !errors.Is(err, ErrNotificationExpired) {
			t.Fatalf("expired resolved ack error = %v", err)
		}
		stored, storedErr := state.Notification(claimed.NotificationID)
		if storedErr != nil || stored.State != model.NotificationClaimed || stored.AckedAt != nil {
			t.Fatalf("stored = %+v, err = %v", stored, storedErr)
		}
	})

	t.Run("adoption after resolution", func(t *testing.T) {
		state, task, claimed := newClaim(t)
		defer state.Close()
		resolved, err := state.NotificationByClaimToken(claimed.ClaimToken)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.AdoptTask(task.ID, task.DriverID, "driver:replacement"); err != nil {
			t.Fatal(err)
		}
		_, err = state.AckNotification(model.NotificationAckRequest{NotificationID: resolved.NotificationID, ClaimToken: resolved.ClaimToken, ConsumerID: resolved.ClaimOwner, DriverGeneration: resolved.DriverGeneration, HandlingID: "handling:old-owner"})
		if !errors.Is(err, ErrNotificationConflict) {
			t.Fatalf("adopted resolved ack error = %v", err)
		}
		stored, storedErr := state.Notification(claimed.NotificationID)
		if storedErr != nil || stored.State != model.NotificationPending || stored.TargetDriverID != "driver:replacement" || stored.ClaimToken != "" {
			t.Fatalf("stored = %+v, err = %v", stored, storedErr)
		}
	})
}

func TestNotificationFairnessAndTaskOrdering(t *testing.T) {
	state, firstTask, firstAttempt := notificationFixture(t)
	defer state.Close()
	secondTask, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: firstTask.RepoID, FeatureKey: "second", Objective: "second"})
	if err != nil {
		t.Fatal(err)
	}
	secondAttempt, err := state.BeginAttempt(secondTask.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(secondAttempt.ID, "session-2", "/tmp/tree-2", "lease-2", "branch-2"); err != nil {
		t.Fatal(err)
	}
	secondAttempt.RunGeneration = prepareAttempt(t, state, secondAttempt)
	for index := int64(0); index < 3; index++ {
		recordWorkerCheckpoint(t, state, firstAttempt, firstAttempt.RunGeneration, index*2+1, []string{"ask"})
		if _, err := state.AddEventForRun(firstAttempt.ID, firstAttempt.RunGeneration, model.Event{Type: "question", Payload: "first"}, index*2+2, model.WorkspaceFacts{}); err != nil {
			t.Fatal(err)
		}
	}
	recordWorkerCheckpoint(t, state, secondAttempt, secondAttempt.RunGeneration, 1, []string{"ask"})
	if _, err := state.AddEventForRun(secondAttempt.ID, secondAttempt.RunGeneration, model.Event{Type: "question", Payload: "second"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	result, err := state.DrainNotifications("", "driver:test", "generation:1", 3, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Notifications) != 3 {
		t.Fatalf("drain = %+v", result)
	}
	if result.Notifications[0].TaskID != firstTask.ID || result.Notifications[1].TaskID != firstTask.ID || result.Notifications[2].TaskID != secondTask.ID {
		t.Fatalf("fair order = %+v", result.Notifications)
	}
	if result.Notifications[0].SourceCursor >= result.Notifications[1].SourceCursor {
		t.Fatalf("task order = %+v", result.Notifications)
	}
}

func TestExclusiveDrainWaitsForTheOwnersOutstandingClaim(t *testing.T) {
	state, firstTask, firstAttempt := notificationFixture(t)
	defer state.Close()
	question := func(task model.Task, attempt model.Attempt) {
		t.Helper()
		recordWorkerCheckpoint(t, state, attempt, attempt.RunGeneration, 1, []string{"ask"})
		if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "question", Payload: task.FeatureKey}, 2, model.WorkspaceFacts{}); err != nil {
			t.Fatal(err)
		}
	}
	question(firstTask, firstAttempt)
	tasks := []model.Task{firstTask}
	for _, owner := range []string{"driver:test", "driver:other"} {
		task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: owner, RepoID: firstTask.RepoID, FeatureKey: "exclusive-" + strings.TrimPrefix(owner, "driver:"), Objective: "exclusive"})
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := state.BeginAttempt(task.ID, "pi", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := state.ConfigureAttempt(attempt.ID, "session-"+task.ID, "/tmp/tree-"+task.ID, "lease-"+task.ID, "branch-"+task.ID); err != nil {
			t.Fatal(err)
		}
		attempt.RunGeneration = prepareAttempt(t, state, attempt)
		question(task, attempt)
		tasks = append(tasks, task)
	}
	drain := func(owner, generation string) model.NotificationDrain {
		t.Helper()
		result, err := state.DrainNotificationExclusive(owner, generation, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	first := drain("driver:test", "watch:one")
	if len(first.Notifications) != 1 || first.Notifications[0].TaskID != tasks[0].ID {
		t.Fatalf("first drain = %+v", first)
	}
	claimed := first.Notifications[0]
	if restarted := drain("driver:test", "watch:two"); len(restarted.Notifications) != 0 {
		t.Fatalf("restarted drain claimed past an outstanding claim: %+v", restarted)
	}
	if stored, err := state.Notification(claimed.NotificationID); err != nil || stored.State != model.NotificationClaimed || stored.ClaimToken != claimed.ClaimToken || stored.DriverGeneration != "watch:one" {
		t.Fatalf("outstanding claim changed: %+v %v", stored, err)
	}
	if other := drain("driver:other", "watch:other"); len(other.Notifications) != 1 || other.Notifications[0].TaskID != tasks[2].ID {
		t.Fatalf("unrelated owner drain = %+v", other)
	}
	if _, err := state.db.Exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, stamp(time.Now().UTC().Add(-time.Second)), claimed.NotificationID); err != nil {
		t.Fatal(err)
	}
	expired := drain("driver:test", "watch:two")
	if expired.Reclaimed != 1 || len(expired.Notifications) != 1 || expired.Notifications[0].NotificationID != claimed.NotificationID || expired.Notifications[0].ClaimToken == claimed.ClaimToken {
		t.Fatalf("expired claim was not redelivered first: %+v", expired)
	}
	if blocked := drain("driver:test", "watch:two"); len(blocked.Notifications) != 0 {
		t.Fatalf("drain claimed past its own outstanding claim: %+v", blocked)
	}
	reclaimed := expired.Notifications[0]
	if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: reclaimed.NotificationID, ClaimToken: reclaimed.ClaimToken, ConsumerID: "driver:test", DriverGeneration: "watch:two", HandlingID: "handling:first"}); err != nil {
		t.Fatal(err)
	}
	if next := drain("driver:test", "watch:three"); len(next.Notifications) != 1 || next.Notifications[0].TaskID != tasks[1].ID {
		t.Fatalf("drain after acknowledgement = %+v", next)
	}
}

func TestTaskAdoptionRetargetsAndReleasesClaims(t *testing.T) {
	state, task, attempt := notificationFixture(t)
	defer state.Close()
	recordWorkerCheckpoint(t, state, attempt, attempt.RunGeneration, 1, []string{"ask"})
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "question", Payload: "adopt"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	claimed, err := state.DrainNotifications(task.ID, "driver:test", "generation:1", 1, 30*time.Second)
	if err != nil || len(claimed.Notifications) != 1 {
		t.Fatalf("claim = %+v, err = %v", claimed, err)
	}
	adoption, err := state.AdoptTask(task.ID, task.DriverID, "driver:new")
	if err != nil {
		t.Fatal(err)
	}
	if adoption.PreviousDriverID != "driver:test" || adoption.Retargeted != 1 || adoption.ReleasedClaims != 1 {
		t.Fatalf("adoption = %+v", adoption)
	}
	storedTask, err := state.Task(task.ID)
	if err != nil || storedTask.DriverID != "driver:new" {
		t.Fatalf("task = %+v, err = %v", storedTask, err)
	}
	notification, err := state.Notification(claimed.Notifications[0].NotificationID)
	if err != nil || notification.State != model.NotificationPending || notification.TargetDriverID != "driver:new" || notification.ClaimToken != "" || notification.AckedAt != nil {
		t.Fatalf("notification = %+v, err = %v", notification, err)
	}
	if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: notification.NotificationID, ClaimToken: claimed.Notifications[0].ClaimToken, ConsumerID: "driver:test", DriverGeneration: "generation:1", HandlingID: "old"}); !errors.Is(err, ErrNotificationConflict) {
		t.Fatalf("old owner ack error = %v", err)
	}
	oldOwner, err := state.DrainNotifications(task.ID, "driver:test", "generation:2", 1, 30*time.Second)
	if err != nil || len(oldOwner.Notifications) != 0 {
		t.Fatalf("old owner drain = %+v, err = %v", oldOwner, err)
	}
	newOwner, err := state.DrainNotifications(task.ID, "driver:new", "generation:2", 1, 30*time.Second)
	if err != nil || len(newOwner.Notifications) != 1 {
		t.Fatalf("new owner drain = %+v, err = %v", newOwner, err)
	}
	logs, err := state.NotificationDeliveryLogs(notification.NotificationID, 10)
	if err != nil || len(logs) < 2 || logs[1].Operation != "adopt" || logs[1].TargetDriverID != "driver:new" {
		t.Fatalf("logs = %+v, err = %v", logs, err)
	}
}

func TestReleasedReportDoneNotificationLifecycle(t *testing.T) {
	t.Run("release before claim", func(t *testing.T) {
		state, task, attempt := notificationFixtureForDeliverable(t, "report")
		defer state.Close()
		notice := landedReportDoneNotification(t, state, task, attempt)
		releaseNotificationAttempt(t, state, attempt.ID)

		stored, err := state.Notification(notice.NotificationID)
		if err != nil || stored.State != model.NotificationPending || stored.DeliveryAttempts != 0 || stored.SupersededAt != nil {
			t.Fatalf("released notification = %+v, err = %v", stored, err)
		}
		storedAttempt, err := state.Attempt(attempt.ID)
		if err != nil || storedAttempt.ReleaseState != "released" || storedAttempt.ReleasedAt == nil || !storedAttempt.LandedProven || storedAttempt.LandingKind != "report_artifact" {
			t.Fatalf("released attempt = %+v, err = %v", storedAttempt, err)
		}
		storedTask, err := state.Task(task.ID)
		if err != nil || !storedTask.Landed || storedTask.Status != model.TaskStatusDone {
			t.Fatalf("landed task = %+v, err = %v", storedTask, err)
		}
		logs, err := state.NotificationDeliveryLogs(notice.NotificationID, 10)
		if err != nil || len(logs) != 0 {
			t.Fatalf("pre-claim logs = %+v, err = %v", logs, err)
		}

		drain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:post-release", 1, 30*time.Second)
		if err != nil || len(drain.Notifications) != 1 {
			t.Fatalf("post-release drain = %+v, err = %v", drain, err)
		}
		claimed := drain.Notifications[0]
		if claimed.NotificationID != notice.NotificationID || claimed.State != model.NotificationClaimed || claimed.DeliveryAttempts != 1 || claimed.ClaimToken == "" {
			t.Fatalf("post-release claim = %+v", claimed)
		}
		renewed, err := state.RenewNotification(model.NotificationRenewRequest{NotificationID: claimed.NotificationID, ClaimToken: claimed.ClaimToken,
			ConsumerID: task.DriverID, DriverGeneration: "generation:post-release"}, 30*time.Second)
		if err != nil || renewed.State != model.NotificationClaimed || renewed.ClaimUntil == nil {
			t.Fatalf("post-release renew = %+v, err = %v", renewed, err)
		}
		request := model.NotificationAckRequest{NotificationID: claimed.NotificationID, ClaimToken: claimed.ClaimToken,
			ConsumerID: task.DriverID, DriverGeneration: "generation:post-release", HandlingID: "handling:post-release"}
		receipt, err := state.AckNotification(request)
		if err != nil || receipt.Idempotent {
			t.Fatalf("post-release ack = %+v, err = %v", receipt, err)
		}
		repeat, err := state.AckNotification(request)
		if err != nil || !repeat.Idempotent {
			t.Fatalf("post-release repeat ack = %+v, err = %v", repeat, err)
		}
		finalDrain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:post-release", 1, 30*time.Second)
		if err != nil || len(finalDrain.Notifications) != 0 {
			t.Fatalf("post-ack drain = %+v, err = %v", finalDrain, err)
		}
		stored, err = state.Notification(notice.NotificationID)
		if err != nil || stored.State != model.NotificationAcknowledged || stored.DeliveryAttempts != 1 || stored.HandlingID != request.HandlingID || stored.AckedAt == nil {
			t.Fatalf("acknowledged notification = %+v, err = %v", stored, err)
		}
		logs, err = state.NotificationDeliveryLogs(notice.NotificationID, 10)
		if err != nil || len(logs) != 3 || logs[0].Operation != "claim" || logs[1].Operation != "renew" || logs[2].Operation != "ack" {
			t.Fatalf("delivery logs = %+v, err = %v", logs, err)
		}
	})

	t.Run("release after claim", func(t *testing.T) {
		state, task, attempt := notificationFixtureForDeliverable(t, "report")
		defer state.Close()
		notice := landedReportDoneNotification(t, state, task, attempt)
		drain, err := state.DrainNotifications(task.ID, task.DriverID, "generation:in-flight", 1, 30*time.Second)
		if err != nil || len(drain.Notifications) != 1 {
			t.Fatalf("claim = %+v, err = %v", drain, err)
		}
		claimed := drain.Notifications[0]
		releaseNotificationAttempt(t, state, attempt.ID)
		if err := state.MarkReleased(attempt.ID); err != nil {
			t.Fatal(err)
		}
		stored, err := state.Notification(notice.NotificationID)
		if err != nil || stored.State != model.NotificationClaimed || stored.ClaimToken != claimed.ClaimToken || stored.DeliveryAttempts != 1 || stored.SupersededAt != nil {
			t.Fatalf("in-flight released notification = %+v, err = %v", stored, err)
		}
		if _, err := state.RenewNotification(model.NotificationRenewRequest{NotificationID: claimed.NotificationID, ClaimToken: claimed.ClaimToken,
			ConsumerID: task.DriverID, DriverGeneration: "generation:in-flight"}, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: claimed.NotificationID, ClaimToken: claimed.ClaimToken,
			ConsumerID: task.DriverID, DriverGeneration: "generation:in-flight", HandlingID: "handling:in-flight"}); err != nil {
			t.Fatal(err)
		}
		stored, err = state.Notification(notice.NotificationID)
		if err != nil || stored.State != model.NotificationAcknowledged || stored.DeliveryAttempts != 1 {
			t.Fatalf("post-release acknowledgement = %+v, err = %v", stored, err)
		}
	})
}

func TestReleasedReportDoneNotificationAdoptionAndRetry(t *testing.T) {
	state, task, attempt := notificationFixtureForDeliverable(t, "report")
	defer state.Close()
	notice := landedReportDoneNotification(t, state, task, attempt)
	releaseNotificationAttempt(t, state, attempt.ID)
	claimed, err := state.DrainNotifications(task.ID, task.DriverID, "generation:old", 1, 30*time.Second)
	if err != nil || len(claimed.Notifications) != 1 {
		t.Fatalf("old owner claim = %+v, err = %v", claimed, err)
	}
	adoption, err := state.AdoptTask(task.ID, task.DriverID, "driver:new")
	if err != nil || adoption.Retargeted != 1 || adoption.ReleasedClaims != 1 {
		t.Fatalf("adoption = %+v, err = %v", adoption, err)
	}
	if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: notice.NotificationID, ClaimToken: claimed.Notifications[0].ClaimToken,
		ConsumerID: task.DriverID, DriverGeneration: "generation:old", HandlingID: "handling:old"}); !errors.Is(err, ErrNotificationConflict) {
		t.Fatalf("old owner ack error = %v", err)
	}
	newClaim, err := state.DrainNotifications(task.ID, "driver:new", "generation:new", 1, 30*time.Second)
	if err != nil || len(newClaim.Notifications) != 1 || newClaim.Notifications[0].TargetDriverID != "driver:new" || newClaim.Notifications[0].DeliveryAttempts != 2 {
		t.Fatalf("new owner claim = %+v, err = %v", newClaim, err)
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := state.Notification(notice.NotificationID)
	if err != nil || stored.State != model.NotificationSuperseded || stored.SupersedeReason != "attempt superseded by retry" {
		t.Fatalf("retried notification = %+v, err = %v", stored, err)
	}
	if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: notice.NotificationID, ClaimToken: newClaim.Notifications[0].ClaimToken,
		ConsumerID: "driver:new", DriverGeneration: "generation:new", HandlingID: "handling:new"}); !errors.Is(err, ErrNotificationConflict) {
		t.Fatalf("superseded new owner ack error = %v", err)
	}
}

func TestReleasedLandedCodeDoneNotificationStillSuperseded(t *testing.T) {
	state, task, attempt := notificationFixture(t)
	defer state.Close()
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "code complete", NextSteps: []string{"finish"}}
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{HeadCommit: "sealed-code-commit"}); err != nil {
		t.Fatal(err)
	}
	message, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "done", Payload: "complete", Artifact: "branch:branch"}, 2, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	storedCheckpoint, err := state.LatestCheckpoint(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordLandingProof(attempt.ID, model.LandingProof{TaskID: task.ID, AttemptID: attempt.ID,
		RunGeneration: attempt.RunGeneration, Kind: "local_default_branch", SourceCommit: storedCheckpoint.HeadCommit,
		TargetRef: "refs/heads/main", TargetCommit: "default-code-commit", CheckpointRevision: storedCheckpoint.Revision,
		VerifiedAt: time.Now().UTC()}, "verified code landing"); err != nil {
		t.Fatal(err)
	}
	releaseNotificationAttempt(t, state, attempt.ID)
	notifications, err := state.Notifications(task.ID)
	if err != nil || len(notifications) != 1 || notifications[0].MessageID != message.ID || notifications[0].State != model.NotificationSuperseded || notifications[0].SupersedeReason != "attempt released" {
		t.Fatalf("released code notifications = %+v, err = %v", notifications, err)
	}
}

func TestSupersededNotificationRejectsAckAndRenew(t *testing.T) {
	state, task, attempt := notificationFixture(t)
	defer state.Close()
	recordWorkerCheckpoint(t, state, attempt, attempt.RunGeneration, 1, []string{"ask"})
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "question", Payload: "supersede"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	claimed, err := state.DrainNotifications(task.ID, "driver:test", "generation:1", 1, 30*time.Second)
	if err != nil || len(claimed.Notifications) != 1 {
		t.Fatalf("claim = %+v, err = %v", claimed, err)
	}
	notice := claimed.Notifications[0]
	if _, err := state.SupersedeNotificationsForAttempt(attempt.ID, "attempt released"); err != nil {
		t.Fatal(err)
	}
	ackErr := func() error {
		_, err := state.AckNotification(model.NotificationAckRequest{NotificationID: notice.NotificationID, ClaimToken: notice.ClaimToken, ConsumerID: "driver:test", DriverGeneration: "generation:1", HandlingID: "handling:1"})
		return err
	}()
	if !errors.Is(ackErr, ErrNotificationConflict) {
		t.Fatalf("superseded ack error = %v", ackErr)
	}
	renewErr := func() error {
		_, err := state.RenewNotification(model.NotificationRenewRequest{NotificationID: notice.NotificationID, ClaimToken: notice.ClaimToken, ConsumerID: "driver:test", DriverGeneration: "generation:1"}, 30*time.Second)
		return err
	}()
	if !errors.Is(renewErr, ErrNotificationConflict) {
		t.Fatalf("superseded renew error = %v", renewErr)
	}
	stored, err := state.Notification(notice.NotificationID)
	if err != nil || stored.State != model.NotificationSuperseded {
		t.Fatalf("stored = %+v, err = %v", stored, err)
	}
}

func TestRunGenerationReplacementSupersedesNotification(t *testing.T) {
	state, _, attempt := notificationFixture(t)
	defer state.Close()
	recordWorkerCheckpoint(t, state, attempt, attempt.RunGeneration, 1, []string{"ask"})
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "question", Payload: "replace"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ReserveRunGeneration(attempt.ID); err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(attempt.TaskID)
	if err != nil || len(notifications) != 1 {
		t.Fatalf("notifications = %+v, err = %v", notifications, err)
	}
	if notifications[0].State != model.NotificationSuperseded || notifications[0].SupersedeReason == "" {
		t.Fatalf("notification = %+v", notifications[0])
	}
}
