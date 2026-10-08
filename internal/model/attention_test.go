package model

import (
	"testing"
	"time"
)

func TestClassifyAttentionWithoutStorage(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	updated := now.Add(-time.Hour)
	releasedAt := updated
	verifiedAt := updated
	tests := []struct {
		name   string
		facts  AttentionTaskFacts
		bucket string
		kind   string
	}{
		{name: "queued", facts: AttentionTaskFacts{Task: Task{ID: "queued", Status: TaskStatusQueued, CreatedAt: updated, UpdatedAt: updated}, Now: now}, bucket: BucketQueued, kind: "queued_unstarted"},
		{name: "waiting", facts: AttentionTaskFacts{Task: Task{ID: "waiting", Status: TaskStatusWaiting, CreatedAt: updated, UpdatedAt: updated}, Now: now}, bucket: BucketActNow, kind: "question_waiting"},
		{name: "unlanded", facts: AttentionTaskFacts{Task: Task{ID: "done", Status: TaskStatusDone, ClaimedDone: true, CurrentAttemptID: "attempt", CreatedAt: updated, UpdatedAt: updated}, Attempts: []Attempt{{ID: "attempt", Status: AttemptStatusDone, ReleaseState: "held"}}, Now: now}, bucket: BucketResultReady, kind: "artifact_unlanded"},
		{name: "closed", facts: AttentionTaskFacts{Task: Task{ID: "closed", Status: TaskStatusDone, ClaimedDone: true, Landed: true, CurrentAttemptID: "attempt", CreatedAt: updated, UpdatedAt: updated}, Attempts: []Attempt{{ID: "attempt", Status: AttemptStatusDone, ReleaseState: "released", ReleasedAt: &releasedAt, LandedProven: true, LandingKind: "report_artifact", LandedVerifiedAt: &verifiedAt}}, Now: now}, bucket: BucketClosed, kind: "closed_landed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := ClassifyAttention(test.facts)
			if item.Bucket != test.bucket || item.Kind != test.kind || item.AttentionSince.IsZero() || item.AgeBucket == "" {
				t.Fatalf("attention item = %+v", item)
			}
		})
	}
}

func TestProjectAttentionScopesAndPlansWithoutStorage(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	facts := []AttentionTaskFacts{
		{Task: Task{ID: "owned", DriverID: "driver:one", Status: TaskStatusWaiting, CreatedAt: now, UpdatedAt: now}, Now: now},
		{Task: Task{ID: "other", DriverID: "driver:two", Status: TaskStatusWaiting, CreatedAt: now, UpdatedAt: now}, Now: now},
	}
	plans := AttentionPlans{Status: "available"}
	planned := []AttentionPlanItem{{ItemID: "ready", DriverID: "driver:one", Ready: true, UpdatedAt: now}}
	counts, projectedPlans, items, omitted := ProjectAttention(facts, plans, planned, AttentionFilter{DriverID: "driver:one", Limit: AttentionDefaultLimit}, now)
	if counts.ActNow != 1 || counts.PlannedReady != 1 || counts.OtherDriverAttention != 1 || projectedPlans.ReadyCount != 1 || len(items) != 2 || len(omitted) != 0 {
		t.Fatalf("counts=%+v plans=%+v items=%+v omitted=%+v", counts, projectedPlans, items, omitted)
	}
}

func TestStatusTransitionsWithoutStorage(t *testing.T) {
	if err := ValidateTransition(TaskStatusQueued, TaskStatusStarting, false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTransition(TaskStatusDone, TaskStatusQueued, true); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTransition(TaskStatusDone, TaskStatusWorking, false); err == nil {
		t.Fatal("terminal transition succeeded")
	}
}
