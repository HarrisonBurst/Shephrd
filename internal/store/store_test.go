package store

import (
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestTaskAttemptLifecycleAndStaleEvents(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(t.TempDir(), "demo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "feature", Objective: "Build it", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt1, err := state.BeginAttempt(task.ID, "claude-code", "Luna/Sol")
	if err != nil {
		t.Fatal(err)
	}
	if attempt1.Model != "Luna/Sol" {
		t.Fatalf("model = %q", attempt1.Model)
	}
	if err := state.ConfigureAttempt(attempt1.ID, "session-1", "/tmp/tree-1", "lease-1", "shephrd/"+task.ID); err != nil {
		t.Fatal(err)
	}
	attempt1.RunGeneration = prepareAttempt(t, state, attempt1)
	if _, err := state.AddEventForRun(attempt1.ID, attempt1.RunGeneration, model.Event{Type: "progress", Payload: "working"}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	recordWorkerCheckpoint(t, state, attempt1, attempt1.RunGeneration, 2, []string{"get answer"})
	if _, err := state.AddEventForRun(attempt1.ID, attempt1.RunGeneration, model.Event{Type: "question", Payload: "Which option?"}, 3, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	waiting, _ := state.Task(task.ID)
	if waiting.Status != "waiting" {
		t.Fatalf("status = %s, want waiting", waiting.Status)
	}
	if _, err := state.AddEventForRun(attempt1.ID, attempt1.RunGeneration, model.Event{Type: "done", Payload: "done"}, 4, model.WorkspaceFacts{}); err == nil || !strings.Contains(err.Error(), "artifact") {
		t.Fatalf("missing artifact error = %v", err)
	}
	artifact := "report:/tmp/report.md"
	recordWorkerCheckpoint(t, state, attempt1, attempt1.RunGeneration, 4, []string{})
	if _, err := state.AddEventForRun(attempt1.ID, attempt1.RunGeneration, model.Event{Type: "done", Payload: "done", Artifact: artifact}, 5, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	late, err := state.AddEventForRun(attempt1.ID, attempt1.RunGeneration, model.Event{Type: "failed", Payload: "late"}, 6, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	if !late.Stale {
		t.Fatal("late event on terminal task was not marked stale")
	}
	if err := state.PrepareRetry(task.ID); err != nil {
		t.Fatal(err)
	}
	attempt2, err := state.BeginAttempt(task.ID, "pi", "Luna/Sol")
	if err != nil {
		t.Fatal(err)
	}
	if attempt2.Model != "Luna/Sol" {
		t.Fatalf("selected model = %q", attempt2.Model)
	}
	if err := state.ConfigureAttempt(attempt2.ID, "session-2", "/tmp/tree-2", "lease-2", "shephrd/"+task.ID+"-attempt-2"); err != nil {
		t.Fatal(err)
	}
	attempt2.RunGeneration = prepareAttempt(t, state, attempt2)
	stale, err := state.AddEventForRun(attempt1.ID, attempt1.RunGeneration, model.Event{Type: "question", Payload: "old question"}, 7, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Stale {
		t.Fatal("superseded attempt event was not marked stale")
	}
	current, _ := state.Task(task.ID)
	if current.CurrentAttemptID != attempt2.ID || current.Status != "working" {
		t.Fatalf("current task = %+v", current)
	}
}

func TestAcceptedTerminalEventCannotBeRegressedByRunnerDeath(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "terminal", Objective: "finish", Deliverable: "report"})
	attempt, _ := state.BeginAttempt(task.ID, "pi", "")
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tree", "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation := prepareAttempt(t, state, attempt)
	recordWorkerCheckpoint(t, state, attempt, generation, 1, []string{})
	artifact := "report:/tmp/report.md"
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: artifact}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	late, err := state.RecordRunnerDeathMessage(attempt.ID, generation, "late runner failure")
	if err != nil {
		t.Fatal(err)
	}
	current, _ := state.Task(task.ID)
	if !late.Stale || late.Wake || current.Status != "done" || !current.ClaimedDone || current.ArtifactRef != artifact {
		t.Fatalf("late=%+v task=%+v", late, current)
	}
}

func TestTaskAndPlanItemTitlesAreRequiredAtStorageBoundaries(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "titles", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.CreateTask(model.Task{DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "missing-title", Objective: "Do the work"}); err == nil || !strings.Contains(err.Error(), "title") {
		t.Fatalf("blank task title error = %v", err)
	}
	created, err := state.CreateTask(model.Task{Title: "  Human task title  ", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "titled", Objective: "Do all required work"})
	if err != nil || created.Title != "Human task title" {
		t.Fatalf("created task = %+v, err = %v", created, err)
	}
	list, err := state.CreatePlan("titles", "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddPlanItem(list.ID, model.PlanItem{Objective: "Plan the work"}, model.PlanItemRelations{}); err == nil || !strings.Contains(err.Error(), "title") {
		t.Fatalf("blank item title error = %v", err)
	}
	item, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "  Human item title  ", Objective: "Plan the complete work"}, model.PlanItemRelations{})
	if err != nil || item.Title != "Human item title" {
		t.Fatalf("created plan item = %+v, err = %v", item, err)
	}
	blank := " "
	if _, err := state.UpdatePlanItem(list.ID, item.ID, model.PlanItemUpdate{Title: &blank}); err == nil || !strings.Contains(err.Error(), "title") {
		t.Fatalf("blank item title update error = %v", err)
	}
}

func TestEmptyListsEncodeAsArrays(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	tasks, err := state.Tasks(TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if tasks == nil || len(tasks) != 0 {
		t.Fatalf("tasks = %#v", tasks)
	}
	repos, err := state.Repos()
	if err != nil {
		t.Fatal(err)
	}
	if repos == nil || len(repos) != 0 {
		t.Fatalf("repos = %#v", repos)
	}
}

func TestStatusTransitions(t *testing.T) {
	valid := [][2]string{{"queued", "starting"}, {"starting", "working"}, {"working", "waiting"}, {"waiting", "working"}, {"working", "done"}}
	for _, transition := range valid {
		if err := model.ValidateTransition(transition[0], transition[1], false); err != nil {
			t.Errorf("%s -> %s: %v", transition[0], transition[1], err)
		}
	}
	if err := model.ValidateTransition("done", "working", false); err == nil {
		t.Fatal("terminal transition accepted")
	}
	if err := model.ValidateTransition("done", "queued", true); err != nil {
		t.Fatal(err)
	}
}
