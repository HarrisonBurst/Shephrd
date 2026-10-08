package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"shephrd/internal/model"
)

func TestPlanReadinessEditingPrerequisitesInputsDispatchAndStaleness(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: "/demo", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := state.CreatePlan("release", "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "Implement"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if implementation.Readiness.Ready || reasonCodes(implementation.Readiness) != "repo_required,feature_required,acceptance_required" {
		t.Fatalf("initial readiness = %+v", implementation.Readiness)
	}
	feature, acceptance, description, deliverable, repoID := "implement", "checks pass", "Use reports", "code", repo.ID
	implementation, err = state.UpdatePlanItem(list.ID, implementation.ID, model.PlanItemUpdate{FeatureKey: &feature, Description: &description,
		AcceptanceCriteria: &acceptance, RepoID: &repoID, Deliverable: &deliverable})
	if err != nil {
		t.Fatal(err)
	}
	first, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", FeatureKey: "first", Objective: "First report", AcceptanceCriteria: "report complete", RepoID: repo.ID, Deliverable: "report"}, model.PlanItemRelations{Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", FeatureKey: "second", Objective: "Second report", AcceptanceCriteria: "report complete", RepoID: repo.ID, Deliverable: "report"}, model.PlanItemRelations{Position: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.AddPlanPrerequisite(list.ID, implementation.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := state.AddPlanPrerequisite(list.ID, implementation.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := state.AddPlanPrerequisite(list.ID, first.ID, implementation.ID); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
	otherList, err := state.CreatePlan("other", "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	otherItem, err := state.AddPlanItem(otherList.ID, model.PlanItem{Title: "Test item", Objective: "Other"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.AddPlanPrerequisite(list.ID, implementation.ID, otherItem.ID); err == nil || !strings.Contains(err.Error(), "same plan") {
		t.Fatalf("cross-plan prerequisite error = %v", err)
	}
	if _, err := state.AddPlanReport(list.ID, implementation.ID, otherItem.ID); err == nil || !strings.Contains(err.Error(), "requires a prerequisite relation first") {
		t.Fatalf("input prerequisite invariant error = %v", err)
	}
	if _, err := state.AddPlanReport(list.ID, implementation.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddPlanReport(list.ID, implementation.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	firstTask, err := state.DispatchPlanItem(list.ID, first.ID, "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	secondTask, err := state.DispatchPlanItem(list.ID, second.ID, "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	firstArtifact := completePlanReport(t, state, firstTask, "a")
	secondArtifact := completePlanReport(t, state, secondTask, "b")
	if err := state.MovePlanReport(list.ID, implementation.ID, second.ID, 1); err != nil {
		t.Fatal(err)
	}
	ready, err := state.PlanItem(list.ID, implementation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Readiness.Ready || len(ready.Reports) != 2 || ready.Reports[0].ArtifactID != "" || !hasReason(ready.Readiness, "report_not_selected") || len(ready.Readiness.AvailableEvidence) != 2 || ready.Readiness.AvailableEvidence[0].VerifiedReport == nil {
		t.Fatalf("unselected but dispatchable item = %+v", ready)
	}
	dispatched, err := state.DispatchPlanItem(list.ID, implementation.ID, "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	if dispatched.Status != "queued" || dispatched.CurrentAttemptID != "" || dispatched.Title != ready.Title {
		t.Fatalf("dispatch changed title or started work: %+v", dispatched)
	}
	selected, err := state.PlanItem(list.ID, implementation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Reports) != 2 || selected.Reports[0].ArtifactID != secondArtifact.ID || selected.Reports[1].ArtifactID != firstArtifact.ID || selected.Reports[0].SelectedByDriverID != "driver:test" || selected.Reports[0].SelectedAt == nil {
		t.Fatalf("automatic selections = %+v", selected.Reports)
	}
	inputs, err := state.ReportInputs(dispatched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0].ArtifactID != secondArtifact.ID || inputs[1].ArtifactID != firstArtifact.ID || inputs[0].ProducerAttemptID != secondArtifact.ProducerAttemptID || inputs[1].DoneMessageID != firstArtifact.DoneMessageID {
		t.Fatalf("dispatched inputs = %+v", inputs)
	}
	for _, statement := range []string{
		`UPDATE plan_items SET title='changed' WHERE id='` + implementation.ID + `'`,
		`UPDATE plan_items SET objective='changed' WHERE id='` + implementation.ID + `'`,
		`UPDATE plan_items SET dispatched_task_id=NULL WHERE id='` + implementation.ID + `'`,
		`UPDATE plan_prerequisites SET prerequisite_item_id=prerequisite_item_id WHERE item_id='` + implementation.ID + `'`,
	} {
		if _, err := state.db.Exec(statement); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("invariant statement %q error = %v", statement, err)
		}
	}

	future, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", FeatureKey: "future", Objective: "Future", AcceptanceCriteria: "done", RepoID: repo.ID}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.AddPlanPrerequisite(list.ID, future.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddPlanReport(list.ID, future.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SelectPlanReport(list.ID, future.ID, first.ID, firstArtifact.ID, "driver:test"); err != nil {
		t.Fatal(err)
	}
	if err := state.PrepareRetry(firstTask.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := state.PlanItem(list.ID, future.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Reports[0].Stale || !hasReason(stale.Readiness, "report_selection_stale") || stale.Readiness.Ready {
		t.Fatalf("stale readiness = %+v", stale)
	}
	if _, err := state.DispatchPlanItem(list.ID, future.ID, "driver:test"); err == nil {
		t.Fatal("stale input dispatched")
	}
}

func TestOrderedPositionMutationsCoverItemsAndInputs(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	list, err := state.CreatePlan("ordered", "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	first, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "First"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "Second"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	third, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "Third"}, model.PlanItemRelations{Position: 2})
	if err != nil {
		t.Fatal(err)
	}
	position := 1
	if _, err := state.UpdatePlanItem(list.ID, second.ID, model.PlanItemUpdate{Position: &position}); err != nil {
		t.Fatal(err)
	}
	if err := state.DeletePlanItem(list.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	persisted, err := state.Plan(list.ID, "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Items) != 2 || persisted.Items[0].ID != second.ID || persisted.Items[0].Position != 1 || persisted.Items[1].ID != third.ID || persisted.Items[1].Position != 2 {
		t.Fatalf("item order = %+v", persisted.Items)
	}

	successor, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", Objective: "Successor"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	for _, prerequisite := range []model.PlanItem{second, third} {
		if err := state.AddPlanPrerequisite(list.ID, successor.ID, prerequisite.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := state.AddPlanReport(list.ID, successor.ID, prerequisite.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.MovePlanReport(list.ID, successor.ID, third.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := state.RemovePlanReport(list.ID, successor.ID, third.ID); err != nil {
		t.Fatal(err)
	}
	successor, err = state.PlanItem(list.ID, successor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(successor.Reports) != 1 || successor.Reports[0].PrerequisiteItemID != second.ID || successor.Reports[0].Position != 1 {
		t.Fatalf("input order = %+v", successor.Reports)
	}
}

func TestPlanDispatchRejectsChangedAutomaticReportIdentity(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: "/demo", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := state.CreatePlan("identity", "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	producerItem, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Produce report", FeatureKey: "producer", Objective: "Produce", AcceptanceCriteria: "done", RepoID: repo.ID, Deliverable: "report"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	successor, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Use report", FeatureKey: "successor", Objective: "Use", AcceptanceCriteria: "done", RepoID: repo.ID}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.AddPlanPrerequisite(list.ID, successor.ID, producerItem.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddPlanReport(list.ID, successor.ID, producerItem.ID); err != nil {
		t.Fatal(err)
	}
	producer, err := state.DispatchPlanItem(list.ID, producerItem.ID, "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	firstArtifact := completePlanReport(t, state, producer, "a")
	selection := []model.PlanReportSelection{{PrerequisiteItemID: producerItem.ID, ProducerTaskID: producer.ID, ArtifactID: firstArtifact.ID}}
	if err := state.PrepareRetry(producer.ID); err != nil {
		t.Fatal(err)
	}
	secondArtifact := completePlanReport(t, state, producer, "b")
	if secondArtifact.ID == firstArtifact.ID {
		t.Fatal("report identity did not change")
	}
	if _, err := state.DispatchPlanItemWithSelections(list.ID, successor.ID, "driver:test", selection); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("changed report identity error = %v", err)
	}
	persisted, err := state.PlanItem(list.ID, successor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.DispatchedTaskID != "" || persisted.Reports[0].ArtifactID != "" {
		t.Fatalf("changed identity partially committed: %+v", persisted)
	}
}

func TestPlanDispatchIsAtomicAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	repo, err := first.UpsertRepo(model.Repo{Name: "demo", Path: "/demo", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := first.CreatePlan("atomic", "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	producerItem, err := first.AddPlanItem(list.ID, model.PlanItem{Title: "Atomic report", FeatureKey: "atomic-report", Objective: "Report", AcceptanceCriteria: "done", RepoID: repo.ID, Deliverable: "report"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	producer, err := first.DispatchPlanItem(list.ID, producerItem.ID, "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	artifact := completePlanReport(t, first, producer, "c")
	item, err := first.AddPlanItem(list.ID, model.PlanItem{Title: "Test item", FeatureKey: "atomic", Objective: "Atomic", AcceptanceCriteria: "done", RepoID: repo.ID}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AddPlanPrerequisite(list.ID, item.ID, producerItem.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := first.AddPlanReport(list.ID, item.ID, producerItem.ID); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	stores := []*Store{first, second}
	results := make(chan error, len(stores))
	var wait sync.WaitGroup
	for _, state := range stores {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := state.DispatchPlanItem(list.ID, item.ID, "driver:test")
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful dispatches = %d", successes)
	}
	tasks, err := first.Tasks(TaskFilter{})
	if err != nil || len(tasks) != 2 {
		t.Fatalf("tasks = %+v, err = %v", tasks, err)
	}
	var target model.Task
	for _, task := range tasks {
		if task.FeatureKey == "atomic" {
			target = task
		}
	}
	inputs, err := first.ReportInputs(target.ID)
	if target.Status != "queued" || err != nil || len(inputs) != 1 || inputs[0].ArtifactID != artifact.ID {
		t.Fatalf("atomic target=%+v inputs=%+v err=%v", target, inputs, err)
	}
}

func TestPlanSummariesDiscloseOwnershipAndDispatchedEvidenceWithoutMutation(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: "/demo", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := state.CreatePlan("owned plan", "driver:current")
	if err != nil {
		t.Fatal(err)
	}
	item, err := state.AddPlanItem(owned.ID, model.PlanItem{Title: "Queued evidence", FeatureKey: "queued", Objective: "Queue", AcceptanceCriteria: "done", RepoID: repo.ID}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := state.DispatchPlanItem(owned.ID, item.ID, "driver:current")
	if err != nil {
		t.Fatal(err)
	}
	unowned, err := state.CreatePlan("other plan", "driver:other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddPlanItem(unowned.ID, model.PlanItem{Title: "Future work", Objective: "Plan"}, model.PlanItemRelations{}); err != nil {
		t.Fatal(err)
	}
	summaries, err := state.PlanSummaries("driver:current", true)
	if err != nil {
		t.Fatal(err)
	}
	byList := make(map[string]model.PlanSummary, len(summaries))
	for _, summary := range summaries {
		byList[summary.ID] = summary
	}
	ownedSummary, unownedSummary := byList[owned.ID], byList[unowned.ID]
	if len(summaries) != 2 || ownedSummary.Ownership != "owned" || ownedSummary.DispatchedTaskCount != 1 || ownedSummary.LiveTaskCount != 1 || len(ownedSummary.DispatchedTasks) != 1 || ownedSummary.DispatchedTasks[0].ID != dispatched.ID {
		t.Fatalf("owned summary = %+v", summaries)
	}
	if unownedSummary.Ownership != "unowned" || unownedSummary.UndispatchedItemCount != 1 {
		t.Fatalf("unowned summary = %+v", unownedSummary)
	}
	persisted, err := state.Plan(unowned.ID, "driver:other")
	if err != nil || persisted.DriverID != "driver:other" {
		t.Fatalf("cross-driver summary mutated ownership: %+v, %v", persisted, err)
	}
}

func TestPlanOwnershipPersistsAndCanBeAdopted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	list, err := state.CreatePlan("next", "driver:old")
	if err != nil {
		t.Fatal(err)
	}
	state.Close()
	state, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if _, err := state.Plan(list.ID, "driver:old"); err != nil {
		t.Fatal(err)
	}
	adopted, err := state.AdoptPlan(list.ID, list.DriverID, "driver:new")
	if err != nil {
		t.Fatal(err)
	}
	if adopted.DriverID != "driver:new" {
		t.Fatalf("adopted = %+v", adopted)
	}
	if _, err := state.Plan(list.ID, "driver:old"); err == nil {
		t.Fatal("old driver retained task list")
	}
}

func completePlanReport(t *testing.T, state *Store, task model.Task, digest string) model.VerifiedArtifact {
	t.Helper()
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tree", "lease-"+digest, "branch-"+digest); err != nil {
		t.Fatal(err)
	}
	generation := prepareAttempt(t, state, attempt)
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "done", NextSteps: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "done", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	artifactRef := "report:/data/" + task.ID + "/report.md"
	message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: artifactRef}, 2, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := state.SetVerifiedReportDelivery(task.ID, attempt.ID, message.ID, model.VerifiedArtifact{Kind: "report", OriginalRef: artifactRef,
		SHA256: strings.Repeat(digest, 64), SizeBytes: 1, SnapshotPath: "/data/artifacts/" + digest}, "verified")
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func reasonCodes(readiness model.PlanReadiness) string {
	values := make([]string, len(readiness.Reasons))
	for index, reason := range readiness.Reasons {
		values[index] = reason.Code
	}
	return strings.Join(values, ",")
}

func hasReason(readiness model.PlanReadiness, code string) bool {
	for _, reason := range readiness.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func TestPlanContinuationBudgetBoundsReportInputs(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: "/demo", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := state.CreatePlan("budget", "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	producers := make([]model.PlanItem, 0, model.MaxTaskReportInputs+1)
	for index := 1; index <= model.MaxTaskReportInputs+1; index++ {
		producer, err := state.AddPlanItem(plan.ID, model.PlanItem{Title: fmt.Sprintf("Producer %d", index), FeatureKey: fmt.Sprintf("producer-%d", index), Objective: "Report", AcceptanceCriteria: "report complete", RepoID: repo.ID, Deliverable: "report"}, model.PlanItemRelations{})
		if err != nil {
			t.Fatal(err)
		}
		producers = append(producers, producer)
	}
	atBudget := model.PlanItemRelations{}
	for _, producer := range producers[:model.MaxTaskReportInputs] {
		atBudget.Requires = append(atBudget.Requires, producer.ID)
		atBudget.Reports = append(atBudget.Reports, producer.ID)
	}
	overBudget := model.PlanItemRelations{Requires: append(append([]string{}, atBudget.Requires...), producers[model.MaxTaskReportInputs].ID), Reports: append(append([]string{}, atBudget.Reports...), producers[model.MaxTaskReportInputs].ID)}
	if _, err := state.AddPlanItem(plan.ID, model.PlanItem{Title: "Over budget", Objective: "Over budget"}, overBudget); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("at most %d relations", model.MaxTaskReportInputs)) {
		t.Fatalf("over-budget creation error = %v", err)
	}
	successor, err := state.AddPlanItem(plan.ID, model.PlanItem{Title: "At budget", Objective: "At budget"}, atBudget)
	if err != nil {
		t.Fatal(err)
	}
	if len(successor.Reports) != model.MaxTaskReportInputs || len(successor.Prerequisites) != model.MaxTaskReportInputs {
		t.Fatalf("budget successor = %+v", successor)
	}
	if err := state.AddPlanPrerequisite(plan.ID, successor.ID, producers[model.MaxTaskReportInputs].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddPlanReport(plan.ID, successor.ID, producers[model.MaxTaskReportInputs].ID); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("at most %d report inputs", model.MaxTaskReportInputs)) {
		t.Fatalf("over-budget report add error = %v", err)
	}
}
