package cli_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/testkit"
)

type wakeObservation struct {
	Time         string `json:"time"`
	State        string `json:"state,omitempty"`
	WakeAt       string `json:"wake_at,omitempty"`
	RunLiveness  string `json:"run_liveness,omitempty"`
	TurnReported bool   `json:"turn_reported"`
	Eligibility  string `json:"eligibility,omitempty"`
	WokenEvents  int    `json:"woken_events"`
	Error        string `json:"error,omitempty"`
}

func subDriverDiagnostic(t *testing.T, env *testkit.Env) func(bool) {
	t.Helper()
	store := filepath.Join(env.Home, ".local", "state", "shephrd", "shephrd.db")
	db, err := sql.Open("sqlite", "file:"+store+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var trace []wakeObservation
	return func(timedOut bool) {
		if !timedOut {
			at := time.Now().UTC()
			obs := wakeObservation{Time: at.Format(time.RFC3339Nano)}
			var wake, liveness sql.NullString
			var reported int
			err := db.QueryRow(`SELECT t.state, t.wake_at, r.liveness, COALESCE(r.turn_reported, 0),
				(SELECT count(*) FROM events WHERE task = t.id AND name = 'turn.woken')
				FROM tasks t LEFT JOIN runs r ON r.id = t.run WHERE t.id = 1`).Scan(&obs.State, &wake, &liveness, &reported, &obs.WokenEvents)
			if err != nil {
				obs.Error = err.Error()
			} else {
				obs.WakeAt, obs.RunLiveness, obs.TurnReported = wake.String, liveness.String, reported != 0
				switch {
				case obs.State != "running" && obs.State != "waiting" && obs.State != "done":
					obs.Eligibility = "state_not_wakeable"
				case !wake.Valid:
					obs.Eligibility = "no_pending_wake"
				case wake.String > at.Add(-500*time.Millisecond).Format("2006-01-02T15:04:05.000000000Z"):
					obs.Eligibility = "settling"
				case liveness.Valid && liveness.String != "exited":
					obs.Eligibility = "run_not_exited"
				default:
					obs.Eligibility = "wake_query_candidate"
				}
			}
			trace = append(trace, obs)
			return
		}
		dir := os.Getenv("SHEPHRD_TEST_DIAGNOSTICS")
		if dir == "" {
			dir, err = os.MkdirTemp("", "shephrd-subdriver-diagnostic-")
		} else {
			err = os.MkdirAll(dir, 0o700)
		}
		if err == nil {
			err = saveSubDriverDiagnostic(db, dir, trace, env.DaemonLog())
		}
		if err != nil {
			t.Logf("sub-driver diagnostic could not be saved: %v", err)
		} else {
			t.Logf("sub-driver diagnostic (synthetic fixture only): %s/{store.sqlite,events.jsonl,wake-trace.jsonl}", dir)
		}
	}
}

func saveSubDriverDiagnostic(db *sql.DB, dir string, trace []wakeObservation, daemonLog string) error {
	if _, err := db.Exec(`VACUUM INTO ?`, filepath.Join(dir, "store.sqlite")); err != nil {
		return fmt.Errorf("snapshot fixture store: %w", err)
	}
	snapshot, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "store.sqlite")+"?mode=ro")
	if err != nil {
		return err
	}
	defer snapshot.Close()
	events, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return err
	}
	rows, err := snapshot.Query(`SELECT seq, time, name, COALESCE(task, 0), COALESCE(attempt, 0), COALESCE(run, 0) FROM events ORDER BY seq`)
	if err == nil {
		encoder := json.NewEncoder(events)
		for rows.Next() {
			var seq int
			var at, name string
			var task, attempt, run int64
			if err = rows.Scan(&seq, &at, &name, &task, &attempt, &run); err != nil {
				break
			}
			err = encoder.Encode(map[string]any{"seq": seq, "time": at, "name": name, "task": task, "attempt": attempt, "run": run})
			if err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
	}
	closeErr := events.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	wakeTrace, err := os.Create(filepath.Join(dir, "wake-trace.jsonl"))
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(wakeTrace)
	for _, obs := range trace {
		if err = encoder.Encode(obs); err != nil {
			break
		}
	}
	if err == nil {
		for _, line := range strings.Split(daemonLog, "\n") {
			if strings.Contains(line, "continue t_1") || strings.Contains(line, "wakes:") {
				if err = encoder.Encode(map[string]string{"daemon_error": line}); err != nil {
					break
				}
			}
		}
	}
	closeErr = wakeTrace.Close()
	if err != nil {
		return err
	}
	return closeErr
}
