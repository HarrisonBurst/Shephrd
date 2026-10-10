package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/proc"
	"shephrd/internal/store"
)

type Executor struct {
	DB      *store.Store
	Cfg     *config.Config
	Self    string
	Getenv  config.Getenv
	Harness func(ctx context.Context, name string, req HarnessRequest) (HarnessCommand, error)
	Guide   func(role string) (string, error)
	// Inputs lists a starting attempt's pinned inputs for its brief.
	Inputs func(t *coord.Task, a *coord.Attempt) ([]string, error)
	// Base chooses a new attempt's base when it stacks on a dependency.
	Base func(t *coord.Task) (base string, stackedOn int64, err error)
}

type Started struct {
	Task    *coord.Task    `json:"task"`
	Attempt *coord.Attempt `json:"attempt"`
	Run     *coord.Run     `json:"run"`
}

func (x *Executor) workspacePath(t *coord.Task, n int64) string {
	repo := t.Repo
	if repo == "" {
		repo = "general"
	}
	return filepath.Join(x.Cfg.DataDir, "workspaces", repo, fmt.Sprintf("%s-%d", t.Ref, n))
}

func (x *Executor) runDir(t *coord.Task) func(int64) string {
	return func(generation int64) string {
		return filepath.Join(x.Cfg.DataDir, "runs", t.Ref, strconv.FormatInt(generation, 10))
	}
}

func (x *Executor) write(ctx context.Context, fn func(*store.Tx) error) error {
	return x.DB.Write(ctx, fn)
}

// Start begins a run for one of the purposes coordination allows: start
// and retry create a new attempt and workspace; resume, continue and nudge
// reuse the current attempt's workspace exactly.
func (x *Executor) Start(ctx context.Context, caller string, taskID int64, purpose string, target *config.Target) (*Started, error) {
	var t *coord.Task
	var a *coord.Attempt
	if purpose == "start" || purpose == "retry" {
		var repo coord.Repo
		var base string
		var stackedOn int64
		err := x.write(ctx, func(tx *store.Tx) error {
			var err error
			if t, err = coord.Load(tx, taskID); err != nil {
				return err
			}
			if err := coord.CheckRunnable(tx, t, purpose); err != nil {
				return err
			}
			if x.Base != nil {
				if base, stackedOn, err = x.Base(t); err != nil {
					return err
				}
			}
			chosen := t.Target
			if target != nil {
				chosen = *target
			}
			a, err = coord.AllocateAttempt(tx, t, chosen, func(n int64) (string, string) {
				if t.RepoID == 0 {
					return x.workspacePath(t, n), ""
				}
				return x.workspacePath(t, n), fmt.Sprintf("shephrd/%s/%d", t.Ref, n)
			}, stackedOn)
			if err != nil || t.RepoID == 0 {
				return err
			}
			return tx.QueryRow(`SELECT path, default_branch, setup FROM repos WHERE id = ?`, t.RepoID).Scan(&repo.Path, &repo.DefaultBranch, &repo.Setup)
		})
		if err != nil {
			return nil, err
		}
		base, err = PrepareWorkspace(WorkspaceSpec{Repo: repo.Path, DefaultBranch: repo.DefaultBranch, Path: a.Workspace, Branch: a.Branch, Base: base})
		if err != nil {
			var werr *WorkspaceError
			if !errors.As(err, &werr) {
				werr = &WorkspaceError{State: "unknown", Err: fault.As(err)}
			}
			x.write(ctx, func(tx *store.Tx) error { return coord.SetWorkspaceState(tx, caller, a, werr.State, "") })
			return nil, werr.Err
		}
		var run *coord.Run
		var token string
		err = x.write(ctx, func(tx *store.Tx) error {
			var err error
			if t, err = coord.Load(tx, taskID); err != nil {
				return err
			}
			if err := coord.SetWorkspaceState(tx, caller, a, "held", base); err != nil {
				return err
			}
			if err := coord.CheckRunnable(tx, t, purpose); err != nil {
				return err
			}
			if err := coord.AdoptAttempt(tx, caller, t, a); err != nil {
				return err
			}
			run, token, err = coord.BeginRun(tx, caller, t, purpose, x.runDir(t))
			return err
		})
		if err != nil {
			return nil, err
		}
		if repo.Setup != "" {
			if err := os.MkdirAll(run.Dir, 0o700); err != nil {
				return nil, err
			}
			if err := RunSetup(a.Workspace, repo.Setup, filepath.Join(run.Dir, "setup.log"), []string{"PATH=" + x.Getenv("PATH"), "HOME=" + x.Getenv("HOME")}); err != nil {
				x.abandon(ctx, run, "setup_failed")
				return nil, err
			}
		}
		return x.launch(ctx, t, a, run, token)
	}
	var run *coord.Run
	var token string
	err := x.write(ctx, func(tx *store.Tx) error {
		var err error
		if t, err = coord.Load(tx, taskID); err != nil {
			return err
		}
		if err := coord.CheckRunnable(tx, t, purpose); err != nil {
			return err
		}
		if a, err = coord.LoadAttempt(tx, t.ID, t.Attempt); err != nil {
			return err
		}
		if a.WorkspaceState != "held" {
			return fault.New("workspace_unavailable", "%s attempt %d workspace is %s", t.Ref, a.N, a.WorkspaceState).WithNext("task", "retry", t.Ref)
		}
		run, token, err = coord.BeginRun(tx, caller, t, purpose, x.runDir(t))
		return err
	})
	if err != nil {
		return nil, err
	}
	if a.Branch != "" {
		var repoPath string
		x.DB.QueryRowContext(ctx, `SELECT path FROM repos WHERE id = ?`, t.RepoID).Scan(&repoPath)
		if err := VerifyWorkspace(repoPath, a.Workspace, a.Branch, a.Base); err != nil {
			x.write(ctx, func(tx *store.Tx) error {
				return coord.SetWorkspaceState(tx, "system", a, "unknown", "")
			})
			x.abandon(ctx, run, "workspace_mismatch")
			return nil, err
		}
	}
	return x.launch(ctx, t, a, run, token)
}

func (x *Executor) launch(ctx context.Context, t *coord.Task, a *coord.Attempt, run *coord.Run, token string) (*Started, error) {
	fail := func(err error) (*Started, error) {
		x.abandon(ctx, run, "start_failed")
		return nil, fault.New("start_failed", "%v", err).WithNext("task", "log", t.Ref)
	}
	if err := os.MkdirAll(run.Dir, 0o700); err != nil {
		return fail(err)
	}
	brief, err := x.brief(ctx, t, a, run)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(BriefPath(run.Dir), []byte(brief), 0o600); err != nil {
		return fail(err)
	}
	if t.Role == "driver" {
		if err := SetReadOnly(a.Workspace, true); err != nil {
			return fail(err)
		}
	}
	mode := "new"
	if a.Session != "" {
		mode = "resume"
	}
	command, err := x.Harness(ctx, a.Harness, HarnessRequest{
		Mode: mode, ReadOnly: t.Role == "driver", Brief: BriefPath(run.Dir), Prompt: brief,
		Workspace: a.Workspace, Model: a.Model, Session: a.Session, Title: t.Ref + " " + t.Title,
	})
	if err != nil {
		return fail(err)
	}
	if len(command.Command) == 0 {
		return fail(errors.New("harness provider returned no command"))
	}
	if command.Session != "" && command.Session != a.Session {
		a.Session = command.Session
		if err := x.write(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE attempts SET session = ? WHERE task = ? AND n = ?`, a.Session, a.Task, a.N)
			return err
		}); err != nil {
			return fail(err)
		}
	}
	id, err := Launch(run.Dir, RunSpec{
		Command: command.Command, Env: command.Env, Workspace: a.Workspace, Harness: a.Harness,
		Shephrd: x.Self, Config: x.Cfg.Path, Path: x.Getenv("PATH"), Home: x.Getenv("HOME"),
	}, token)
	if err != nil {
		return fail(err)
	}
	err = x.write(ctx, func(tx *store.Tx) error {
		r, err := coord.LoadRun(tx, run.ID)
		if err != nil {
			return err
		}
		run = r
		if r.Liveness == "starting" {
			return coord.MarkLive(tx, r, id.PID, id.Start, "", a.Harness, a.Model)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	t, err = coord.Load(x.DB, t.ID)
	return &Started{Task: t, Attempt: a, Run: run}, err
}

// abandon ends a run that never got a live process and holds its task.
func (x *Executor) abandon(ctx context.Context, run *coord.Run, reason string) {
	x.write(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE runs SET stop_reason = ? WHERE id = ?`, reason, run.ID); err != nil {
			return err
		}
		run.StopReason = reason
		if _, err := coord.RecordExit(tx, run, -1, ""); err != nil {
			return err
		}
		t, err := coord.Load(tx, run.Task)
		if err != nil {
			return err
		}
		return coord.Hold(tx, t, reason)
	})
}

func (x *Executor) brief(ctx context.Context, t *coord.Task, a *coord.Attempt, run *coord.Run) (string, error) {
	in := briefInput{task: t, attempt: a, run: run}
	guide, err := x.Guide(map[string]string{"worker": "worker", "driver": "subdriver"}[t.Role])
	if err != nil {
		return "", err
	}
	in.guide = guide
	previous, err := coord.PreviousRun(x.DB, t.ID, run.Generation)
	if err != nil {
		return "", err
	}
	if run.Purpose == "nudge" && previous != nil {
		in.nudge = "no_report"
		if previous.StopReason == "inactive" {
			in.nudge = "inactive"
		}
	}
	from := int64(0)
	if previous != nil {
		from = previous.FromSeq
	}
	since, err := x.DB.EventsWhere(ctx, `WHERE seq > ? AND seq <= ? AND caller != ?
		AND (task = ? OR task IN (SELECT id FROM tasks WHERE parent = ?))
		AND name IN ('task.message', 'task.reply', 'task.question', 'task.result', 'task.blocker', 'task.state', 'dependency.changed', 'task.ready')
		ORDER BY seq DESC LIMIT 101`, from, run.FromSeq, "task:"+t.Ref, t.ID, t.ID)
	if err != nil {
		return "", err
	}
	if len(since) > 100 {
		since, in.truncated = since[:100], true
	}
	for i, j := 0, len(since)-1; i < j; i, j = i+1, j-1 {
		since[i], since[j] = since[j], since[i]
	}
	in.since = since
	notes, err := x.DB.EventsWhere(ctx, `WHERE task = ? AND name = 'task.note' ORDER BY seq DESC LIMIT 5`, t.ID)
	if err != nil {
		return "", err
	}
	for i, j := 0, len(notes)-1; i < j; i, j = i+1, j-1 {
		notes[i], notes[j] = notes[j], notes[i]
	}
	in.notes = notes
	if t.Role == "driver" {
		requests, err := coord.Requests(x.DB, t.ID)
		if err != nil {
			return "", err
		}
		for _, r := range requests {
			if r.State == "answered" {
				continue
			}
			view := requestView{Request: r, body: t.Objective}
			var data sql.NullString
			x.DB.QueryRowContext(ctx, `SELECT data FROM events WHERE seq = ? AND name = 'task.message'`, r.Seq).Scan(&data)
			if data.Valid {
				view.body = eventText(store.Event{Data: []byte(data.String)})
			}
			if view.children, err = coord.Query(x.DB, `t.parent = ? AND t.request = ?`, t.ID, r.Seq); err != nil {
				return "", err
			}
			in.requests = append(in.requests, view)
		}
	}
	if x.Inputs != nil {
		if in.inputs, err = x.Inputs(t, a); err != nil {
			return "", err
		}
	}
	return renderBrief(in), nil
}

// Exited records a run's exit as its supervisor reported it, and starts
// the one nudge when the run ended without reporting its turn.
func (x *Executor) Exited(ctx context.Context, runID int64, status int, session string) error {
	var nudge bool
	var taskID int64
	err := x.write(ctx, func(tx *store.Tx) error {
		r, err := coord.LoadRun(tx, runID)
		if err != nil {
			return err
		}
		taskID = r.Task
		nudge, err = coord.RecordExit(tx, r, status, session)
		return err
	})
	if err != nil || !nudge {
		return err
	}
	_, err = x.Start(ctx, "system", taskID, "nudge", nil)
	return err
}

// Probe observes a run's liveness and records what it finds. A run whose
// process is proven gone is exited, using its exit record when the
// supervisor left one; a run with no exit record was lost.
func (x *Executor) Probe(ctx context.Context, r *coord.Run) (string, error) {
	if r.Liveness == "exited" {
		return "exited", nil
	}
	observed := "unknown"
	switch {
	case r.PID != 0:
		observed = proc.Identity{PID: r.PID, Start: r.Start}.Probe()
	case r.Liveness == "starting":
		started, _ := time.Parse("2006-01-02T15:04:05.000000000Z", r.Started)
		if time.Since(started) < 2*time.Minute {
			return "starting", nil
		}
	}
	if observed != "exited" {
		err := x.write(ctx, func(tx *store.Tx) error { return coord.SetLiveness(tx, r, observed) })
		return observed, err
	}
	if record, ok := ReadExit(r.Dir); ok {
		return "exited", x.Exited(ctx, r.ID, record.Status, record.Session)
	}
	err := x.write(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE runs SET stop_reason = 'lost' WHERE id = ? AND stop_reason = ''`, r.ID); err != nil {
			return err
		}
		r.StopReason = "lost"
		if _, err := coord.RecordExit(tx, r, -1, ""); err != nil {
			return err
		}
		t, err := coord.Load(tx, r.Task)
		if err != nil {
			return err
		}
		if current, err := coord.CurrentRun(tx, t); err != nil || current.ID != r.ID || (t.State != "running" && t.State != "waiting") {
			return err
		}
		return coord.Hold(tx, t, "lost")
	})
	return "exited", err
}

// Stop ends a task's current run: it signals the run's process group, and
// counts the run stopped only once the group is confirmed gone.
func (x *Executor) Stop(ctx context.Context, t *coord.Task, reason string) (*coord.Run, error) {
	r, err := coord.CurrentRun(x.DB, t)
	if err != nil {
		return nil, err
	}
	if r != nil && r.Liveness != "exited" {
		if err := x.write(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE runs SET stop_reason = ? WHERE id = ?`, reason, r.ID)
			return err
		}); err != nil {
			return nil, err
		}
		r.StopReason = reason
		if r.PID == 0 || !proc.StopGroup(r.PID, 5*time.Second) {
			err := x.write(ctx, func(tx *store.Tx) error {
				if err := coord.SetLiveness(tx, r, "unknown"); err != nil {
					return err
				}
				t, err := coord.Load(tx, t.ID)
				if err != nil || t.State == "held" {
					return err
				}
				return coord.Hold(tx, t, reason)
			})
			if err != nil {
				return nil, err
			}
			return r, fault.New("stop_unconfirmed", "%s run %d could not be confirmed stopped; its liveness is unknown", t.Ref, r.Generation).
				WithNext("workspace", "reconcile")
		}
	}
	err = x.write(ctx, func(tx *store.Tx) error {
		if r != nil && r.Liveness != "exited" {
			if _, err := coord.RecordExit(tx, r, -1, ""); err != nil {
				return err
			}
		}
		t, err := coord.Load(tx, t.ID)
		if err != nil {
			return err
		}
		switch t.State {
		case "running", "waiting":
			return coord.Hold(tx, t, reason)
		case "held":
			return nil
		}
		return fault.New("not_running", "%s is %s; there is nothing to stop", t.Ref, t.State)
	})
	return r, err
}
