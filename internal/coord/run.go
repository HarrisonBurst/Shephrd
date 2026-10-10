package coord

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"

	"shephrd/internal/config"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

type Attempt struct {
	Task           int64  `json:"-"`
	N              int64  `json:"attempt"`
	Host           string `json:"host"`
	Harness        string `json:"harness"`
	Model          string `json:"model,omitempty"`
	Base           string `json:"base,omitempty"`
	StackedOn      int64  `json:"-"`
	Branch         string `json:"branch,omitempty"`
	Workspace      string `json:"workspace"`
	WorkspaceState string `json:"workspace_state"`
	Session        string `json:"session,omitempty"`
	Created        string `json:"created"`
}

type Run struct {
	ID           int64  `json:"run"`
	Task         int64  `json:"-"`
	Attempt      int64  `json:"attempt"`
	Generation   int64  `json:"generation"`
	Host         string `json:"host"`
	Purpose      string `json:"purpose"`
	Dir          string `json:"dir"`
	PID          int    `json:"pid,omitempty"`
	Start        string `json:"-"`
	Liveness     string `json:"liveness"`
	TurnReported bool   `json:"turn_reported"`
	ExitStatus   *int   `json:"exit_status,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	WarnedLong   bool   `json:"-"`
	FromSeq      int64  `json:"-"`
	Started      string `json:"started"`
	LastActivity string `json:"last_activity"`
	Exited       string `json:"exited,omitempty"`
}

const runColumns = `id, task, attempt, generation, host, purpose, dir, COALESCE(pid, 0), COALESCE(start_time, ''), liveness,
	turn_reported, exit_status, stop_reason, warned_long, from_seq, started_at, last_activity, COALESCE(exited_at, '') FROM runs`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var status sql.NullInt64
	err := row.Scan(&r.ID, &r.Task, &r.Attempt, &r.Generation, &r.Host, &r.Purpose, &r.Dir, &r.PID, &r.Start, &r.Liveness,
		&r.TurnReported, &status, &r.StopReason, &r.WarnedLong, &r.FromSeq, &r.Started, &r.LastActivity, &r.Exited)
	if status.Valid {
		code := int(status.Int64)
		r.ExitStatus = &code
	}
	return &r, err
}

func LoadRun(q Querier, id int64) (*Run, error) {
	return scanRun(q.QueryRow(`SELECT `+runColumns+` WHERE id = ?`, id))
}

func Runs(q Querier, where string, args ...any) ([]*Run, error) {
	rows, err := q.Query(`SELECT `+runColumns+` WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// PreviousRun returns the task's run before the given generation, or nil.
func PreviousRun(q Querier, task, generation int64) (*Run, error) {
	r, err := scanRun(q.QueryRow(`SELECT `+runColumns+` WHERE task = ? AND generation < ? ORDER BY generation DESC LIMIT 1`, task, generation))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// CurrentRun returns the task's current run, or nil if it never ran.
func CurrentRun(q Querier, t *Task) (*Run, error) {
	var id sql.NullInt64
	if err := q.QueryRow(`SELECT run FROM tasks WHERE id = ?`, t.ID).Scan(&id); err != nil || !id.Valid {
		return nil, err
	}
	return LoadRun(q, id.Int64)
}

const attemptColumns = `task, n, host, harness, model, base, COALESCE(stacked_on, 0), branch, workspace, workspace_state, session, created_at FROM attempts`

func scanAttempt(row interface{ Scan(...any) error }) (*Attempt, error) {
	var a Attempt
	err := row.Scan(&a.Task, &a.N, &a.Host, &a.Harness, &a.Model, &a.Base, &a.StackedOn, &a.Branch, &a.Workspace, &a.WorkspaceState, &a.Session, &a.Created)
	return &a, err
}

func LoadAttempt(q Querier, task, n int64) (*Attempt, error) {
	a, err := scanAttempt(q.QueryRow(`SELECT `+attemptColumns+` WHERE task = ? AND n = ?`, task, n))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fault.New("not_found", "%s has no attempt %d", TaskRef(task), n)
	}
	return a, err
}

func Attempts(q Querier, where string, args ...any) ([]*Attempt, error) {
	rows, err := q.Query(`SELECT `+attemptColumns+` WHERE `+where+` ORDER BY task, n`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []*Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// RunCaller resolves a run token to the run it was issued for. The run may
// be stale; callers that change state check that with RequireCurrent.
func RunCaller(q Querier, token string) (Caller, *Run, error) {
	r, err := scanRun(q.QueryRow(`SELECT `+runColumns+` WHERE token_hash = ?`, TokenHash(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return Caller{}, nil, fault.New("invalid_token", "the run token does not belong to any run")
	}
	if err != nil {
		return Caller{}, nil, err
	}
	return Caller{Kind: "run", Task: r.Task, Attempt: r.Attempt, Run: r.ID}, r, nil
}

// RequireCurrent refuses input from a run that is not its task's current,
// unfinished run.
func RequireCurrent(q Querier, c Caller) error {
	var current sql.NullInt64
	var liveness string
	err := q.QueryRow(`SELECT t.run, r.liveness FROM tasks t JOIN runs r ON r.id = ? WHERE t.id = ?`, c.Run, c.Task).Scan(&current, &liveness)
	if err != nil {
		return err
	}
	if !current.Valid || current.Int64 != c.Run || liveness == "exited" {
		return fault.New("stale_run", "run %d is no longer the current run of %s", c.Run, TaskRef(c.Task))
	}
	return nil
}

func RecordStale(tx *store.Tx, c Caller, command string) error {
	_, err := tx.Emit("task.stale", c.String(), c.Task, c.Attempt, c.Run, map[string]any{"command": command})
	return err
}

// AllocateAttempt records the intent for a new attempt and its workspace
// before anything is created on disk.
func AllocateAttempt(tx *store.Tx, t *Task, target config.Target, workspace func(n int64) (path, branch string), stackedOn int64) (*Attempt, error) {
	var n int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(n), 0) + 1 FROM attempts WHERE task = ?`, t.ID).Scan(&n); err != nil {
		return nil, err
	}
	path, branch := workspace(n)
	var stacked any
	if stackedOn != 0 {
		stacked = stackedOn
	}
	_, err := tx.Exec(`INSERT INTO attempts (task, n, host, harness, model, base, stacked_on, branch, workspace, workspace_state, session, created_at)
		VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, 'allocating', '', ?)`, t.ID, n, target.Host, target.Harness, target.Model, stacked, branch, path, store.Timestamp(tx.Now))
	if err != nil {
		return nil, err
	}
	return LoadAttempt(tx, t.ID, n)
}

func SetWorkspaceState(tx *store.Tx, caller string, a *Attempt, state, base string) error {
	from := a.WorkspaceState
	a.WorkspaceState = state
	if base != "" {
		a.Base = base
	}
	if _, err := tx.Exec(`UPDATE attempts SET workspace_state = ?, base = ? WHERE task = ? AND n = ?`, state, a.Base, a.Task, a.N); err != nil {
		return err
	}
	if from == state {
		return nil
	}
	_, err := tx.Emit("workspace.state", caller, a.Task, a.N, 0, map[string]any{
		"attempt": a.N, "host": a.Host, "path": a.Workspace, "branch": a.Branch, "base": a.Base, "from": from, "to": state,
	})
	return err
}

// AdoptAttempt makes a prepared attempt the task's current one.
func AdoptAttempt(tx *store.Tx, caller string, t *Task, a *Attempt) error {
	t.Attempt = a.N
	t.Target = config.Target{Host: a.Host, Harness: a.Harness, Model: a.Model}
	if err := Touch(tx, t, `attempt = ?, host = ?, harness = ?, model = ?`, a.N, a.Host, a.Harness, a.Model); err != nil {
		return err
	}
	_, err := tx.Emit("attempt.created", caller, t.ID, a.N, 0, map[string]any{
		"attempt": a.N, "base": a.Base, "branch": a.Branch, "target": t.Target,
	})
	return err
}

var runnableFrom = map[string][]string{
	"start":    {"queued"},
	"retry":    {"waiting", "held", "done"},
	"resume":   {"held"},
	"continue": {"running", "waiting", "done"},
	"nudge":    {"running", "waiting"},
}

// CheckRunnable refuses a new run unless the task's state allows the
// purpose and no current run might still be alive.
func CheckRunnable(q Querier, t *Task, purpose string) error {
	if !slices.Contains(runnableFrom[purpose], t.State) {
		return fault.New("invalid_state", "%s is %s; %s needs %v", t.Ref, t.State, purpose, runnableFrom[purpose]).WithNext("task", "show", t.Ref)
	}
	if purpose == "start" {
		if _, ready, err := Dependencies(q, t.ID); err != nil {
			return err
		} else if !ready {
			return fault.New("not_ready", "%s waits on dependencies that have not reached their milestone", t.Ref).WithNext("task", "show", t.Ref)
		}
	}
	current, err := CurrentRun(q, t)
	if err != nil || current == nil {
		return err
	}
	switch current.Liveness {
	case "live", "starting":
		return fault.New("run_live", "%s run %d is still %s", t.Ref, current.Generation, current.Liveness).WithNext("task", "stop", t.Ref)
	case "unknown":
		return fault.New("liveness_unknown", "%s run %d may still be alive on %s; nothing replaces it until that is known", t.Ref, current.Generation, current.Host).
			WithNext("workspace", "reconcile")
	}
	return nil
}

// BeginRun reserves the next run generation and its token, and makes the
// task running.
func BeginRun(tx *store.Tx, caller string, t *Task, purpose string, dir func(generation int64) string) (*Run, string, error) {
	var generation int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(generation), 0) + 1 FROM runs WHERE task = ?`, t.ID).Scan(&generation); err != nil {
		return nil, "", err
	}
	token := newToken()
	now := store.Timestamp(tx.Now)
	result, err := tx.Exec(`INSERT INTO runs (task, attempt, generation, token_hash, host, purpose, dir, liveness, from_seq, started_at, last_activity)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'starting', (SELECT COALESCE(MAX(seq), 0) FROM events), ?, ?)`, t.ID, t.Attempt, generation, TokenHash(token), t.Target.Host, purpose, dir(generation), now, now)
	if err != nil {
		return nil, "", err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, "", err
	}
	if err := Touch(tx, t, `run = ?, wake_at = NULL`, id); err != nil {
		return nil, "", err
	}
	if t.State != "running" {
		if err := SetState(tx, caller, t, "running", ""); err != nil {
			return nil, "", err
		}
	}
	r, err := LoadRun(tx, id)
	return r, token, err
}

// MarkLive records a run's process identity; the first record emits run.started.
func MarkLive(tx *store.Tx, r *Run, pid int, start, endpoint string, harness, model string) error {
	if r.Liveness != "starting" {
		if r.PID == pid && r.Start == start {
			return nil
		}
		return fault.New("stale_run", "run %d already recorded another process", r.ID)
	}
	if _, err := tx.Exec(`UPDATE runs SET pid = ?, start_time = ?, endpoint = ?, liveness = 'live' WHERE id = ?`, pid, start, endpoint, r.ID); err != nil {
		return err
	}
	r.PID, r.Start, r.Liveness = pid, start, "live"
	_, err := tx.Emit("run.started", "system", r.Task, r.Attempt, r.ID, map[string]any{
		"run": r.Generation, "host": r.Host, "harness": harness, "model": model, "endpoint": endpoint, "purpose": r.Purpose,
	})
	return err
}

func SetLiveness(tx *store.Tx, r *Run, liveness string) error {
	if r.Liveness == liveness {
		return nil
	}
	from := r.Liveness
	r.Liveness = liveness
	if _, err := tx.Exec(`UPDATE runs SET liveness = ? WHERE id = ?`, liveness, r.ID); err != nil {
		return err
	}
	_, err := tx.Emit("run.liveness", "system", r.Task, r.Attempt, r.ID, map[string]string{"run": strconv.FormatInt(r.Generation, 10), "from": from, "to": liveness})
	return err
}

// RecordExit marks a run exited and applies what its exit means. It
// reports whether the run needs its one nudge.
func RecordExit(tx *store.Tx, r *Run, status int, session string) (bool, error) {
	if r.Liveness == "exited" {
		return false, nil
	}
	now := store.Timestamp(tx.Now)
	if _, err := tx.Exec(`UPDATE runs SET liveness = 'exited', exit_status = ?, exited_at = ? WHERE id = ?`, status, now, r.ID); err != nil {
		return false, err
	}
	r.Liveness, r.ExitStatus, r.Exited = "exited", &status, now
	if session != "" {
		if _, err := tx.Exec(`UPDATE attempts SET session = ? WHERE task = ? AND n = ?`, session, r.Task, r.Attempt); err != nil {
			return false, err
		}
	}
	if _, err := tx.Emit("run.exited", "system", r.Task, r.Attempt, r.ID, map[string]any{
		"run": r.Generation, "status": status, "reported": r.TurnReported,
	}); err != nil {
		return false, err
	}
	t, err := Load(tx, r.Task)
	if err != nil {
		return false, err
	}
	current, err := CurrentRun(tx, t)
	if err != nil || current == nil || current.ID != r.ID || r.StopReason != "" {
		return false, err
	}
	if r.TurnReported {
		if t.Role == "driver" && t.State == "running" {
			return false, SettleDriver(tx, t)
		}
		return false, nil
	}
	if t.State != "running" && t.State != "waiting" {
		return false, nil
	}
	if r.Purpose == "nudge" {
		return false, Hold(tx, t, "no_report")
	}
	return true, nil
}

// SettleDriver sets a driver task's state between turns from its requests.
func SettleDriver(tx *store.Tx, t *Task) error {
	requests, err := Requests(tx, t.ID)
	if err != nil {
		return err
	}
	state := "done"
	for _, r := range requests {
		switch {
		case r.State == "open":
			state = "running"
		case r.State == "asked" && state == "done":
			state = "waiting"
		}
	}
	if state == t.State {
		return nil
	}
	return SetState(tx, "system", t, state, "")
}

// Hold makes a task held and wakes its owner.
func Hold(tx *store.Tx, t *Task, reason string) error {
	if err := SetState(tx, "system", t, "held", reason); err != nil {
		return err
	}
	seq, err := lastSeq(tx, t.ID)
	if err != nil {
		return err
	}
	return WakeOwner(tx, t, seq)
}

// WakeOwner tells a task's owner that an event needs its attention. A
// parent task gets a turn; a driver gets an inbox item.
func WakeOwner(tx *store.Tx, t *Task, seq int64) error {
	if t.Parent == 0 {
		return AddItem(tx, t.Driver, t, seq)
	}
	parent, err := Load(tx, t.Parent)
	if err != nil {
		return err
	}
	return Wake(tx, parent)
}
