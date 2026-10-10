package execution

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

type livenessHost struct {
	probe string
	exit  ExitResponse
	calls []string
}

func (h *livenessHost) Name() string { return "workhorse" }

func (h *livenessHost) Call(_ context.Context, op string, _, resp any) error {
	h.calls = append(h.calls, op)
	switch op {
	case "probe":
		resp.(*StateResponse).State = h.probe
	case "exit_record":
		*resp.(*ExitResponse) = h.exit
	case "stop":
		resp.(*StateResponse).State = "exited"
	}
	return nil
}

func livenessFixture(t *testing.T, host *livenessHost, live bool, state string) (*store.Store, *Executor, *coord.Task, *coord.Run) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var task *coord.Task
	var run *coord.Run
	err = db.Write(ctx, func(tx *store.Tx) error {
		if _, err := coord.AddRepo(tx, "operator", coord.Repo{Name: "api", Host: "workhorse", Path: "/src/api", DefaultBranch: "main"}); err != nil {
			return err
		}
		proposal, err := coord.Propose(tx, &config.Config{Host: "workhorse", MaxDepth: 3}, coord.Caller{Kind: "driver", Name: "main"}, coord.NewTask{Repo: "api", Objective: "Work"})
		if err != nil {
			return err
		}
		task, err = coord.Create(tx, coord.Caller{Kind: "driver", Name: "main"}, proposal, config.Target{Host: "workhorse", Harness: "codex"})
		if err != nil {
			return err
		}
		attempt, err := coord.AllocateAttempt(tx, task, task.Target, func(int64) (string, string) { return t.TempDir(), "shephrd/t_1/1" }, 0)
		if err != nil {
			return err
		}
		if err := coord.SetWorkspaceState(tx, "system", attempt, "held", "base"); err != nil {
			return err
		}
		if err := coord.AdoptAttempt(tx, "system", task, attempt); err != nil {
			return err
		}
		run, _, err = coord.BeginRun(tx, "system", task, "start", func(int64) string { return t.TempDir() })
		if err != nil {
			return err
		}
		if live {
			if err := coord.MarkLive(tx, run, 123, "start", "", "codex", ""); err != nil {
				return err
			}
		}
		if state == "done" {
			if err := coord.SetState(tx, "system", task, "done", ""); err != nil {
				return err
			}
		} else if state == "closed" {
			if err := coord.SetState(tx, "system", task, "done", ""); err != nil {
				return err
			}
			if err := coord.SetState(tx, "system", task, "closed", "test"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return db, &Executor{DB: db, Host: func(string) (Host, error) { return host, nil }}, task, run
}

func TestProbeRecordsSupervisorExitBeforeIdentity(t *testing.T) {
	host := &livenessHost{exit: ExitResponse{Found: true, Record: ExitRecord{Status: 1}}}
	db, x, task, run := livenessFixture(t, host, false, "running")
	got, err := x.Probe(context.Background(), run)
	if err != nil || got != "exited" {
		t.Fatalf("probe: %s, %v", got, err)
	}
	current, err := coord.LoadRun(db, run.ID)
	if err != nil || current.Liveness != "exited" {
		t.Fatalf("run: %+v, %v", current, err)
	}
	currentTask, err := coord.Load(db, task.ID)
	if err != nil || currentTask.State != "running" {
		t.Fatalf("task: %+v, %v", currentTask, err)
	}
}

func TestProbeHoldsRunWhenSupervisorIdentityWasNeverRecorded(t *testing.T) {
	host := &livenessHost{}
	db, x, task, run := livenessFixture(t, host, false, "running")
	old := store.Timestamp(time.Now().Add(-3 * time.Minute))
	if _, err := db.Exec(`UPDATE runs SET started_at = ? WHERE id = ?`, old, run.ID); err != nil {
		t.Fatal(err)
	}
	run.Started = old
	got, err := x.Probe(context.Background(), run)
	if err != nil || got != "unknown" {
		t.Fatalf("probe: %s, %v", got, err)
	}
	current, err := coord.LoadRun(db, run.ID)
	if err != nil || current.Liveness != "unknown" {
		t.Fatalf("run: %+v, %v", current, err)
	}
	currentTask, err := coord.Load(db, task.ID)
	if err != nil || currentTask.State != "held" || currentTask.Reason != "start_failed" {
		t.Fatalf("task: %+v, %v", currentTask, err)
	}
}

func TestProbeHoldsWhenSupervisorIsGoneButGroupIsAlive(t *testing.T) {
	host := &livenessHost{probe: "unknown"}
	db, x, task, run := livenessFixture(t, host, true, "running")
	got, err := x.Probe(context.Background(), run)
	if err != nil || got != "unknown" {
		t.Fatalf("probe: %s, %v", got, err)
	}
	current, err := coord.LoadRun(db, run.ID)
	if err != nil || current.Liveness != "unknown" {
		t.Fatalf("run: %+v, %v", current, err)
	}
	currentTask, err := coord.Load(db, task.ID)
	if err != nil || currentTask.State != "held" || currentTask.Reason != "lost" {
		t.Fatalf("task: %+v, %v", currentTask, err)
	}
	items, err := coord.Items(db, `i.state = 'pending'`)
	if err != nil || len(items) != 1 {
		t.Fatalf("owner notification: %v, %v", items, err)
	}
	for _, call := range host.calls {
		if call == "stop" {
			t.Fatal("probed an unowned process group")
		}
	}
}

func TestFailedStopPreservesUnknownLivenessOnClosedStates(t *testing.T) {
	for _, state := range []string{"done", "closed"} {
		t.Run(state, func(t *testing.T) {
			host := &livenessHost{probe: "unknown"}
			db, x, task, run := livenessFixture(t, host, true, state)
			_, err := x.Stop(context.Background(), task, "stopped")
			if fault.As(err).Kind != "stop_unconfirmed" {
				t.Fatalf("stop: %v", err)
			}
			current, err := coord.LoadRun(db, run.ID)
			if err != nil || current.Liveness != "unknown" {
				t.Fatalf("run: %+v, %v", current, err)
			}
			currentTask, err := coord.Load(db, task.ID)
			if err != nil || currentTask.State != state {
				t.Fatalf("task: %+v, %v", currentTask, err)
			}
		})
	}
}

func TestFailedInterruptPreservesUnknownLivenessOnClosedStates(t *testing.T) {
	for _, state := range []string{"done", "closed"} {
		t.Run(state, func(t *testing.T) {
			host := &livenessHost{probe: "unknown"}
			db, x, task, run := livenessFixture(t, host, true, state)
			if err := x.Interrupt(context.Background(), run, "inactive"); err != nil {
				t.Fatalf("interrupt: %v", err)
			}
			current, err := coord.LoadRun(db, run.ID)
			if err != nil || current.Liveness != "unknown" {
				t.Fatalf("run: %+v, %v", current, err)
			}
			currentTask, err := coord.Load(db, task.ID)
			if err != nil || currentTask.State != state {
				t.Fatalf("task: %+v, %v", currentTask, err)
			}
		})
	}
}
