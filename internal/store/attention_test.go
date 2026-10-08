package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"shephrd/internal/model"
)

const attentionTestDriver = "driver:pi:attention-test"

func attentionFixtureStore(t *testing.T) (*Store, model.Repo) {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "demo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return state, repo
}

func attentionTask(t *testing.T, state *Store, repo model.Repo, feature, driverID string) model.Task {
	t.Helper()
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: driverID, RepoID: repo.ID, FeatureKey: feature,
		Objective: "objective for " + feature, Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// attentionAttempt walks a task through spawn, configure, run-generation
// reservation, and a sealed worker checkpoint so terminal events are accepted.
func attentionAttempt(t *testing.T, state *Store, task model.Task) (model.Attempt, int) {
	t.Helper()
	attempt, err := state.BeginAttempt(task.ID, "claude-code", "model")
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join("/private/attention-worktree-secret", task.ID)
	if err := state.ConfigureAttempt(attempt.ID, "session-"+task.ID, worktree, "lease-"+task.ID, "shephrd/"+task.ID); err != nil {
		t.Fatal(err)
	}
	generation := prepareAttempt(t, state, attempt)
	recordWorkerCheckpoint(t, state, attempt, generation, 2, []string{"finish"})
	return attempt, generation
}

func attentionTerminalEvent(t *testing.T, state *Store, attempt model.Attempt, generation int, typ, artifact string) model.Message {
	t.Helper()
	message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: typ, Payload: "terminal payload for " + attempt.TaskID, Artifact: artifact}, 3, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func attentionLandReport(t *testing.T, state *Store, task model.Task, attempt model.Attempt, message model.Message) {
	t.Helper()
	_, err := state.SetVerifiedReportDelivery(task.ID, attempt.ID, message.ID, model.VerifiedArtifact{
		Kind: "report", OriginalRef: message.ArtifactRef, SHA256: strings.Repeat("a", 64), SizeBytes: 1,
		SnapshotPath: filepath.Join("/verified-reports", attempt.ID+".md"),
	}, "verified report artifact")
	if err != nil {
		t.Fatal(err)
	}
}

func attentionRelease(t *testing.T, state *Store, attemptID string) {
	t.Helper()
	claimed, _, err := state.ClaimRelease(attemptID)
	if err != nil || !claimed {
		t.Fatalf("claim release %s: claimed=%t err=%v", attemptID, claimed, err)
	}
	if err := state.MarkReleased(attemptID); err != nil {
		t.Fatal(err)
	}
}

func attentionAcknowledge(t *testing.T, state *Store, taskID, driverID string) {
	t.Helper()
	generation := "generation:" + taskID
	drain, err := state.DrainNotifications(taskID, driverID, generation, 10)
	if err != nil || len(drain.Notifications) != 1 {
		t.Fatalf("notification drain = %+v, err = %v", drain, err)
	}
	notification := drain.Notifications[0]
	if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: notification.NotificationID, ClaimToken: notification.ClaimToken,
		ConsumerID: driverID, DriverGeneration: generation, HandlingID: "handling:" + taskID}); err != nil {
		t.Fatal(err)
	}
}

func closedLandedTask(t *testing.T, state *Store, repo model.Repo, feature, driverID string) model.Task {
	t.Helper()
	task := attentionTask(t, state, repo, feature, driverID)
	attempt, generation := attentionAttempt(t, state, task)
	message := attentionTerminalEvent(t, state, attempt, generation, "done", "report:"+task.ID+".md")
	attentionLandReport(t, state, task, attempt, message)
	attentionRelease(t, state, attempt.ID)
	attentionAcknowledge(t, state, task.ID, driverID)
	return task
}

func blockedHeldTask(t *testing.T, state *Store, repo model.Repo, feature, driverID string) (model.Task, model.Attempt) {
	t.Helper()
	task := attentionTask(t, state, repo, feature, driverID)
	attempt, generation := attentionAttempt(t, state, task)
	attentionTerminalEvent(t, state, attempt, generation, "blocked", "")
	return task, attempt
}

func snapshotFor(t *testing.T, state *Store, filter model.AttentionFilter) model.AttentionSnapshot {
	t.Helper()
	snapshot, err := state.AttentionSnapshot(filter)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func findItem(snapshot model.AttentionSnapshot, taskID string) *model.AttentionItem {
	for index := range snapshot.Items {
		item := &snapshot.Items[index]
		if item.Task != nil && item.Task.ID == taskID {
			return item
		}
	}
	return nil
}

// TestAttentionClassificationTable exercises the pure classifier across every
// task status crossed with process, artifact, landing, discard, release,
// notification, and archive facts, without any database.
func TestAttentionClassificationTable(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	anchor := now.Add(-30 * time.Minute)
	archived := anchor
	base := func(status string) model.Task {
		return model.Task{Title: "Test task", ID: "task_x", RepoName: "demo", FeatureKey: "f", DriverID: "driver:t", Status: status,
			CreatedAt: anchor, UpdatedAt: anchor}
	}
	held := model.Attempt{ID: "attempt_1", TaskID: "task_x", ReleaseState: "held"}
	released := func() model.Attempt {
		at := anchor
		return model.Attempt{ID: "attempt_1", TaskID: "task_x", ReleaseState: "released", ReleasedAt: &at, Status: "done"}
	}
	cases := []struct {
		name     string
		facts    model.AttentionTaskFacts
		bucket   string
		kind     string
		priority int
		reason   string
	}{
		{"queued", model.AttentionTaskFacts{Task: base("queued"), Now: now}, model.BucketQueued, "queued_unstarted", 210, ""},
		{"working", model.AttentionTaskFacts{Task: base("working"), Now: now}, model.BucketUnderway, "healthy_working", 110, ""},
		{"starting", model.AttentionTaskFacts{Task: base("starting"), Now: now}, model.BucketUnderway, "healthy_working", 110, ""},
		{"waiting", model.AttentionTaskFacts{Task: base("waiting"), Now: now}, model.BucketActNow, "question_waiting", 370, "awaiting_driver_answer"},
		{"blocked held", model.AttentionTaskFacts{Task: func() model.Task { task := base("blocked"); task.CurrentAttemptID = "attempt_1"; return task }(),
			Attempts: []model.Attempt{func() model.Attempt { a := held; a.Status = "blocked"; return a }()}, Now: now},
			model.BucketNeedsDisposition, "terminal_held", 330, "held_attempt"},
		{"failed held", model.AttentionTaskFacts{Task: func() model.Task { task := base("failed"); task.CurrentAttemptID = "attempt_1"; return task }(),
			Attempts: []model.Attempt{func() model.Attempt { a := held; a.Status = "failed"; return a }()}, Now: now},
			model.BucketNeedsDisposition, "terminal_held", 330, "held_attempt"},
		{"stopped held", model.AttentionTaskFacts{Task: func() model.Task { task := base("stopped"); task.CurrentAttemptID = "attempt_1"; return task }(),
			Attempts: []model.Attempt{func() model.Attempt { a := held; a.Status = "stopped"; return a }()}, Now: now},
			model.BucketNeedsDisposition, "terminal_held", 330, "held_attempt"},
		{"done unlanded", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("done")
			task.ClaimedDone, task.CurrentAttemptID = true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt { a := held; a.Status = "done"; return a }()}, Now: now},
			model.BucketResultReady, "artifact_unlanded", 340, "accepted_artifact_without_landing"},
		{"claimed done blocked contradiction", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("blocked")
			task.ClaimedDone, task.CurrentAttemptID = true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt { a := held; a.Status = "blocked"; return a }()}, Now: now},
			model.BucketNeedsDisposition, "terminal_held", 340, "accepted_artifact_without_landing"},
		{"landed needs release", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("done")
			task.ClaimedDone, task.Landed, task.CurrentAttemptID = true, true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt {
			a := held
			a.Status, a.LandedProven, a.LandingKind, a.LandedVerifiedAt = "done", true, "report_artifact", &anchor
			return a
		}()}, Now: now}, model.BucketNeedsDisposition, "release_needed", 350, "landed_with_held_attempt"},
		{"discard authorized needs release", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("stopped")
			task.DiscardAuthorized, task.CurrentAttemptID = true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt { a := held; a.Status = "stopped"; a.DiscardAuthorized = true; return a }()}, Now: now},
			model.BucketNeedsDisposition, "release_needed", 350, "discard_authorized"},
		{"closed landed", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("done")
			task.ClaimedDone, task.Landed, task.CurrentAttemptID = true, true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt {
			a := released()
			a.LandedProven, a.LandingKind, a.LandedVerifiedAt = true, "report_artifact", &anchor
			return a
		}()}, Now: now}, model.BucketClosed, "closed_landed", 0, ""},
		{"closed landed via attested local recovery", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("done")
			task.ClaimedDone, task.Landed, task.CurrentAttemptID = true, true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt {
			a := released()
			a.LandedProven, a.LandingKind, a.LandedVerifiedAt = true, model.LandingKindLocalAttestedAncestry, &anchor
			return a
		}()}, Now: now}, model.BucketClosed, "closed_landed", 0, "artifact_mismatch_recovered"},
		{"closed discarded never landed", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("blocked")
			task.DiscardAuthorized, task.CurrentAttemptID = true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt { a := released(); a.Status = "blocked"; return a }()}, Now: now},
			model.BucketClosed, "closed_discarded", 0, ""},
		{"stopped no work", model.AttentionTaskFacts{Task: base("stopped"), Now: now}, model.BucketClosed, "closed_no_work", 0, ""},
		{"stopped no workspace attempt", model.AttentionTaskFacts{Task: func() model.Task { task := base("stopped"); task.CurrentAttemptID = "attempt_1"; return task }(),
			Attempts: []model.Attempt{{ID: "attempt_1", TaskID: "task_x", ReleaseState: "no_workspace", Status: "stopped"}}, Now: now},
			model.BucketClosed, "closed_no_work", 0, ""},
		{"incomplete landing proof fails safe", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("done")
			task.ClaimedDone, task.Landed, task.CurrentAttemptID = true, true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt {
			a := released()
			a.LandedProven = true
			return a
		}()}, Now: now}, model.BucketActNow, "state_inconsistent", 410, "incomplete_landing_proof"},
		{"unknown workspace", model.AttentionTaskFacts{Task: func() model.Task { task := base("blocked"); task.CurrentAttemptID = "attempt_1"; return task }(),
			Attempts: []model.Attempt{{ID: "attempt_1", TaskID: "task_x", ReleaseState: "held", Status: "unknown"}}, Now: now},
			model.BucketActNow, "state_inconsistent", 410, "unknown_lease_identity"},
		{"stale releasing", model.AttentionTaskFacts{Task: func() model.Task { task := base("blocked"); task.CurrentAttemptID = "attempt_1"; return task }(),
			Attempts: []model.Attempt{func() model.Attempt {
				claimed := now.Add(-time.Hour)
				return model.Attempt{ID: "attempt_1", TaskID: "task_x", ReleaseState: "releasing", Status: "blocked", ReleaseClaimedAt: &claimed}
			}()}, Now: now}, model.BucketActNow, "state_inconsistent", 410, "stale_releasing"},
		{"terminal with recorded runner", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("blocked")
			task.ProcessAlive, task.CurrentAttemptID = true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt { a := held; a.Status = "blocked"; a.RunnerPID = 4242; return a }()}, Now: now},
			model.BucketActNow, "state_inconsistent", 410, "terminal_with_recorded_runner"},
		{"unrecognized status fails safe", model.AttentionTaskFacts{Task: base("weird_status"), Now: now},
			model.BucketActNow, "state_unrecognized", 410, "unrecognized_task_status:weird_status"},
		{"unrecognized release state fails safe", model.AttentionTaskFacts{Task: func() model.Task { task := base("blocked"); task.CurrentAttemptID = "attempt_1"; return task }(),
			Attempts: []model.Attempt{{ID: "attempt_1", TaskID: "task_x", ReleaseState: "mystery", Status: "blocked"}}, Now: now},
			model.BucketActNow, "state_unrecognized", 410, "unrecognized_release_state:mystery"},
		{"archived unresolved", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("blocked")
			task.CurrentAttemptID, task.ArchivedAt = "attempt_1", &archived
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt { a := held; a.Status = "blocked"; return a }()}, Now: now},
			model.BucketActNow, "terminal_held", 410, "archived_unresolved"},
		{"held superseded attempt prevents closure", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("done")
			task.ClaimedDone, task.Landed, task.CurrentAttemptID = true, true, "attempt_2"
			return task
		}(), Attempts: []model.Attempt{
			{ID: "attempt_1", TaskID: "task_x", ReleaseState: "held", Status: "superseded"},
			func() model.Attempt {
				a := released()
				a.ID, a.LandedProven, a.LandingKind, a.LandedVerifiedAt = "attempt_2", true, "report_artifact", &anchor
				return a
			}(),
		}, Now: now}, model.BucketNeedsDisposition, "release_needed", 350, "held_superseded_attempt"},
		{"acknowledged notification does not reopen closed", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("done")
			task.ClaimedDone, task.Landed, task.CurrentAttemptID = true, true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt {
			a := released()
			a.LandedProven, a.LandingKind, a.LandedVerifiedAt = true, "report_artifact", &anchor
			return a
		}()}, LatestNotificationKind: "done", LatestNotificationState: "acknowledged", Now: now},
			model.BucketClosed, "closed_landed", 0, ""},
		{"pending notification prevents closure", model.AttentionTaskFacts{Task: func() model.Task {
			task := base("done")
			task.ClaimedDone, task.Landed, task.CurrentAttemptID = true, true, "attempt_1"
			return task
		}(), Attempts: []model.Attempt{func() model.Attempt {
			a := released()
			a.LandedProven, a.LandingKind, a.LandedVerifiedAt = true, "report_artifact", &anchor
			return a
		}()}, ActiveNotifications: 1, LatestNotificationKind: "done", LatestNotificationState: "pending", Now: now},
			model.BucketNeedsDisposition, "unhandled_notification", 330, "active_notification"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			item := model.ClassifyAttention(test.facts)
			if item.Bucket != test.bucket || item.Kind != test.kind || item.Priority != test.priority {
				t.Fatalf("classified bucket=%s kind=%s priority=%d, want %s/%s/%d",
					item.Bucket, item.Kind, item.Priority, test.bucket, test.kind, test.priority)
			}
			if test.reason != "" {
				found := false
				for _, code := range item.ReasonCodes {
					if code == test.reason {
						found = true
					}
				}
				if !found {
					t.Fatalf("reason codes %v miss %q", item.ReasonCodes, test.reason)
				}
			}
			if item.AttentionSince.IsZero() || item.AttentionSinceSource == "" || item.AgeBucket == "" {
				t.Fatalf("age fields incomplete: %+v", item)
			}
		})
	}
}

func TestAttentionAgeBucketsAndAnchors(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	questionAt := now.Add(-time.Hour)
	item := model.ClassifyAttention(model.AttentionTaskFacts{Task: model.Task{Title: "Test task", ID: "task_q", Status: "waiting", CreatedAt: now, UpdatedAt: now},
		LatestQuestionAt: &questionAt, Now: now})
	if item.AttentionSinceSource != "accepted_question" || !item.AttentionSince.Equal(questionAt) {
		t.Fatalf("question anchor = %s %s", item.AttentionSinceSource, item.AttentionSince)
	}
	terminalAt := now.Add(-2 * time.Hour)
	item = model.ClassifyAttention(model.AttentionTaskFacts{Task: model.Task{Title: "Test task", ID: "task_b", Status: "blocked", CurrentAttemptID: "attempt_1", CreatedAt: now, UpdatedAt: now},
		Attempts:         []model.Attempt{{ID: "attempt_1", TaskID: "task_b", ReleaseState: "held", Status: "blocked"}},
		LatestTerminalAt: &terminalAt, Now: now})
	if item.AttentionSinceSource != "accepted_terminal_message" || !item.AttentionSince.Equal(terminalAt) {
		t.Fatalf("terminal anchor = %s %s", item.AttentionSinceSource, item.AttentionSince)
	}
}

// TestAttentionIncidentFixture reproduces the investigated incident shape at
// reduced scale ratios: the flat list contains 28 unarchived tasks but only
// six require disposition, two are underway, and twenty are safely closed.
func TestAttentionIncidentFixture(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	for index := 0; index < 19; index++ {
		closedLandedTask(t, state, repo, fmt.Sprintf("landed-%d", index), attentionTestDriver)
	}
	discarded := attentionTask(t, state, repo, "discarded", attentionTestDriver)
	discardedAttempt, discardedGeneration := attentionAttempt(t, state, discarded)
	attentionTerminalEvent(t, state, discardedAttempt, discardedGeneration, "blocked", "")
	if err := state.AuthorizeDiscard(discarded.ID, discardedAttempt.ID); err != nil {
		t.Fatal(err)
	}
	attentionRelease(t, state, discardedAttempt.ID)
	for index := 0; index < 5; index++ {
		blockedHeldTask(t, state, repo, fmt.Sprintf("blocked-%d", index), attentionTestDriver)
	}
	stopped := attentionTask(t, state, repo, "stopped-held", attentionTestDriver)
	attentionAttempt(t, state, stopped)
	if err := state.Stop(stopped.ID, "driver stop"); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		task := attentionTask(t, state, repo, fmt.Sprintf("working-%d", index), attentionTestDriver)
		attentionAttempt(t, state, task)
	}

	snapshot := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver})
	counts := snapshot.Counts
	if counts.NeedsDisposition != 6 || counts.Underway != 2 || counts.Closed != 20 || counts.ActNow != 0 || counts.ResultReady != 0 {
		t.Fatalf("incident counts = %+v", counts)
	}
	// The default inbox shows only the six disposition rows: at least 75
	// percent fewer rows than the 28-task flat list, with zero closed rows.
	if len(snapshot.Items) != 6 {
		t.Fatalf("default inbox has %d rows, want 6", len(snapshot.Items))
	}
	for _, item := range snapshot.Items {
		if item.Bucket == model.BucketClosed || item.Bucket == model.BucketUnderway {
			t.Fatalf("default inbox leaked %s row %s", item.Bucket, item.AttentionKey)
		}
	}
	revealed := map[string]int{}
	for _, omission := range snapshot.Omitted {
		if omission.Reveal == "" || omission.Count <= 0 {
			t.Fatalf("omission missing reveal guidance: %+v", omission)
		}
		revealed[omission.Section] = omission.Count
	}
	if revealed[model.BucketClosed] != 20 || revealed[model.BucketUnderway] != 2 {
		t.Fatalf("omissions = %v", revealed)
	}
	portfolio := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Portfolio: true})
	if len(portfolio.Items) != 28 {
		t.Fatalf("portfolio has %d rows, want 28", len(portfolio.Items))
	}
	if item := findItem(portfolio, discarded.ID); item == nil || item.Kind != "closed_discarded" || item.Task.Landed {
		t.Fatalf("discarded task row = %+v", item)
	}
}

func TestAttentionLatestDecisionAppearsOnlyOnSpecifiedRowsWithoutReclassification(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	blocked, _ := blockedHeldTask(t, state, repo, "decision-blocked", attentionTestDriver)
	before := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), blocked.ID)
	if before == nil {
		t.Fatal("blocked row missing before decision")
	}
	longDecision := strings.Repeat("d", 200)
	if _, err := state.AddAnnotation(model.AnnotationScope{TaskID: blocked.ID}, attentionTestDriver, 0,
		model.AnnotationInput{Judgment: longDecision}); err != nil {
		t.Fatal(err)
	}
	after := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), blocked.ID)
	if after == nil || after.Bucket != before.Bucket || after.Kind != before.Kind || after.Priority != before.Priority || len(after.LatestAnnotation) != 160 {
		t.Fatalf("decision reclassified row: before=%+v after=%+v", before, after)
	}
	result := attentionTask(t, state, repo, "decision-result", attentionTestDriver)
	resultAttempt, resultGeneration := attentionAttempt(t, state, result)
	attentionTerminalEvent(t, state, resultAttempt, resultGeneration, "done", "report:decision-result.md")
	if _, err := state.AddAnnotation(model.AnnotationScope{TaskID: result.ID}, attentionTestDriver, 0,
		model.AnnotationInput{Judgment: "verify the report"}); err != nil {
		t.Fatal(err)
	}
	resultRow := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), result.ID)
	if resultRow == nil || resultRow.Bucket != model.BucketResultReady || resultRow.LatestAnnotation != "verify the report" {
		t.Fatalf("result-ready decision summary = %+v", resultRow)
	}
	waiting := attentionTask(t, state, repo, "decision-waiting", attentionTestDriver)
	attempt, generation := attentionAttempt(t, state, waiting)
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "choose"}, 3, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddAnnotation(model.AnnotationScope{TaskID: waiting.ID}, attentionTestDriver, 0,
		model.AnnotationInput{Judgment: "answer pending"}); err != nil {
		t.Fatal(err)
	}
	waitingRow := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), waiting.ID)
	if waitingRow == nil || waitingRow.Bucket != model.BucketActNow || waitingRow.LatestAnnotation != "" {
		t.Fatalf("act-now decision summary = %+v", waitingRow)
	}
}

func TestAttentionLandingReleaseAndDiscardFlows(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	task := attentionTask(t, state, repo, "flow", attentionTestDriver)
	attempt, generation := attentionAttempt(t, state, task)
	message := attentionTerminalEvent(t, state, attempt, generation, "done", "report:flow.md")

	item := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), task.ID)
	if item == nil || item.Kind != "artifact_unlanded" || item.Bucket != model.BucketResultReady {
		t.Fatalf("unlanded accepted result = %+v", item)
	}
	attentionLandReport(t, state, task, attempt, message)
	item = findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), task.ID)
	if item == nil || item.Kind != "release_needed" || item.Bucket != model.BucketNeedsDisposition {
		t.Fatalf("landed without release = %+v", item)
	}
	attentionRelease(t, state, attempt.ID)
	item = findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), task.ID)
	if item == nil || item.Kind != "unhandled_notification" || item.Bucket != model.BucketNeedsDisposition || item.Notifications.ActiveCount != 1 {
		t.Fatalf("released unacknowledged report = %+v", item)
	}
	attentionAcknowledge(t, state, task.ID, attentionTestDriver)
	snapshot := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Portfolio: true})
	item = findItem(snapshot, task.ID)
	if item == nil || item.Kind != "closed_landed" || item.Bucket != model.BucketClosed {
		t.Fatalf("acknowledged released report = %+v", item)
	}
}

func TestAttentionReportRecoveryRequiresNoAcceptedCurrentWorkerTerminal(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	for _, terminal := range []string{"blocked", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			task, attempt, generation := reportRecoveryAttentionAttempt(t, state, repo, "ordinary-"+terminal)
			attentionTerminalEvent(t, state, attempt, generation, terminal, "")
			item := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), task.ID)
			if item == nil || item.Kind != "terminal_held" || len(item.RecoveryCommands) != 0 || slicesContain(item.DriverChoices, "task_attest_report_recovery") {
				t.Fatalf("ordinary %s projection = %+v", terminal, item)
			}
			checkpoint, _ := state.LatestCheckpoint(attempt.ID)
			if _, err := state.ReportRecoveryCandidateFor(task.ID, attempt.ID, task.DriverID, generation, checkpoint.Revision, checkpoint.SourceCursor); err == nil || attestationErrorKind(err) != "terminal_event_conflict" {
				t.Fatalf("ordinary %s candidate error = %v", terminal, err)
			}
		})
	}
}

func TestAttentionReportRecoveryAllowsSystemBlockAndIgnoresStaleGenerationTerminal(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	task, attempt, generation := reportRecoveryAttentionAttempt(t, state, repo, "system-blocked")
	if _, err := state.RecordControlFailureMessage(attempt.ID, generation, "terminal handoff failed"); err != nil {
		t.Fatal(err)
	}
	assertReportRecoveryAttention(t, state, task, attempt)
	checkpoint, _ := state.LatestCheckpoint(attempt.ID)
	candidate, err := state.ReportRecoveryCandidateFor(task.ID, attempt.ID, task.DriverID, generation, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.RecordReportRecoveryAttestation(candidate, reportRecoveryStoreEvidence(task, attempt)); err != nil {
		t.Fatal(err)
	}
	item := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), task.ID)
	verifyCommand, retryCommand := model.ReportRecoveryContinuationCommands(task.ID, attempt.ID, task.DriverID)
	if item == nil || item.Kind != "report_recovery_attested" || !reflect.DeepEqual(item.DriverChoices, []string{"task_verify", "task_retry"}) || !reflect.DeepEqual(item.RecoveryCommands, []string{verifyCommand, retryCommand}) || slicesContain(item.DriverChoices, "worker_relaunch") {
		t.Fatalf("attested report recovery projection = %+v", item)
	}

	staleTask, staleAttempt, staleGeneration := reportRecoveryAttentionAttempt(t, state, repo, "stale-terminal")
	attentionTerminalEvent(t, state, staleAttempt, staleGeneration, "blocked", "")
	currentGeneration, err := state.ReserveRunGeneration(staleAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(staleAttempt.ID, currentGeneration, "reassigned", []string{"finish report"}, model.WorkspaceFacts{HeadCommit: strings.Repeat("c", 40)}); err != nil {
		t.Fatal(err)
	}
	checkpointBody := model.Checkpoint{SchemaVersion: 1, Summary: "report complete", Completed: []string{"canonical report written"}, NextSteps: []string{"emit done"}}
	if _, err := state.AddEventForRun(staleAttempt.ID, currentGeneration, model.Event{Type: "checkpoint", Payload: "report complete", Checkpoint: &checkpointBody}, 1, model.WorkspaceFacts{HeadCommit: strings.Repeat("d", 40)}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordControlFailureMessage(staleAttempt.ID, currentGeneration, "terminal handoff failed"); err != nil {
		t.Fatal(err)
	}
	staleAttempt, _ = state.Attempt(staleAttempt.ID)
	assertReportRecoveryAttention(t, state, staleTask, staleAttempt)
}

func TestAttentionReportRecoveryConcurrentTerminalProjectionMatchesAcceptedEvidence(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	for index := 0; index < 12; index++ {
		task, attempt, generation := reportRecoveryAttentionAttempt(t, state, repo, fmt.Sprintf("concurrent-terminal-%d", index))
		start := make(chan struct{})
		errors := make(chan error, 2)
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			_, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "blocked", Payload: "worker blocked"}, 2, model.WorkspaceFacts{HeadCommit: strings.Repeat("b", 40)})
			errors <- err
		}()
		go func() {
			defer group.Done()
			<-start
			_, err := state.RecordControlFailureMessage(attempt.ID, generation, "terminal handoff failed")
			errors <- err
		}()
		close(start)
		group.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
		var accepted int
		if err := state.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND run_generation=? AND direction='worker-to-driver' AND type IN ('question','done','blocked','failed') AND stale=0`, attempt.ID, generation).Scan(&accepted); err != nil {
			t.Fatal(err)
		}
		item := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), task.ID)
		if item == nil {
			t.Fatal("concurrent terminal task missing from attention")
		}
		if accepted == 0 && item.Kind != "report_recovery_available" || accepted != 0 && item.Kind == "report_recovery_available" {
			t.Fatalf("accepted=%d projection=%+v", accepted, item)
		}
	}
}

func reportRecoveryAttentionAttempt(t *testing.T, state *Store, repo model.Repo, feature string) (model.Task, model.Attempt, int) {
	t.Helper()
	task := attentionTask(t, state, repo, feature, attentionTestDriver)
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(attempt.ID, "session-"+task.ID, filepath.Join(t.TempDir(), task.ID), "lease-"+task.ID, "shephrd/"+task.ID); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"write report"}, model.WorkspaceFacts{HeadCommit: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	checkpointBody := model.Checkpoint{SchemaVersion: 1, Summary: "report complete", Completed: []string{"canonical report written"}, NextSteps: []string{"emit done"}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "report complete", Checkpoint: &checkpointBody}, 1, model.WorkspaceFacts{HeadCommit: strings.Repeat("b", 40)}); err != nil {
		t.Fatal(err)
	}
	attempt, _ = state.Attempt(attempt.ID)
	return task, attempt, generation
}

func assertReportRecoveryAttention(t *testing.T, state *Store, task model.Task, attempt model.Attempt) {
	t.Helper()
	item := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), task.ID)
	if item == nil || item.Kind != "report_recovery_available" || len(item.RecoveryCommands) != 1 || !strings.Contains(item.RecoveryCommands[0], attempt.ID) || !slicesContain(item.DriverChoices, "task_attest_report_recovery") {
		t.Fatalf("report recovery projection = %+v", item)
	}
	checkpoint, _ := state.LatestCheckpoint(attempt.ID)
	candidate, err := state.ReportRecoveryCandidateFor(task.ID, attempt.ID, task.DriverID, attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor)
	if err != nil || candidate.RecoveryCommand != item.RecoveryCommands[0] {
		t.Fatalf("candidate=%+v error=%v item=%+v", candidate, err, item)
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestAttentionAcknowledgedNotificationDoesNotCloseRow(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	task, _ := blockedHeldTask(t, state, repo, "acked", attentionTestDriver)
	drain, err := state.DrainNotifications("", attentionTestDriver, "generation-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(drain.Notifications) != 1 {
		t.Fatalf("drained %d notifications", len(drain.Notifications))
	}
	notice := drain.Notifications[0]
	if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: notice.NotificationID, ClaimToken: notice.ClaimToken,
		ConsumerID: attentionTestDriver, DriverGeneration: "generation-1", HandlingID: "handled-1"}); err != nil {
		t.Fatal(err)
	}
	item := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), task.ID)
	if item == nil || item.Bucket != model.BucketNeedsDisposition || item.Kind != "terminal_held" {
		t.Fatalf("acknowledged notification closed the row: %+v", item)
	}
	if item.Notifications.ActiveCount != 0 || item.Notifications.LatestState != "acknowledged" {
		t.Fatalf("notification summary = %+v", item.Notifications)
	}
}

func TestAttentionArchivedUnresolvedStaysVisible(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	task, _ := blockedHeldTask(t, state, repo, "archived-held", attentionTestDriver)
	if _, err := state.ArchiveTask(task.ID); err != nil {
		t.Fatal(err)
	}
	item := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), task.ID)
	if item == nil {
		t.Fatal("archived unresolved task disappeared from attention")
	}
	if item.Bucket != model.BucketActNow || item.Visibility != "archived_unresolved" || item.Priority < 400 {
		t.Fatalf("archived unresolved row = %+v", item)
	}
	// An archived closed task stays out of the default view and out of the
	// portfolio unless explicitly revealed, with an exact omission count.
	closed := closedLandedTask(t, state, repo, "archived-closed", attentionTestDriver)
	if _, err := state.ArchiveTask(closed.ID); err != nil {
		t.Fatal(err)
	}
	portfolio := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Portfolio: true})
	if findItem(portfolio, closed.ID) != nil {
		t.Fatal("archived closed row appeared without --include-archived-closed")
	}
	if portfolio.Counts.ArchivedClosed != 1 {
		t.Fatalf("archived closed count = %d", portfolio.Counts.ArchivedClosed)
	}
	found := false
	for _, omission := range portfolio.Omitted {
		if omission.Section == "archived_closed" && omission.Count == 1 && strings.Contains(omission.Reveal, "--include-archived-closed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("archived closed omission missing: %+v", portfolio.Omitted)
	}
	revealed := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Portfolio: true, IncludeArchivedClosed: true})
	if item := findItem(revealed, closed.ID); item == nil || item.Visibility != "archived_closed" {
		t.Fatalf("revealed archived closed row = %+v", item)
	}
}

// TestAttentionAllAttemptsPreventClosure proves the projection inspects every
// attempt, not only current_attempt_id: a landed, released current attempt
// with a held superseded attempt keeps the task open as release_needed.
func TestAttentionAllAttemptsPreventClosure(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	task := attentionTask(t, state, repo, "superseded-held", attentionTestDriver)
	first, _ := attentionAttempt(t, state, task)
	if err := state.Stop(task.ID, "stop before retry"); err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	second, generation := attentionAttempt(t, state, task)
	message := attentionTerminalEvent(t, state, second, generation, "done", "report:retry.md")
	attentionLandReport(t, state, task, second, message)
	attentionRelease(t, state, second.ID)
	item := findItem(snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver}), task.ID)
	if item == nil {
		t.Fatal("task with held superseded attempt vanished from attention")
	}
	if item.Bucket == model.BucketClosed {
		t.Fatalf("held superseded attempt %s did not prevent closure: %+v", first.ID, item)
	}
	if item.Kind != "release_needed" || item.Attempts.HeldCount != 1 || item.Attempts.ReleasedCount != 1 {
		t.Fatalf("all-attempt aggregate = %+v kind=%s", item.Attempts, item.Kind)
	}
	hasReason := false
	for _, code := range item.ReasonCodes {
		if code == "held_superseded_attempt" {
			hasReason = true
		}
	}
	if !hasReason {
		t.Fatalf("reason codes %v miss held_superseded_attempt", item.ReasonCodes)
	}
}

func TestAttentionNoWorkspaceAndQuestionRows(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	spawnFailed := attentionTask(t, state, repo, "spawn-failed", attentionTestDriver)
	attempt, err := state.BeginAttempt(spawnFailed.ID, "claude-code", "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Stop(spawnFailed.ID, "workspace acquisition failed"); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkNoWorkspace(attempt.ID, "workspace acquisition failed before any worktree existed"); err != nil {
		t.Fatal(err)
	}
	waiting := attentionTask(t, state, repo, "question", attentionTestDriver)
	waitingAttempt, generation := attentionAttempt(t, state, waiting)
	if _, err := state.AddEventForRun(waitingAttempt.ID, generation, model.Event{Type: "question", Payload: "Which option?"}, 3, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Portfolio: true})
	if item := findItem(snapshot, spawnFailed.ID); item == nil || item.Kind != "closed_no_work" || item.Attempts.NoWorkspaceCount != 1 {
		t.Fatalf("no-workspace stopped task = %+v", item)
	}
	if item := findItem(snapshot, waiting.ID); item == nil || item.Kind != "question_waiting" || item.Bucket != model.BucketActNow ||
		item.AttentionSinceSource != "accepted_question" {
		t.Fatalf("waiting question row = %+v", item)
	}
}

// TestAttentionRestartOwnerScope covers driver restart: a new session's
// driver sees an exact cross-owner attention count, --all-drivers disclosure
// works, and no ownership is transferred by reading.
func TestAttentionRestartOwnerScope(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	task, _ := blockedHeldTask(t, state, repo, "prior-owner", "driver:pi:previous-session")
	closedLandedTask(t, state, repo, "prior-closed", "driver:pi:previous-session")
	restarted := snapshotFor(t, state, model.AttentionFilter{DriverID: "driver:pi:new-session"})
	if len(restarted.Items) != 0 {
		t.Fatalf("new driver inbox has %d rows", len(restarted.Items))
	}
	if restarted.Counts.OtherDriverAttention != 1 {
		t.Fatalf("other driver attention = %d, want 1 (closed rows are not attention)", restarted.Counts.OtherDriverAttention)
	}
	disclosed := snapshotFor(t, state, model.AttentionFilter{AllDrivers: true})
	item := findItem(disclosed, task.ID)
	if item == nil || item.Task.DriverID != "driver:pi:previous-session" {
		t.Fatalf("all-drivers disclosure row = %+v", item)
	}
	if disclosed.Counts.OtherDriverAttention != 0 {
		t.Fatalf("all-drivers snapshot reported other-driver count %d", disclosed.Counts.OtherDriverAttention)
	}
	reloaded, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DriverID != "driver:pi:previous-session" {
		t.Fatalf("attention read transferred ownership to %s", reloaded.DriverID)
	}
}

func TestAttentionTruncationDisclosure(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	for index := 0; index < 3; index++ {
		blockedHeldTask(t, state, repo, fmt.Sprintf("truncate-%d", index), attentionTestDriver)
	}
	snapshot := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Limit: 1})
	if len(snapshot.Items) != 1 {
		t.Fatalf("truncated section shows %d rows", len(snapshot.Items))
	}
	if snapshot.Counts.NeedsDisposition != 3 {
		t.Fatalf("counts must be exact totals, got %d", snapshot.Counts.NeedsDisposition)
	}
	found := false
	for _, omission := range snapshot.Omitted {
		if omission.Section == model.BucketNeedsDisposition && omission.Count == 2 && strings.Contains(omission.Reveal, "--limit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("truncation omission missing: %+v", snapshot.Omitted)
	}
	if _, err := state.AttentionSnapshot(model.AttentionFilter{DriverID: attentionTestDriver, Limit: model.AttentionMaxLimit + 1}); err == nil {
		t.Fatal("limit above maximum was accepted")
	}
}

// TestAttentionPrivacyExclusions plants sensitive values in every excluded
// field and asserts none of them appear in the serialized snapshot.
func TestAttentionPrivacyExclusions(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	secretObjective := "SECRET-OBJECTIVE-72fd1"
	secretAcceptance := "SECRET-ACCEPTANCE-72fd2"
	secretPayload := "SECRET-PAYLOAD-72fd3"
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: attentionTestDriver, RepoID: repo.ID, FeatureKey: "private",
		Objective: secretObjective, AcceptanceCriteria: secretAcceptance, Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, generation := attentionAttempt(t, state, task)
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "blocked", Payload: secretPayload}, 3, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	drain, err := state.DrainNotifications("", attentionTestDriver, "generation-privacy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(drain.Notifications) != 1 || drain.Notifications[0].ClaimToken == "" {
		t.Fatalf("expected one claimed notification, got %+v", drain.Notifications)
	}
	claimToken := drain.Notifications[0].ClaimToken

	snapshot := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver})
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	for _, secret := range []string{secretObjective, secretAcceptance, secretPayload, claimToken, "/private/attention-worktree-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("attention JSON leaked %q", secret)
		}
	}
	detailed := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Details: true})
	encoded, err = json.Marshal(detailed)
	if err != nil {
		t.Fatal(err)
	}
	body = string(encoded)
	if !strings.Contains(body, secretObjective) {
		t.Fatal("--details omitted the bounded objective")
	}
	for _, secret := range []string{secretAcceptance, claimToken, "/private/attention-worktree-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("attention --details JSON leaked %q", secret)
		}
	}
}

func TestAttentionPlanProjection(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	list, err := state.CreatePlan("roadmap", attentionTestDriver)
	if err != nil {
		t.Fatal(err)
	}
	blockedItem, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "later work"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	readyItem, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Plan the ready work", Objective: "ready work", FeatureKey: "planned-ready",
		RepoID: repo.ID, AcceptanceCriteria: "criteria"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	producer := closedLandedTask(t, state, repo, "producer", attentionTestDriver)
	producerTask, err := state.Task(producer.ID)
	if err != nil {
		t.Fatal(err)
	}
	message, err := state.AcceptedDoneMessage(producerTask.ID, producerTask.CurrentAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := state.VerifiedArtifactForDoneMessage(message.ID)
	if err != nil || artifact == nil {
		t.Fatalf("artifact=%+v err=%v", artifact, err)
	}
	staleItem, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "stale input work", FeatureKey: "planned-stale",
		RepoID: repo.ID, AcceptanceCriteria: "criteria"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	producerItem, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "producer placeholder"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.db.Exec(`UPDATE plan_items SET dispatched_task_id=? WHERE id=?`, producerTask.ID, producerItem.ID); err != nil {
		t.Fatal(err)
	}
	if err := state.AddPlanPrerequisite(list.ID, staleItem.ID, producerItem.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddPlanReport(list.ID, staleItem.ID, producerItem.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SelectPlanReport(list.ID, staleItem.ID, producerItem.ID, artifact.ID, attentionTestDriver); err != nil {
		t.Fatal(err)
	}
	// Retrying the producer withdraws its accepted report, so the recorded
	// input selection must project as stale without touching the selection.
	if err := state.PrepareRetry(producerTask.ID); err != nil {
		t.Fatal(err)
	}

	snapshot := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Portfolio: true})
	if snapshot.Plans.Status != "available" {
		t.Fatalf("plans status = %s (%s)", snapshot.Plans.Status, snapshot.Plans.Diagnostic)
	}
	if snapshot.Plans.ReadyCount != 1 || snapshot.Plans.BlockedCount != 2 {
		t.Fatalf("plan counts ready=%d blocked=%d", snapshot.Plans.ReadyCount, snapshot.Plans.BlockedCount)
	}
	byItem := map[string]model.AttentionPlanItem{}
	for _, item := range snapshot.Items {
		if item.Planned != nil {
			value := *item.Planned
			byItem[value.ItemID] = value
		}
	}
	if planned := byItem[readyItem.ID]; !planned.Ready || len(planned.ReasonCodes) != 0 || planned.Title != readyItem.Title || planned.FeatureKey != readyItem.FeatureKey {
		t.Fatalf("ready planned item = %+v", planned)
	}
	blockedCodes := strings.Join(byItem[blockedItem.ID].ReasonCodes, ",")
	for _, code := range []string{"repo_required", "feature_required", "acceptance_required"} {
		if !strings.Contains(blockedCodes, code) {
			t.Fatalf("blocked planned reasons %q miss %s", blockedCodes, code)
		}
	}
	if !strings.Contains(strings.Join(byItem[staleItem.ID].ReasonCodes, ","), "report_selection_stale") {
		t.Fatalf("stale input reasons = %v", byItem[staleItem.ID].ReasonCodes)
	}
	var plannedReadyRow, plannedBlockedRow bool
	for _, item := range snapshot.Items {
		if item.Planned != nil && item.Planned.ItemID == readyItem.ID && item.Bucket == model.BucketPlannedReady && item.Kind == "planned_ready" {
			plannedReadyRow = true
		}
		if item.Planned != nil && item.Planned.ItemID == blockedItem.ID && item.Bucket == model.BucketPlannedBlocked {
			plannedBlockedRow = true
		}
	}
	if !plannedReadyRow || !plannedBlockedRow {
		t.Fatalf("planned rows missing: ready=%t blocked=%t", plannedReadyRow, plannedBlockedRow)
	}
	if snapshot.Counts.PlannedReady != 1 || snapshot.Counts.PlannedBlocked != 2 {
		t.Fatalf("planned counts = %+v", snapshot.Counts)
	}
	// Default view includes planned_ready but folds planned_blocked into an
	// omission with reveal guidance.
	inbox := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver})
	for _, item := range inbox.Items {
		if item.Bucket == model.BucketPlannedBlocked {
			t.Fatal("planned_blocked row leaked into the default inbox")
		}
	}
}

func TestAttentionPlanUnavailableDiagnostic(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	task, _ := blockedHeldTask(t, state, repo, "still-renders", attentionTestDriver)
	if _, err := state.db.Exec(`ALTER TABLE plan_items RENAME TO plan_items_broken`); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver})
	if snapshot.Plans.Status != "unavailable" || snapshot.Plans.Diagnostic == "" {
		t.Fatalf("task lists section = %+v", snapshot.Plans)
	}
	if item := findItem(snapshot, task.ID); item == nil {
		t.Fatal("ordinary tasks stopped rendering when the planned section became unavailable")
	}
}

// TestAttentionSnapshotMakesNoMutation fingerprints every row of every table
// before and after repeated snapshot reads, proving the projection performs
// zero task, attempt, message, notification, or plan writes.
func TestAttentionSnapshotMakesNoMutation(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	blockedHeldTask(t, state, repo, "immutable-blocked", attentionTestDriver)
	closedLandedTask(t, state, repo, "immutable-closed", attentionTestDriver)
	attentionAttempt(t, state, attentionTask(t, state, repo, "immutable-working", attentionTestDriver))
	list, err := state.CreatePlan("immutable", attentionTestDriver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "planned"}, model.PlanItemRelations{}); err != nil {
		t.Fatal(err)
	}
	before := databaseFingerprint(t, state)
	for index := 0; index < 3; index++ {
		snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Portfolio: true, IncludeArchivedClosed: true, Details: true})
		snapshotFor(t, state, model.AttentionFilter{AllDrivers: true})
	}
	after := databaseFingerprint(t, state)
	if before != after {
		t.Fatal("attention snapshot mutated the database")
	}
}

func databaseFingerprint(t *testing.T, state *Store) string {
	t.Helper()
	tableRows, err := state.db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	tables := make([]string, 0)
	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := tableRows.Close(); err != nil {
		t.Fatal(err)
	}
	var builder strings.Builder
	for _, table := range tables {
		rows, err := state.db.Query(`SELECT * FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		lines := make([]string, 0)
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, fmt.Sprintf("%v", values))
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(lines)
		fmt.Fprintf(&builder, "%s:%v\n", table, lines)
	}
	return builder.String()
}

// TestAttentionDeterministicOrderingAndSchema asserts the JSON contract is
// stable: identical databases produce identical ordering and identical keys,
// and every item carries the deterministic ranking fields.
func TestAttentionDeterministicOrderingAndSchema(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	blockedHeldTask(t, state, repo, "order-blocked", attentionTestDriver)
	question := attentionTask(t, state, repo, "order-question", attentionTestDriver)
	questionAttempt, generation := attentionAttempt(t, state, question)
	if _, err := state.AddEventForRun(questionAttempt.ID, generation, model.Event{Type: "question", Payload: "?"}, 3, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	done := attentionTask(t, state, repo, "order-done", attentionTestDriver)
	doneAttempt, doneGeneration := attentionAttempt(t, state, done)
	attentionTerminalEvent(t, state, doneAttempt, doneGeneration, "done", "report:order.md")

	first := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Portfolio: true})
	second := snapshotFor(t, state, model.AttentionFilter{DriverID: attentionTestDriver, Portfolio: true})
	if first.SchemaVersion != model.AttentionSchemaVersion || first.DatabaseSchemaVersion < 9 {
		t.Fatalf("schema versions = %d/%d", first.SchemaVersion, first.DatabaseSchemaVersion)
	}
	keys := func(snapshot model.AttentionSnapshot) []string {
		out := make([]string, 0, len(snapshot.Items))
		for _, item := range snapshot.Items {
			out = append(out, item.AttentionKey+"/"+item.Bucket+"/"+fmt.Sprint(item.Priority))
		}
		return out
	}
	if strings.Join(keys(first), "|") != strings.Join(keys(second), "|") {
		t.Fatalf("ordering is not deterministic:\n%v\n%v", keys(first), keys(second))
	}
	if len(first.Items) < 3 {
		t.Fatalf("expected at least 3 rows, got %d", len(first.Items))
	}
	if first.Items[0].Kind != "question_waiting" {
		t.Fatalf("question must rank first, got %s", first.Items[0].Kind)
	}
	for index := 1; index < len(first.Items); index++ {
		previous, current := first.Items[index-1], first.Items[index]
		if previous.Bucket == current.Bucket && current.Bucket != model.BucketClosed && previous.Priority < current.Priority {
			t.Fatalf("priority ordering violated at %d: %d then %d", index, previous.Priority, current.Priority)
		}
	}
	for _, item := range first.Items {
		if item.AttentionKey == "" || item.Bucket == "" || item.Kind == "" || item.AttentionSinceSource == "" ||
			item.AgeBucket == "" || item.ReasonCodes == nil || item.DriverChoices == nil {
			t.Fatalf("item missing deterministic fields: %+v", item)
		}
	}
}

func TestClassifyAttentionNativeWorkspaceStates(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute)
	stale := now.Add(-time.Hour)
	task := model.Task{Title: "Test task", ID: "task_native", RepoID: "repo_1", RepoName: "demo", FeatureKey: "native",
		DriverID: "driver:test", Status: "working", Deliverable: "code", CurrentAttemptID: "attempt_1",
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	attempt := func(state string, changed *time.Time) model.Attempt {
		return model.Attempt{ID: "attempt_1", TaskID: task.ID, Status: "working", ReleaseState: "held",
			WorkspaceBackend: model.WorkspaceBackendNative, WorkspaceState: state, WorkspaceStateChangedAt: changed}
	}

	// A live spawn briefly in allocating is healthy underway work.
	item := model.ClassifyAttention(model.AttentionTaskFacts{Task: task, Attempts: []model.Attempt{attempt(model.WorkspaceStateAllocating, &fresh)}, Now: now})
	if item.Bucket != model.BucketUnderway || item.Attempts.AllocatingCount != 1 {
		t.Fatalf("fresh allocating item = %+v", item)
	}

	// A stale allocating row is an interrupted allocation that reconcile
	// must classify; archive-independent act-now visibility.
	item = model.ClassifyAttention(model.AttentionTaskFacts{Task: task, Attempts: []model.Attempt{attempt(model.WorkspaceStateAllocating, &stale)}, Now: now})
	if item.Bucket != model.BucketActNow || item.Kind != "state_inconsistent" || !containsReason(item.ReasonCodes, "stale_allocating") {
		t.Fatalf("stale allocating item = %+v", item)
	}

	// An unknown native workspace identity is an act-now inconsistency even
	// when the attempt status was not updated.
	item = model.ClassifyAttention(model.AttentionTaskFacts{Task: task, Attempts: []model.Attempt{attempt(model.WorkspaceStateUnknown, &stale)}, Now: now})
	if item.Bucket != model.BucketActNow || item.Kind != "state_inconsistent" || !containsReason(item.ReasonCodes, "unknown_lease_identity") {
		t.Fatalf("unknown workspace item = %+v", item)
	}

	// Unrecognized workspace states fail safe.
	item = model.ClassifyAttention(model.AttentionTaskFacts{Task: task, Attempts: []model.Attempt{attempt("mystery", &stale)}, Now: now})
	if item.Bucket != model.BucketActNow || item.Kind != "state_unrecognized" || !containsReason(item.ReasonCodes, "unrecognized_workspace_state:mystery") {
		t.Fatalf("unrecognized workspace item = %+v", item)
	}

	// Archive cannot hide a held-but-stale-allocating attempt.
	archivedAt := now.Add(-30 * time.Minute)
	archived := task
	archived.Status, archived.ArchivedAt = "blocked", &archivedAt
	blocked := attempt(model.WorkspaceStateAllocating, &stale)
	blocked.Status = "blocked"
	item = model.ClassifyAttention(model.AttentionTaskFacts{Task: archived, Attempts: []model.Attempt{blocked}, Now: now})
	if item.Bucket != model.BucketActNow || item.Visibility != "archived_unresolved" || !containsReason(item.ReasonCodes, "stale_allocating") {
		t.Fatalf("archived stale allocating item = %+v", item)
	}
}

func containsReason(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}
