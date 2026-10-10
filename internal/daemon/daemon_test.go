package daemon

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/execution"
	"shephrd/internal/store"
)

type reportedDriverHost struct {
	calls []string
}

func (*reportedDriverHost) Name() string { return "workhorse" }

func (h *reportedDriverHost) Call(_ context.Context, op string, _, resp any) error {
	h.calls = append(h.calls, op)
	switch op {
	case "probe":
		resp.(*execution.StateResponse).State = "live"
	case "exit_record":
		*resp.(*execution.ExitResponse) = execution.ExitResponse{Found: true, Record: execution.ExitRecord{Status: 0, Session: "session"}}
	case "stop":
		resp.(*execution.StateResponse).State = "exited"
	}
	return nil
}

func TestObserveUsesExitRecordForReportedDriver(t *testing.T) {
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
		proposal, err := coord.Propose(tx, &config.Config{Host: "workhorse", MaxDepth: 3}, coord.Caller{Kind: "driver", Name: "main"}, coord.NewTask{Role: "driver", Repo: "api", Objective: "Work"})
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
		if err := coord.MarkLive(tx, run, 123, "start", "", "codex", ""); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE runs SET turn_reported = 1 WHERE id = ?`, run.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	run.TurnReported = true
	observedRuns, err := coord.Runs(db, `liveness IN ('starting', 'live')`)
	if err != nil || len(observedRuns) != 1 || !observedRuns[0].TurnReported {
		t.Fatalf("stored run: %+v, %v", observedRuns, err)
	}
	host := &reportedDriverHost{}
	x := &execution.Executor{DB: db, Host: func(string) (execution.Host, error) { return host, nil }}
	d := &Daemon{db: db, Log: io.Discard}
	cfg := &config.Config{Host: "workhorse", Timeouts: config.Timeouts{Inactivity: config.Duration{Duration: time.Hour}, DriverInactivity: config.Duration{Duration: time.Hour}}}
	if err := d.observe(ctx, x, cfg); err != nil {
		t.Fatal(err)
	}
	current, err := coord.LoadRun(db, run.ID)
	if err != nil || current.Liveness != "exited" || current.StopReason != "turn_ended" {
		t.Fatalf("run: %+v, %v", current, err)
	}
	if len(host.calls) < 5 || host.calls[0] != "probe" || host.calls[1] != "exit_record" {
		t.Fatalf("host calls: %v", host.calls)
	}
}
