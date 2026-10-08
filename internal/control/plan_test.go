package control

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestPlanDispatchPostCommitErrorsNameDurableTaskAndRecovery(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*testing.T, *store.Store, string, model.Plan, model.PlanItem) model.Plan
		wrapped   bool
	}{
		{
			name: "projection refresh",
			configure: func(t *testing.T, state *store.Store, databasePath string, plan model.Plan, item model.PlanItem) model.Plan {
				sentinel, err := state.AddPlanItem(plan.ID, model.PlanItem{Title: "Projection sentinel", Objective: "Remain undispatched"}, model.PlanItemRelations{})
				if err != nil {
					t.Fatal(err)
				}
				execPlanFaultSQL(t, databasePath, fmt.Sprintf(`CREATE TRIGGER fault_plan_projection AFTER UPDATE OF dispatched_task_id ON plan_items
					WHEN NEW.id='%s' AND NEW.dispatched_task_id IS NOT NULL
					BEGIN UPDATE plan_items SET position='broken' WHERE id='%s'; END`, item.ID, sentinel.ID))
				return model.Plan{}
			},
			wrapped: true,
		},
		{
			name: "missing refreshed item",
			configure: func(t *testing.T, state *store.Store, databasePath string, plan model.Plan, item model.PlanItem) model.Plan {
				sink, err := state.CreatePlan("fault sink", plan.DriverID)
				if err != nil {
					t.Fatal(err)
				}
				execPlanFaultSQL(t, databasePath, fmt.Sprintf(`DROP TRIGGER plan_items_list_immutable;
					DROP TRIGGER plan_items_dispatched_scope_immutable;
					CREATE TRIGGER fault_plan_item_move AFTER UPDATE OF dispatched_task_id ON plan_items
					WHEN NEW.id='%s' AND NEW.dispatched_task_id IS NOT NULL
					BEGIN UPDATE plan_items SET plan_id='%s' WHERE id=NEW.id; END`, item.ID, sink.ID))
				return sink
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			databasePath := filepath.Join(t.TempDir(), "state.db")
			state, err := store.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := state.CreatePlan("fault source", "driver:test")
			if err != nil {
				t.Fatal(err)
			}
			item, err := state.AddPlanItem(plan.ID, model.PlanItem{Title: "Dispatch target", FeatureKey: "fault-" + strings.ReplaceAll(test.name, " ", "-"), Objective: "Dispatch", AcceptanceCriteria: "task persists", RepoID: repo.ID}, model.PlanItemRelations{Position: 1})
			if err != nil {
				t.Fatal(err)
			}
			sink := test.configure(t, state, databasePath, plan, item)
			service := New(config.Config{}, state)
			_, dispatchErr := service.DispatchPlanItem(plan.ID, item.ID, plan.DriverID)
			if dispatchErr == nil {
				t.Fatal("dispatch fault unexpectedly succeeded")
			}
			tasks, err := state.Tasks(store.TaskFilter{})
			if err != nil || len(tasks) != 1 {
				t.Fatalf("durable tasks = %+v, err = %v", tasks, err)
			}
			task := tasks[0]
			recovery := "shephrd task inspect " + task.ID
			if task.Status != model.TaskStatusQueued || !strings.Contains(dispatchErr.Error(), "dispatch succeeded") || !strings.Contains(dispatchErr.Error(), task.ID) || !strings.Contains(dispatchErr.Error(), recovery) {
				t.Fatalf("task=%+v error=%v", task, dispatchErr)
			}
			attempts, err := state.Attempts(task.ID)
			if err != nil || len(attempts) != 0 {
				t.Fatalf("post-commit projection error started work: %+v, %v", attempts, err)
			}
			if test.wrapped != (errors.Unwrap(dispatchErr) != nil) {
				t.Fatalf("wrapped cause = %v, error = %v", errors.Unwrap(dispatchErr), dispatchErr)
			}
			if sink.ID != "" {
				persisted, err := state.Plan(sink.ID, sink.DriverID)
				if err != nil || len(persisted.Items) != 1 || persisted.Items[0].DispatchedTaskID != task.ID {
					t.Fatalf("moved dispatched item = %+v, err = %v", persisted, err)
				}
			}
		})
	}
}

func execPlanFaultSQL(t *testing.T, databasePath, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", databasePath+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}
