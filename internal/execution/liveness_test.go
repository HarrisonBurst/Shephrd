package execution

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

type livenessHost struct {
	probeErr     bool
	probe        string
	presentation string
	exit         ExitResponse
	exitErr      bool
	onExit       func()
	calls        []string
}

func (h *livenessHost) Name() string { return "workhorse" }

func (h *livenessHost) Call(_ context.Context, op string, _, resp any) error {
	h.calls = append(h.calls, op)
	switch op {
	case "probe":
		if h.probeErr {
			return errors.New("probe failed")
		}
		resp.(*StateResponse).State = h.probe
	case "exit_record":
		if h.exitErr {
			return errors.New("host unreachable")
		}
		if h.onExit != nil {
			h.onExit()
		}
		*resp.(*ExitResponse) = h.exit
	case "presentation":
		resp.(*PresentationResponse).State = h.presentation
	case "stop":
		if h.probe == "live" || h.probe == "exited" {
			resp.(*StateResponse).State = "exited"
		}
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
			items, err := coord.Items(db, `i.state = 'pending'`)
			if err != nil || len(items) != 1 {
				t.Fatalf("owner notification: %v, %v", items, err)
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

func TestStopTreatsProvenExitedAsStopped(t *testing.T) {
	host := &livenessHost{probe: "exited"}
	db, x, task, run := livenessFixture(t, host, true, "running")
	if _, err := x.Stop(context.Background(), task, "stopped"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	current, err := coord.LoadRun(db, run.ID)
	if err != nil || current.Liveness != "exited" {
		t.Fatalf("run: %+v, %v", current, err)
	}
}

func TestInterruptTreatsProvenExitedAsStopped(t *testing.T) {
	host := &livenessHost{probe: "exited"}
	db, x, _, run := livenessFixture(t, host, true, "running")
	if _, err := db.Exec(`UPDATE runs SET turn_reported = 1 WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	run.TurnReported = true
	if err := x.Interrupt(context.Background(), run, "turn_ended"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	current, err := coord.LoadRun(db, run.ID)
	if err != nil || current.Liveness != "exited" {
		t.Fatalf("run: %+v, %v", current, err)
	}
}

func TestStalePIDZeroExitRecordDoesNotOverrideRecordedIdentity(t *testing.T) {
	host := &livenessHost{probe: "live", exit: ExitResponse{Found: true, Record: ExitRecord{Status: 1}}}
	db, x, _, run := livenessFixture(t, host, false, "running")
	if _, err := db.Exec(`UPDATE runs SET pid = ?, start_time = ?, liveness = 'live' WHERE id = ?`, 123, "start", run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := x.Probe(context.Background(), run)
	if err != nil || got != "live" {
		t.Fatalf("probe: %s, %v", got, err)
	}
	current, err := coord.LoadRun(db, run.ID)
	if err != nil || current.Liveness != "live" || current.PID != 123 {
		t.Fatalf("run: %+v, %v", current, err)
	}
}

func TestAbandonUnstartedPresentationRun(t *testing.T) {
	host := &livenessHost{presentation: "absent", exit: ExitResponse{Present: true}}
	db, x, task, run := livenessFixture(t, host, false, "running")
	if _, err := db.Exec(`UPDATE runs SET liveness = 'unknown', endpoint = ? WHERE id = ?`, "ep", run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET state = 'held', reason = 'start_failed' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	run.Liveness, run.Endpoint = "unknown", "ep"
	if _, err := x.Abandon(context.Background(), task); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	current, err := coord.LoadRun(db, run.ID)
	if err != nil || current.Liveness != "exited" || current.StopReason != "abandoned" {
		t.Fatalf("run: %+v, %v", current, err)
	}
	currentTask, err := coord.Load(db, task.ID)
	if err != nil || currentTask.State != "held" {
		t.Fatalf("task: %+v, %v", currentTask, err)
	}
}

func TestAbandonRefusesUnprovenPresentationStarts(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, onHost, state string
		pid, found, present           bool
	}{
		{name: "no endpoint", state: "absent", present: true},
		{name: "endpoint mismatch", recorded: "ep1", onHost: "ep2", state: "absent", present: true},
		{name: "endpoint present", onHost: "ep", state: "present", present: true},
		{name: "endpoint uncertain", onHost: "ep", state: "uncertain", present: true},
		{name: "exit record", onHost: "ep", state: "absent", found: true, present: true},
		{name: "run directory missing", onHost: "ep", state: "absent"},
		{name: "process identity recorded", onHost: "ep", state: "absent", pid: true, present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &livenessHost{presentation: tc.state, exit: ExitResponse{Endpoint: tc.onHost, Found: tc.found, Present: tc.present}}
			db, x, task, run := livenessFixture(t, host, tc.pid, "running")
			if _, err := db.Exec(`UPDATE runs SET liveness = 'unknown', endpoint = ? WHERE id = ?`, tc.recorded, run.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE tasks SET state = 'held', reason = 'start_failed' WHERE id = ?`, task.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := x.Abandon(context.Background(), task); fault.As(err).Kind != "abandon_unconfirmed" {
				t.Fatalf("abandon: %v", err)
			}
			current, err := coord.LoadRun(db, run.ID)
			if err != nil || current.Liveness != "unknown" || current.ExitStatus != nil || current.StopReason != "" {
				t.Fatalf("run after refusal: %+v, %v", current, err)
			}
		})
	}
}

func TestAbandonWithHostNoDispatchProof(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		proof    bool
		change   func(*NoDispatchProof)
		mutate   func(*testing.T, *store.Store, *coord.Task, *coord.Run)
		found    bool
		present  bool
		pid      bool
		live     bool
		unheld   bool
		running  bool
		hostErr  bool
		want     string
	}{
		{name: "matching proof", proof: true, present: true},
		{name: "missing proof", present: true, want: "abandon_unconfirmed"},
		{name: "wrong task", proof: true, present: true, change: func(p *NoDispatchProof) { p.Task++ }, want: "abandon_unconfirmed"},
		{name: "stale attempt", proof: true, present: true, change: func(p *NoDispatchProof) { p.Attempt++ }, want: "abandon_unconfirmed"},
		{name: "stale run", proof: true, present: true, change: func(p *NoDispatchProof) { p.Run++ }, want: "abandon_unconfirmed"},
		{name: "stale generation", proof: true, present: true, change: func(p *NoDispatchProof) { p.Generation++ }, want: "abandon_unconfirmed"},
		{name: "wrong host", proof: true, present: true, change: func(p *NoDispatchProof) { p.Host = "other" }, want: "abandon_unconfirmed"},
		{name: "wrong directory", proof: true, present: true, change: func(p *NoDispatchProof) { p.Dir = "other" }, want: "abandon_unconfirmed"},
		{name: "endpoint cannot be bypassed", proof: true, present: true, endpoint: "ep", want: "abandon_unconfirmed"},
		{name: "exit record", proof: true, present: true, found: true, want: "abandon_unconfirmed"},
		{name: "run directory missing", proof: true, want: "abandon_unconfirmed"},
		{name: "host unreachable", proof: true, present: true, hostErr: true, want: "host error"},
		{name: "process identity", proof: true, present: true, pid: true, want: "abandon_unconfirmed"},
		{name: "run still starting", proof: true, present: true, live: true, want: "abandon_unconfirmed"},
		{name: "workspace not held", proof: true, present: true, unheld: true, want: "abandon_unconfirmed"},
		{name: "task not held", proof: true, present: true, running: true, want: "abandon_unconfirmed"},
		{name: "concurrent current run cleared", proof: true, present: true, mutate: func(t *testing.T, db *store.Store, task *coord.Task, _ *coord.Run) {
			if _, err := db.Exec(`UPDATE tasks SET run = NULL WHERE id = ?`, task.ID); err != nil {
				t.Fatal(err)
			}
		}, want: "abandon_stale"},
		{name: "concurrent task change", proof: true, present: true, mutate: func(t *testing.T, db *store.Store, task *coord.Task, _ *coord.Run) {
			if _, err := db.Exec(`UPDATE tasks SET state = 'running' WHERE id = ?`, task.ID); err != nil {
				t.Fatal(err)
			}
		}, want: "abandon_stale"},
		{name: "concurrent run change", proof: true, present: true, mutate: func(t *testing.T, db *store.Store, _ *coord.Task, r *coord.Run) {
			if _, err := db.Exec(`UPDATE runs SET endpoint = 'new' WHERE id = ?`, r.ID); err != nil {
				t.Fatal(err)
			}
		}, want: "abandon_stale"},
		{name: "concurrent workspace change", proof: true, present: true, mutate: func(t *testing.T, db *store.Store, task *coord.Task, _ *coord.Run) {
			if _, err := db.Exec(`UPDATE attempts SET workspace_state = 'unknown' WHERE task = ? AND n = ?`, task.ID, task.Attempt); err != nil {
				t.Fatal(err)
			}
		}, want: "abandon_stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &livenessHost{exit: ExitResponse{Present: tc.present, Found: tc.found, Endpoint: tc.endpoint}, presentation: "present", exitErr: tc.hostErr}
			db, x, task, run := livenessFixture(t, host, tc.pid, "running")
			if !tc.live {
				if _, err := db.Exec(`UPDATE runs SET liveness = 'unknown' WHERE id = ?`, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.running {
				if _, err := db.Exec(`UPDATE tasks SET state = 'held', reason = 'start_failed' WHERE id = ?`, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.unheld {
				if _, err := db.Exec(`UPDATE attempts SET workspace_state = 'unknown' WHERE task = ? AND n = ?`, task.ID, task.Attempt); err != nil {
					t.Fatal(err)
				}
			}
			proof := NoDispatchProof{Task: task.ID, Attempt: task.Attempt, Run: run.ID, Generation: run.Generation, Host: run.Host, Dir: run.Dir}
			if tc.change != nil {
				tc.change(&proof)
			}
			if tc.proof {
				host.exit.NoDispatch = &proof
			}
			if tc.mutate != nil {
				host.onExit = func() { tc.mutate(t, db, task, run) }
			}
			_, err := x.Abandon(context.Background(), task)
			if tc.want == "host error" {
				if err == nil {
					t.Fatal("unreachable host accepted")
				}
			} else if tc.want != "" {
				if fault.As(err).Kind != tc.want {
					t.Fatalf("abandon: %v", err)
				}
			} else if err != nil {
				t.Fatalf("abandon: %v", err)
			}
			current, err := coord.LoadRun(db, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if current.Liveness != "exited" || current.StopReason != "abandoned" || current.Endpoint != "" {
					t.Fatalf("run: %+v", current)
				}
				currentTask, err := coord.Load(db, task.ID)
				if err != nil || currentTask.State != "held" {
					t.Fatalf("task: %+v, %v", currentTask, err)
				}
			} else if current.Liveness == "exited" || current.StopReason != "" || current.ExitStatus != nil {
				t.Fatalf("unproven run exited: %+v", current)
			}
		})
	}
}

func TestHostUnknownDoesNotWakeDoneTask(t *testing.T) {
	host := &livenessHost{probeErr: true}
	db, x, task, run := livenessFixture(t, host, true, "done")
	got, err := x.Probe(context.Background(), run)
	if err != nil || got != "unknown" {
		t.Fatalf("probe: %s, %v", got, err)
	}
	currentTask, err := coord.Load(db, task.ID)
	if err != nil || currentTask.State != "done" {
		t.Fatalf("task: %+v, %v", currentTask, err)
	}
	items, err := coord.Items(db, `i.state = 'pending'`)
	if err != nil || len(items) != 0 {
		t.Fatalf("unexpected owner notification: %v, %v", items, err)
	}
}
