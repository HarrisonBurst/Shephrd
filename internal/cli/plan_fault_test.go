package cli

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestPlanDispatchSpawnStopsAfterDurableProjectionError(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	databasePath := filepath.Join(stateDir, "shephrd.db")
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("SHEPHRD_STATE_DIR", stateDir)
	t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("PI_SESSION_ID", "dispatch-refresh-error")
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: dir, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := state.CreatePlan("projection fault", "driver:pi:dispatch-refresh-error")
	if err != nil {
		t.Fatal(err)
	}
	item, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Dispatch target", FeatureKey: "projection-fault", Objective: "Dispatch", AcceptanceCriteria: "task persists", RepoID: repo.ID}, model.PlanItemRelations{Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	sentinel, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Projection sentinel", Objective: "Remain undispatched"}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", databasePath+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	// The baseline shape fence refuses unknown schema objects, so the fault
	// rides in the baseline dispatch-immutability trigger's own name: the
	// fence accepts the object set, and the redefined body breaks the
	// projection re-read after the durable dispatch write.
	if _, err := db.Exec(`DROP TRIGGER plan_items_dispatch_immutable`); err != nil {
		t.Fatal(err)
	}
	statement := fmt.Sprintf(`CREATE TRIGGER plan_items_dispatch_immutable AFTER UPDATE OF dispatched_task_id ON plan_items
		WHEN NEW.id='%s' AND NEW.dispatched_task_id IS NOT NULL
		BEGIN UPDATE plan_items SET position='broken' WHERE id='%s'; END`, item.ID, sentinel.ID)
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	root := New()
	root.SetArgs([]string{"plan", "dispatch", list.ID, item.ID, "--spawn", "--harness", "pi", "--runtime", "headless"})
	dispatchErr := root.Execute()
	if dispatchErr == nil {
		t.Fatal("dispatch projection fault unexpectedly succeeded")
	}
	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	tasks, err := state.Tasks(store.TaskFilter{})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("durable tasks = %+v, err = %v", tasks, err)
	}
	task := tasks[0]
	attempts, err := state.Attempts(task.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("spawn was attempted after projection failure: %+v, %v", attempts, err)
	}
	recovery := "shephrd task inspect " + task.ID
	if task.Status != model.TaskStatusQueued || !strings.Contains(dispatchErr.Error(), "dispatch succeeded") || !strings.Contains(dispatchErr.Error(), task.ID) || !strings.Contains(dispatchErr.Error(), recovery) {
		t.Fatalf("task=%+v error=%v", task, dispatchErr)
	}
	raw, err := sql.Open("sqlite", databasePath+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var dispatchedTaskID string
	if err := raw.QueryRow(`SELECT dispatched_task_id FROM plan_items WHERE id=?`, item.ID).Scan(&dispatchedTaskID); err != nil || dispatchedTaskID != task.ID {
		t.Fatalf("dispatched item task = %q, err = %v", dispatchedTaskID, err)
	}
}
