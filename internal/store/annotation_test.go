package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"shephrd/internal/model"
)

func annotationFixture(t *testing.T) (*Store, model.Task, model.Plan, model.PlanItem) {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", RepoID: repo.ID, FeatureKey: "decision", DriverID: "driver:old", Objective: "work"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := state.CreatePlan("roadmap", "driver:old")
	if err != nil {
		t.Fatal(err)
	}
	item, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "planned"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	return state, task, list, item
}

func TestAnnotationAppendHistoryValidationAndRawSQLImmutability(t *testing.T) {
	state, task, list, item := annotationFixture(t)
	taskScope := model.AnnotationScope{TaskID: task.ID}
	first, err := state.AddAnnotation(taskScope, "driver:old", 0, model.AnnotationInput{
		Judgment: "Approve findings 2 and 5", Reason: "Material correctness issues", NextAction: "worker send approved findings",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.AddAnnotation(taskScope, "driver:old", 1, model.AnnotationInput{Judgment: "Require re-review"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || second.Revision != 2 || first.DriverID != "driver:old" {
		t.Fatalf("decisions = %+v %+v", first, second)
	}
	history, err := state.Annotations(taskScope)
	if err != nil || len(history) != 2 || history[0].ID != first.ID || history[1].ID != second.ID {
		t.Fatalf("history = %+v err=%v", history, err)
	}
	latest, err := state.LatestAnnotation(taskScope)
	if err != nil || latest == nil || latest.ID != second.ID {
		t.Fatalf("latest = %+v err=%v", latest, err)
	}
	if _, err := state.AddAnnotation(taskScope, "driver:old", 0, model.AnnotationInput{Judgment: "stale"}); err == nil || annotationErrorKind(err) != "annotation_revision_conflict" {
		t.Fatalf("stale append = %v", err)
	}
	for _, test := range []struct {
		scope model.AnnotationScope
		input model.AnnotationInput
	}{
		{scope: model.AnnotationScope{}, input: model.AnnotationInput{Judgment: "valid"}},
		{scope: model.AnnotationScope{TaskID: task.ID, PlanID: list.ID, PlanItemID: item.ID}, input: model.AnnotationInput{Judgment: "valid"}},
		{scope: model.AnnotationScope{PlanItemID: item.ID}, input: model.AnnotationInput{Judgment: "valid"}},
		{scope: model.AnnotationScope{PlanID: list.ID}, input: model.AnnotationInput{Judgment: "valid"}},
		{scope: taskScope, input: model.AnnotationInput{}},
		{scope: taskScope, input: model.AnnotationInput{Judgment: " padded"}},
		{scope: taskScope, input: model.AnnotationInput{Judgment: "nul\x00value"}},
		{scope: taskScope, input: model.AnnotationInput{Judgment: strings.Repeat("x", MaxAnnotationJudgment+1)}},
		{scope: taskScope, input: model.AnnotationInput{Judgment: "valid", Reason: strings.Repeat("x", MaxAnnotationReason+1)}},
		{scope: taskScope, input: model.AnnotationInput{Judgment: "valid", NextAction: strings.Repeat("x", MaxAnnotationNextAction+1)}},
	} {
		if _, err := state.AddAnnotation(test.scope, "driver:old", 2, test.input); err == nil {
			t.Fatalf("invalid decision accepted: %+v", test)
		}
	}
	if _, err := state.db.Exec(`UPDATE annotations SET judgment='changed' WHERE id=?`, first.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("raw update = %v", err)
	}
	if _, err := state.db.Exec(`DELETE FROM annotations WHERE id=?`, first.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("raw delete = %v", err)
	}
	otherList, err := state.CreatePlan("other", "driver:old")
	if err != nil {
		t.Fatal(err)
	}
	otherItem, err := state.AddPlanItem(otherList.ID, model.PlanItem{Title: "Test item", Objective: "other"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO annotations(id,driver_id,revision,judgment,created_at) VALUES('bad_xor','driver:old',1,'valid','2026-01-01T00:00:00Z')`,
		`INSERT INTO annotations(id,driver_id,task_id,plan_id,plan_item_id,revision,judgment,created_at) VALUES('bad_both','driver:old','` + task.ID + `','` + list.ID + `','` + item.ID + `',1,'valid','2026-01-01T00:00:00Z')`,
		`INSERT INTO annotations(id,driver_id,task_id,revision,judgment,created_at) VALUES('bad_fk','driver:old','task_missing',1,'valid','2026-01-01T00:00:00Z')`,
		`INSERT INTO annotations(id,driver_id,plan_id,plan_item_id,revision,judgment,created_at) VALUES('bad_item','driver:old','` + list.ID + `','item_missing',1,'valid','2026-01-01T00:00:00Z')`,
		`INSERT INTO annotations(id,driver_id,plan_id,plan_item_id,revision,judgment,created_at) VALUES('bad_cross','driver:old','` + list.ID + `','` + otherItem.ID + `',1,'valid','2026-01-01T00:00:00Z')`,
		`INSERT INTO annotations(id,driver_id,plan_id,plan_item_id,revision,judgment,created_at) VALUES('bad_trim','driver:old','` + list.ID + `','` + item.ID + `',1,' padded','2026-01-01T00:00:00Z')`,
		`INSERT INTO annotations(id,driver_id,task_id,revision,judgment,created_at) VALUES('bad_gap','driver:old','` + task.ID + `',4,'valid','2026-01-01T00:00:00Z')`,
	} {
		if _, err := state.db.Exec(statement); err == nil {
			t.Fatalf("raw invalid insert accepted: %s", statement)
		}
	}
	for _, input := range []model.AnnotationInput{
		{Judgment: "\tinvalid"},
		{Judgment: "invalid\x00value"},
		{Judgment: strings.Repeat("x", MaxAnnotationJudgment+1)},
		{Judgment: "valid", Reason: "\ninvalid"},
		{Judgment: "valid", Reason: strings.Repeat("x", MaxAnnotationReason+1)},
		{Judgment: "valid", NextAction: "invalid\u00a0"},
		{Judgment: "valid", NextAction: strings.Repeat("x", MaxAnnotationNextAction+1)},
	} {
		if _, err := state.db.Exec(`INSERT INTO annotations(id,driver_id,plan_id,plan_item_id,revision,judgment,reason,next_action,created_at)
			VALUES('bad_raw','driver:old',?,?,1,?,?,?,'2026-01-01T00:00:00Z')`, list.ID, item.ID, input.Judgment, input.Reason, input.NextAction); err == nil {
			t.Fatalf("raw invalid fields accepted: %+v", input)
		}
	}
}

func TestAnnotationConcurrentAppendHasOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	repo, err := first.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := first.CreateTask(model.Task{Title: "Test task", RepoID: repo.ID, FeatureKey: "race", DriverID: "driver:race", Objective: "race"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for index, state := range []*Store{first, second} {
		group.Add(1)
		go func(index int, state *Store) {
			defer group.Done()
			<-start
			_, err := state.AddAnnotation(model.AnnotationScope{TaskID: task.ID}, "driver:race", 0,
				model.AnnotationInput{Judgment: "writer " + string(rune('a'+index))})
			results <- err
		}(index, state)
	}
	close(start)
	group.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case annotationErrorKind(err) == "annotation_revision_conflict":
			conflicts++
		default:
			t.Fatalf("concurrent append = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	history, err := first.Annotations(model.AnnotationScope{TaskID: task.ID})
	if err != nil || len(history) != 1 || history[0].Revision != 1 {
		t.Fatalf("history = %+v err=%v", history, err)
	}
}

func TestAnnotationOwnershipAndAdoptionPreserveAuthors(t *testing.T) {
	state, task, list, item := annotationFixture(t)
	taskScope := model.AnnotationScope{TaskID: task.ID}
	itemScope := model.AnnotationScope{PlanID: list.ID, PlanItemID: item.ID}
	if _, err := state.AddAnnotation(taskScope, "driver:old", 0, model.AnnotationInput{Judgment: "old task decision"}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddAnnotation(itemScope, "driver:old", 0, model.AnnotationInput{Judgment: "old item decision"}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AdoptTask(task.ID, "driver:old", "driver:new"); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AdoptPlan(list.ID, "driver:old", "driver:new"); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []model.AnnotationScope{taskScope, itemScope} {
		if _, err := state.AddAnnotation(scope, "driver:old", 1, model.AnnotationInput{Judgment: "refused"}); err == nil {
			t.Fatalf("old owner appended to %+v", scope)
		}
		if _, err := state.AddAnnotation(scope, "driver:new", 1, model.AnnotationInput{Judgment: "new owner decision"}); err != nil {
			t.Fatal(err)
		}
		history, err := state.Annotations(scope)
		if err != nil || len(history) != 2 || history[0].DriverID != "driver:old" || history[1].DriverID != "driver:new" {
			t.Fatalf("adopted history = %+v err=%v", history, err)
		}
	}
	list, err := state.Plan(list.ID, "driver:new")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.AttachLatestPlanAnnotations(&list); err != nil {
		t.Fatal(err)
	}
	if list.Items[0].LatestAnnotation == nil || list.Items[0].LatestAnnotation.Revision != 2 {
		t.Fatalf("list latest annotation = %+v", list.Items[0].LatestAnnotation)
	}
	detail, err := state.Detail(task.ID)
	if err != nil || len(detail.Annotations) != 2 {
		t.Fatalf("detail decisions = %+v err=%v", detail.Annotations, err)
	}
	encoded, err := json.Marshal(detail)
	if err != nil || !strings.Contains(string(encoded), "old task decision") {
		t.Fatalf("detail JSON = %s err=%v", encoded, err)
	}
}

func TestItemAnnotationHistorySurvivesUndispatchedEditAndRemoval(t *testing.T) {
	state, task, list, first := annotationFixture(t)
	second, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "second"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	third, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "third"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	scope := model.AnnotationScope{PlanID: list.ID, PlanItemID: second.ID}
	record, err := state.AddAnnotation(scope, "driver:old", 0, model.AnnotationInput{Judgment: "retain this judgment"})
	if err != nil {
		t.Fatal(err)
	}
	revised := "revised second"
	if _, err := state.UpdatePlanItem(list.ID, second.ID, model.PlanItemUpdate{Objective: &revised}); err != nil {
		t.Fatal(err)
	}
	if err := state.DeletePlanItem(list.ID, second.ID); err != nil {
		t.Fatalf("decided undispatched item was not removable: %v", err)
	}
	persisted, err := state.Plan(list.ID, "driver:old")
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Items) != 2 || persisted.Items[0].ID != first.ID || persisted.Items[0].Position != 1 || persisted.Items[1].ID != third.ID || persisted.Items[1].Position != 2 {
		t.Fatalf("items after removal = %+v", persisted.Items)
	}
	history, err := state.Annotations(scope)
	if err != nil || len(history) != 1 || history[0].ID != record.ID || history[0].PlanID != list.ID || history[0].PlanItemID != second.ID {
		t.Fatalf("retained history = %+v err=%v", history, err)
	}
	if _, err := state.AddAnnotation(scope, "driver:old", 1, model.AnnotationInput{Judgment: "invalid append"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("removed item accepted append: %v", err)
	}
	feature, acceptance, repoID := "dispatched-delete", "done", task.RepoID
	dispatchedItem, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "dispatch", FeatureKey: feature, AcceptanceCriteria: acceptance, RepoID: repoID}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.DispatchPlanItem(list.ID, dispatchedItem.ID, "driver:old"); err != nil {
		t.Fatal(err)
	}
	if err := state.DeletePlanItem(list.ID, dispatchedItem.ID); err == nil || !strings.Contains(err.Error(), "already dispatched") {
		t.Fatalf("dispatched item deletion = %v", err)
	}
}

func TestAnnotationsDoNotChangeReadinessDispatchSpawnVerificationReleaseOrNotifications(t *testing.T) {
	state, task, list, item := annotationFixture(t)
	feature, acceptance, deliverable, repoID := "planned-decision", "done", "code", task.RepoID
	item, err := state.UpdatePlanItem(list.ID, item.ID, model.PlanItemUpdate{
		FeatureKey: &feature, AcceptanceCriteria: &acceptance, Deliverable: &deliverable, RepoID: &repoID,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := item.Readiness
	if _, err := state.AddAnnotation(model.AnnotationScope{PlanID: list.ID, PlanItemID: item.ID}, "driver:old", 0,
		model.AnnotationInput{Judgment: "dispatch only after explicit command"}); err != nil {
		t.Fatal(err)
	}
	revisedObjective := "revised before dispatch"
	after, err := state.UpdatePlanItem(list.ID, item.ID, model.PlanItemUpdate{Objective: &revisedObjective})
	if err != nil {
		t.Fatalf("decision froze undispatched scope: %v", err)
	}
	if !reflect.DeepEqual(before, after.Readiness) || !after.Readiness.Ready {
		t.Fatalf("readiness changed: before=%+v after=%+v", before, after.Readiness)
	}
	dispatched, err := state.DispatchPlanItem(list.ID, item.ID, "driver:old")
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := state.Attempts(dispatched.ID)
	if err != nil || dispatched.Status != model.TaskStatusQueued || len(attempts) != 0 {
		t.Fatalf("dispatch controlled worker: task=%+v attempts=%+v err=%v", dispatched, attempts, err)
	}
	report, err := state.CreateTask(model.Task{Title: "Test task", RepoID: task.RepoID, FeatureKey: "decision-lifecycle", DriverID: "driver:old", Objective: "report", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddAnnotation(model.AnnotationScope{TaskID: report.ID}, "driver:old", 0,
		model.AnnotationInput{Judgment: "record only"}); err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(report.ID)
	if err != nil || len(notifications) != 0 {
		t.Fatalf("decision emitted notifications: %+v err=%v", notifications, err)
	}
	messages, err := state.Messages(report.ID)
	if err != nil || len(messages) != 0 {
		t.Fatalf("decision emitted worker messages: %+v err=%v", messages, err)
	}
	current, err := state.Task(report.ID)
	if err != nil || current.Status != model.TaskStatusQueued || current.CurrentAttemptID != "" {
		t.Fatalf("decision started task: %+v err=%v", current, err)
	}
	attempt, err := state.BeginAttempt(report.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tree", "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation := prepareAttempt(t, state, attempt)
	recordWorkerCheckpoint(t, state, attempt, generation, 1, []string{"finish"})
	message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: "report:/tmp/decision.md"}, 2, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetVerifiedReportDelivery(report.ID, attempt.ID, message.ID, model.VerifiedArtifact{
		Kind: "report", OriginalRef: message.ArtifactRef, SHA256: strings.Repeat("a", 64), SizeBytes: 1, SnapshotPath: "/tmp/decision-snapshot.md",
	}, "verified"); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := state.ClaimRelease(attempt.ID)
	if err != nil || !claimed {
		t.Fatalf("release claim=%t err=%v", claimed, err)
	}
	if err := state.MarkReleased(attempt.ID); err != nil {
		t.Fatal(err)
	}
}

func annotationErrorKind(err error) string {
	type classified interface{ ErrorKind() string }
	var typed classified
	if errors.As(err, &typed) {
		return typed.ErrorKind()
	}
	return ""
}
