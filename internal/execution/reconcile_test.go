package execution

import (
	"context"
	"path/filepath"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/store"
)

type completingHost struct {
	complete func() error
}

func (h completingHost) Name() string { return "workhorse" }

func (h completingHost) Call(_ context.Context, op string, _, resp any) error {
	if op == "classify" {
		if err := h.complete(); err != nil {
			return err
		}
		resp.(*StateResponse).State = "held"
	}
	return nil
}

func TestReconcileDoesNotOverwriteConcurrentWorkspacePreparation(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var attempt *coord.Attempt
	err = db.Write(ctx, func(tx *store.Tx) error {
		if _, err := coord.AddRepo(tx, "operator", coord.Repo{Name: "api", Host: "workhorse", Path: "/src/api", DefaultBranch: "main"}); err != nil {
			return err
		}
		proposal, err := coord.Propose(tx, &config.Config{Host: "workhorse", MaxDepth: 3}, coord.Caller{Kind: "driver", Name: "main"}, coord.NewTask{Repo: "api", Objective: "Work"})
		if err != nil {
			return err
		}
		task, err := coord.Create(tx, coord.Caller{Kind: "driver", Name: "main"}, proposal, config.Target{Host: "workhorse", Harness: "codex"})
		if err != nil {
			return err
		}
		attempt, err = coord.AllocateAttempt(tx, task, task.Target, func(int64) (string, string) { return t.TempDir(), "shephrd/t_1/1" }, 0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	host := completingHost{complete: func() error {
		return db.Write(ctx, func(tx *store.Tx) error {
			prepared, err := coord.LoadAttempt(tx, attempt.Task, attempt.N)
			if err != nil {
				return err
			}
			return coord.SetWorkspaceState(tx, "system", prepared, "held", "prepared-base")
		})
	}}
	x := &Executor{DB: db, Host: func(string) (Host, error) { return host, nil }}
	out, err := x.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Workspaces) != 0 {
		t.Fatalf("stale classification applied: %v", out.Workspaces)
	}
	prepared, err := coord.LoadAttempt(db, attempt.Task, attempt.N)
	if err != nil || prepared.WorkspaceState != "held" || prepared.Base != "prepared-base" {
		t.Fatalf("prepared workspace overwritten: %+v, %v", prepared, err)
	}
}
