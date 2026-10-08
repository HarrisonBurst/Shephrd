package store

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestAttentionMixedNotificationSources(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	mainTask, _ := blockedHeldTask(t, state, repo, "main", attentionTestDriver)
	otherTask, _ := blockedHeldTask(t, state, repo, "other", "driver:other")
	closedTask := closedLandedTask(t, state, repo, "closed", attentionTestDriver)
	var fence model.SubdriverFence
	var child model.Task
	for _, owner := range []string{attentionTestDriver, "driver:other", "driver:subdriver-only"} {
		request, err := state.HandoffSubdriver(repo.ID, "", owner, "request", "Fixture request", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if fence.ID == "" {
			fence = subdriverStartFixture(t, state, request.SubdriverID)
			child, err = state.DispatchSubdriverWorker(fence, request.ID, "child", model.Task{Title: "Child", FeatureKey: "child", Objective: "Scoped work", Deliverable: "report"})
			if err != nil {
				t.Fatal(err)
			}
			attempt, generation := attentionAttempt(t, state, child)
			attentionTerminalEvent(t, state, attempt, generation, "blocked", "")
		}
		if _, err := state.SubdriverReturn(fence, request.ID, "result", "result", "Result is not task delivery proof"); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := state.CreatePlan("ready", attentionTestDriver)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := state.AddPlanItem(plan.ID, model.PlanItem{Title: "Ready", Objective: "Manual dispatch only", RepoID: repo.ID, FeatureKey: "ready", AcceptanceCriteria: "Explicit dispatch"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	var claimed []model.DriverNotification
	for _, phase := range []string{"pending", "claimed", "acknowledged"} {
		t.Run(phase, func(t *testing.T) {
			active := 1
			switch phase {
			case "claimed":
				drain, err := state.DrainNotifications("", attentionTestDriver, "generation:mixed", 10)
				if err != nil || len(drain.Notifications) != 2 {
					t.Fatalf("mixed drain = %+v, err = %v", drain, err)
				}
				claimed = drain.Notifications
			case "acknowledged":
				active = 0
				for _, notice := range claimed {
					if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: notice.NotificationID, ClaimToken: notice.ClaimToken,
						ConsumerID: attentionTestDriver, DriverGeneration: "generation:mixed", HandlingID: "handling:mixed"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := databaseFingerprint(t, state)
			for _, scope := range []struct {
				filter model.AttentionFilter
				counts model.AttentionCounts
				tasks  []string
			}{
				{model.AttentionFilter{DriverID: attentionTestDriver}, model.AttentionCounts{NeedsDisposition: 1, Closed: 1, PlannedReady: 1, OtherDriverAttention: 2}, []string{mainTask.ID, closedTask.ID}},
				{model.AttentionFilter{DriverID: otherTask.DriverID}, model.AttentionCounts{NeedsDisposition: 1, OtherDriverAttention: 2}, []string{otherTask.ID}},
				{model.AttentionFilter{DriverID: child.DriverID}, model.AttentionCounts{NeedsDisposition: 1, OtherDriverAttention: 2}, []string{child.ID}},
				{model.AttentionFilter{DriverID: "driver:subdriver-only"}, model.AttentionCounts{OtherDriverAttention: 3}, []string{}},
				{model.AttentionFilter{AllDrivers: true}, model.AttentionCounts{NeedsDisposition: 3, Closed: 1, PlannedReady: 1}, []string{mainTask.ID, closedTask.ID, otherTask.ID, child.ID}},
			} {
				scope.filter.Portfolio = true
				snapshot := snapshotFor(t, state, scope.filter)
				if snapshot.Counts != scope.counts {
					t.Fatalf("scope %+v counts = %+v, want %+v", scope.filter, snapshot.Counts, scope.counts)
				}
				tasks := make([]string, 0)
				planned := 0
				for _, item := range snapshot.Items {
					if item.Planned != nil {
						planned++
						if item.Planned.ItemID != ready.ID || !item.Planned.Ready || item.Planned.DriverID != attentionTestDriver {
							t.Fatalf("unexpected planned readiness: %+v", item.Planned)
						}
						continue
					}
					if item.Task == nil || item.Notifications == nil {
						t.Fatalf("non-task notification became an obligation: %+v", item)
					}
					tasks = append(tasks, item.Task.ID)
					want := model.AttentionNotificationSummary{ActiveCount: 1, LatestKind: "blocked", LatestState: "pending"}
					switch item.Task.ID {
					case mainTask.ID:
						want.ActiveCount, want.LatestState = active, phase
					case closedTask.ID:
						want = model.AttentionNotificationSummary{LatestKind: "done", LatestState: "acknowledged"}
					}
					if *item.Notifications != want {
						t.Fatalf("task %s notifications = %+v, want %+v", item.Task.ID, item.Notifications, want)
					}
				}
				sort.Strings(tasks)
				sort.Strings(scope.tasks)
				if !reflect.DeepEqual(tasks, scope.tasks) || planned != scope.counts.PlannedReady {
					t.Fatalf("scope %+v task IDs = %v, want %v; planned = %d", scope.filter, tasks, scope.tasks, planned)
				}
			}
			if databaseFingerprint(t, state) != before {
				t.Fatal("obligations mutated notification, readiness or lifecycle state")
			}
		})
	}
}

func TestAttentionNotificationReadErrorIsNotZeroCounts(t *testing.T) {
	state, repo := attentionFixtureStore(t)
	blockedHeldTask(t, state, repo, "blocked", attentionTestDriver)
	if _, err := state.db.Exec(`ALTER TABLE driver_notifications RENAME TO unavailable_notifications`); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AttentionSnapshot(model.AttentionFilter{DriverID: attentionTestDriver}); err == nil || !strings.Contains(err.Error(), "attention notification read") {
		t.Fatalf("notification read error was not propagated: %v", err)
	}
}
