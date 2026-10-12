package daemon

import (
	"context"
	"fmt"
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
	calls        []string
	onStop       func() error
	noExitRecord bool
}

func (*reportedDriverHost) Name() string { return "workhorse" }

func (h *reportedDriverHost) Call(_ context.Context, op string, _, resp any) error {
	h.calls = append(h.calls, op)
	switch op {
	case "probe":
		resp.(*execution.StateResponse).State = "live"
	case "exit_record":
		if !h.noExitRecord {
			*resp.(*execution.ExitResponse) = execution.ExitResponse{Found: true, Record: execution.ExitRecord{Status: 0, Session: "session"}}
		}
	case "stop":
		if h.onStop != nil {
			if err := h.onStop(); err != nil {
				return err
			}
		}
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
	if len(host.calls) != 4 || host.calls[0] != "probe" || host.calls[1] != "exit_record" || host.calls[2] != "stop" || host.calls[3] != "exit_record" {
		t.Fatalf("host calls: %v", host.calls)
	}
}

func TestObserveReconcilesNoteReportedWhileStoppingResumedDriver(t *testing.T) {
	for _, reason := range []string{"no_report", "inactive"} {
		t.Run(reason, func(t *testing.T) {
			testObserveReconcilesNoteReportedWhileStoppingResumedDriver(t, reason)
		})
	}
}

func testObserveReconcilesNoteReportedWhileStoppingResumedDriver(t *testing.T, reason string) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var task *coord.Task
	var resumed *coord.Run
	var request int64
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
		if err := tx.QueryRow(`SELECT seq FROM requests WHERE task = ?`, task.ID).Scan(&request); err != nil {
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
		first, _, err := coord.BeginRun(tx, "system", task, "start", func(int64) string { return t.TempDir() })
		if err != nil {
			return err
		}
		caller := coord.Caller{Kind: "run", Task: task.ID, Attempt: 1, Run: first.ID}
		child, err := coord.Propose(tx, &config.Config{Host: "workhorse", MaxDepth: 3}, caller, coord.NewTask{Repo: "api", Objective: "Build"})
		if err != nil {
			return err
		}
		worker, err := coord.Create(tx, caller, child, task.Target)
		if err != nil {
			return err
		}
		if err := coord.RecordExit(tx, first, 0, "native-session"); err != nil {
			return err
		}
		workerAttempt, err := coord.AllocateAttempt(tx, worker, worker.Target, func(int64) (string, string) { return t.TempDir(), "shephrd/t_2/1" }, 0)
		if err != nil {
			return err
		}
		if err := coord.AdoptAttempt(tx, "system", worker, workerAttempt); err != nil {
			return err
		}
		childRun, _, err := coord.BeginRun(tx, "system", worker, "start", func(int64) string { return t.TempDir() })
		if err != nil {
			return err
		}
		if _, err := coord.RecordReport(tx, coord.Caller{Kind: "run", Task: worker.ID, Attempt: 1, Run: childRun.ID}, worker, &coord.Report{Kind: "result", Body: "Built"}); err != nil {
			return err
		}
		if err := coord.RecordExit(tx, childRun, 0, "child-session"); err != nil {
			return err
		}
		resumed, _, err = coord.BeginRun(tx, "system", task, "nudge", func(int64) string { return t.TempDir() })
		if err != nil {
			return err
		}
		if err := coord.MarkLive(tx, resumed, 123, "start", "", "codex", ""); err != nil {
			return err
		}
		if reason == "inactive" {
			_, err = tx.Exec(`UPDATE runs SET last_activity = ? WHERE id = ?`, store.Timestamp(time.Now().Add(-2*time.Hour)), resumed.ID)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	host := &reportedDriverHost{noExitRecord: reason == "inactive", onStop: func() error {
		var chosen string
		if err := db.QueryRow(`SELECT stop_reason FROM runs WHERE id = ?`, resumed.ID).Scan(&chosen); err != nil {
			return err
		}
		if chosen != reason {
			return fmt.Errorf("interruption reason: %s, want %s", chosen, reason)
		}
		done := make(chan error, 1)
		go func() {
			done <- db.Write(ctx, func(tx *store.Tx) error {
				caller := coord.Caller{Kind: "run", Task: task.ID, Attempt: 1, Run: resumed.ID}
				if err := coord.RequireCurrent(tx, caller); err != nil {
					return err
				}
				current, err := coord.ReportTarget(tx, caller, &coord.Report{Kind: "result", Body: "Child is done", Request: request})
				if err != nil {
					return err
				}
				if _, err := coord.RecordReport(tx, caller, current, &coord.Report{Kind: "result", Body: "Child is done", Request: request}); err != nil {
					return err
				}
				_, err = coord.RecordReport(tx, caller, current, &coord.Report{Kind: "note", Body: "Request complete"})
				return err
			})
		}()
		return <-done
	}}
	x := &execution.Executor{DB: db, Host: func(string) (execution.Host, error) { return host, nil }}
	d := &Daemon{db: db, Log: io.Discard}
	cfg := &config.Config{Host: "workhorse", Timeouts: config.Timeouts{Inactivity: config.Duration{Duration: time.Hour}, DriverInactivity: config.Duration{Duration: time.Hour}}}
	if err := d.observe(ctx, x, cfg); err != nil {
		t.Fatal(err)
	}
	current, err := coord.LoadRun(db, resumed.ID)
	if err != nil || current.Liveness != "exited" || !current.TurnReported || current.ExitStatus == nil || *current.ExitStatus != -1 || current.StopReason != "turn_ended" {
		t.Fatalf("resumed run: %+v, %v", current, err)
	}
	var reported bool
	if err := db.QueryRow(`SELECT json_extract(data, '$.reported') FROM events WHERE run = ? AND name = 'run.exited'`, resumed.ID).Scan(&reported); err != nil || !reported {
		t.Fatalf("exit reported: %v, %v", reported, err)
	}
	currentTask, err := coord.Load(db, task.ID)
	if err != nil || currentTask.State != "done" || currentTask.Reason != "" {
		t.Fatalf("driver after note: %+v, %v", currentTask, err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM requests WHERE task = ? AND seq = ?`, task.ID, request).Scan(&state); err != nil || state != "answered" {
		t.Fatalf("request state: %s, %v", state, err)
	}
}
