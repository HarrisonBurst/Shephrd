package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/guide"
	"shephrd/internal/plugin"
	"shephrd/internal/proc"
	"shephrd/internal/store"
)

type Executor struct {
	DB     *store.Store
	Cfg    *config.Config
	Getenv config.Getenv
	Host   func(name string) (Host, error)
	Guide  func(role string) (string, error)
	// Inputs lists a starting attempt's pinned inputs for its brief.
	Inputs func(ctx context.Context, t *coord.Task, a *coord.Attempt) ([]string, error)
	// Base chooses a new attempt's base when it stacks on a dependency.
	Base func(t *coord.Task) (base string, stackedOn int64, err error)

	info map[string]HostInfo
}

func New(db *store.Store, cfg *config.Config, reg *plugin.Registry, getenv config.Getenv) (*Executor, error) {
	harness := Harnesses(reg, getenv)
	hosts := Hosts(cfg, getenv)
	return &Executor{
		DB: db, Cfg: cfg, Getenv: getenv,
		Host: func(name string) (Host, error) {
			h, err := hosts(name)
			if local, ok := h.(*Local); ok {
				local.Env.Harness = harness
				local.Env.Plugins = reg
			}
			return h, err
		},
		Guide: func(role string) (string, error) {
			text, _, err := guide.Document(cfg, role)
			return text, err
		},
	}, nil
}

type Started struct {
	Task    *coord.Task    `json:"task"`
	Attempt *coord.Attempt `json:"attempt"`
	Run     *coord.Run     `json:"run"`
}

// HostInfo asks a host for its release, data directory and providers once
// per executor. A host on another release is refused before anything is
// created there.
func (x *Executor) HostInfo(ctx context.Context, name string) (Host, HostInfo, error) {
	h, err := x.Host(name)
	if err != nil {
		return nil, HostInfo{}, err
	}
	if info, ok := x.info[name]; ok {
		return h, info, nil
	}
	var info HostInfo
	if err := h.Call(ctx, "info", struct{}{}, &info); err != nil {
		return nil, HostInfo{}, err
	}
	if x.info == nil {
		x.info = map[string]HostInfo{}
	}
	x.info[name] = info
	return h, info, nil
}

func (x *Executor) write(ctx context.Context, fn func(*store.Tx) error) error {
	return x.DB.Write(ctx, fn)
}

// Start begins a run for one of the purposes coordination allows: start
// and retry create a new attempt and workspace; resume, continue and nudge
// reuse the current attempt's workspace exactly.
func (x *Executor) Start(ctx context.Context, caller string, taskID int64, purpose string, target *config.Target) (*Started, error) {
	t, err := coord.Load(x.DB, taskID)
	if err != nil {
		return nil, err
	}
	if err := coord.CheckRunnable(x.DB, t, purpose); err != nil {
		return nil, err
	}
	chosen := t.Target
	if target != nil {
		chosen = *target
	}
	host, info, err := x.HostInfo(ctx, chosen.Host)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(info.Harnesses, chosen.Harness) {
		return nil, fault.New("unknown_harness", "host %s has no harness %q installed; it has %v", chosen.Host, chosen.Harness, info.Harnesses)
	}
	runDir := func(t *coord.Task) func(int64) string {
		return func(generation int64) string {
			return filepath.Join(info.DataDir, "runs", t.Ref, strconv.FormatInt(generation, 10))
		}
	}
	var a *coord.Attempt
	var run *coord.Run
	var token string
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
			a, err = coord.AllocateAttempt(tx, t, chosen, func(n int64) (string, string) {
				name := t.Repo
				if name == "" {
					name = "general"
				}
				path := filepath.Join(info.DataDir, "workspaces", name, fmt.Sprintf("%s-%d", t.Ref, n))
				if t.RepoID == 0 {
					return path, ""
				}
				return path, fmt.Sprintf("shephrd/%s/%d", t.Ref, n)
			}, stackedOn)
			if err != nil || t.RepoID == 0 {
				return err
			}
			return tx.QueryRow(`SELECT path, default_branch, setup FROM repos WHERE id = ?`, t.RepoID).Scan(&repo.Path, &repo.DefaultBranch, &repo.Setup)
		})
		if err != nil {
			return nil, err
		}
		var prepared struct {
			Base string `json:"base"`
		}
		err = host.Call(ctx, "prepare_workspace", WorkspaceSpec{Repo: repo.Path, DefaultBranch: repo.DefaultBranch, Path: a.Workspace, Branch: a.Branch, Base: base}, &prepared)
		if err != nil {
			var werr *WorkspaceError
			if !errors.As(err, &werr) {
				werr = &WorkspaceError{State: "unknown", Err: fault.As(err)}
			}
			x.write(ctx, func(tx *store.Tx) error { return coord.SetWorkspaceState(tx, caller, a, werr.State, "") })
			return nil, werr.Err
		}
		err = x.write(ctx, func(tx *store.Tx) error {
			var err error
			if t, err = coord.Load(tx, taskID); err != nil {
				return err
			}
			if err := coord.SetWorkspaceState(tx, caller, a, "held", prepared.Base); err != nil {
				return err
			}
			if err := coord.CheckRunnable(tx, t, purpose); err != nil {
				return err
			}
			if err := coord.AdoptAttempt(tx, caller, t, a); err != nil {
				return err
			}
			run, token, err = coord.BeginRun(tx, caller, t, purpose, runDir(t))
			return err
		})
		if err != nil {
			return nil, err
		}
		if repo.Setup != "" {
			if err := host.Call(ctx, "setup", SetupRequest{Path: a.Workspace, Command: repo.Setup, Log: filepath.Join(run.Dir, "setup.log")}, nil); err != nil {
				x.abandon(ctx, run, "setup_failed")
				return nil, err
			}
		}
		return x.launch(ctx, host, t, a, run, token)
	}
	err = x.write(ctx, func(tx *store.Tx) error {
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
		run, token, err = coord.BeginRun(tx, caller, t, purpose, runDir(t))
		return err
	})
	if err != nil {
		return nil, err
	}
	if a.Branch != "" {
		var repoPath string
		x.DB.QueryRowContext(ctx, `SELECT path FROM repos WHERE id = ?`, t.RepoID).Scan(&repoPath)
		if err := host.Call(ctx, "verify_workspace", VerifyRequest{Repo: repoPath, Path: a.Workspace, Branch: a.Branch, Base: a.Base}, nil); err != nil {
			if fault.As(err).Kind == "workspace_mismatch" {
				x.write(ctx, func(tx *store.Tx) error { return coord.SetWorkspaceState(tx, "system", a, "unknown", "") })
			}
			x.abandon(ctx, run, fault.As(err).Kind)
			return nil, err
		}
	}
	return x.launch(ctx, host, t, a, run, token)
}

func (x *Executor) launch(ctx context.Context, host Host, t *coord.Task, a *coord.Attempt, run *coord.Run, token string) (*Started, error) {
	fail := func(err error) (*Started, error) {
		x.abandon(ctx, run, "start_failed")
		return nil, fault.New("start_failed", "%v", err).WithNext("task", "log", t.Ref)
	}
	_, info, err := x.HostInfo(ctx, a.Host)
	if err != nil {
		return fail(err)
	}
	brief, err := x.brief(ctx, info.Shephrd, t, a, run)
	if err != nil {
		return fail(err)
	}
	if t.Role == "driver" {
		if err := host.Call(ctx, "read_only", ReadOnlyRequest{Path: a.Workspace, ReadOnly: true}, nil); err != nil {
			return fail(err)
		}
	}
	mode := "new"
	if a.Session != "" {
		mode = "resume"
	}
	var command HarnessCommand
	err = host.Call(ctx, "harness", HarnessCall{Name: a.Harness, Request: HarnessRequest{
		Mode: mode, ReadOnly: t.Role == "driver", Brief: BriefPath(run.Dir), Prompt: brief,
		Workspace: a.Workspace, Model: a.Model, Session: a.Session, Title: t.Ref + " " + t.Title,
	}}, &command)
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
	var launched LaunchResponse
	err = host.Call(ctx, "launch", LaunchRequest{
		RunDir: run.Dir, Brief: brief, Token: token,
		Spec: RunSpec{Command: command.Command, Env: command.Env, Workspace: a.Workspace, Harness: a.Harness},
	}, &launched)
	if err != nil {
		return fail(err)
	}
	err = x.write(ctx, func(tx *store.Tx) error {
		if launched.Endpoint != "" {
			if _, err := tx.Exec(`UPDATE runs SET endpoint = ? WHERE id = ?`, launched.Endpoint, run.ID); err != nil {
				return err
			}
		}
		r, err := coord.LoadRun(tx, run.ID)
		if err != nil {
			return err
		}
		run = r
		if r.Liveness == "starting" && launched.PID != 0 {
			return coord.MarkLive(tx, r, launched.PID, launched.Start, "", a.Harness, a.Model)
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

func (x *Executor) brief(ctx context.Context, shephrd string, t *coord.Task, a *coord.Attempt, run *coord.Run) (string, error) {
	in := briefInput{shephrd: shephrd, task: t, attempt: a, run: run}
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
		AND name IN ('task.message', 'task.reply', 'task.question', 'task.result', 'task.blocker', 'task.state', 'dependency.changed', 'task.ready', 'workspace.retained')
		ORDER BY seq DESC LIMIT 101`, from, run.FromSeq, "task:"+t.Ref, t.ID, t.ID)
	if err != nil {
		return "", err
	}
	if len(since) > 100 {
		since, in.truncated = since[:100], true
	}
	slices.Reverse(since)
	in.since = since
	notes, err := x.DB.EventsWhere(ctx, `WHERE task = ? AND name = 'task.note' ORDER BY seq DESC LIMIT 5`, t.ID)
	if err != nil {
		return "", err
	}
	slices.Reverse(notes)
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
		if in.inputs, err = x.Inputs(ctx, t, a); err != nil {
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

// Probe observes a run's liveness on its host and records what it finds.
// An unreachable host makes the run unknown, never exited. A run whose
// process is proven gone is exited, using its exit record when the
// supervisor left one; a run with no exit record was lost.
func (x *Executor) Probe(ctx context.Context, r *coord.Run) (string, error) {
	if r.Liveness == "exited" {
		return "exited", nil
	}
	observed := "unknown"
	host, hostErr := x.Host(r.Host)
	switch {
	case r.PID != 0 && hostErr == nil:
		var state StateResponse
		if host.Call(ctx, "probe", proc.Identity{PID: r.PID, Start: r.Start}, &state) == nil {
			observed = state.State
		}
	case r.Liveness == "starting" && r.Endpoint != "" && hostErr == nil:
		var shown PresentationResponse
		if err := host.Call(ctx, "presentation", PresentationRequest{Operation: "probe", Endpoint: r.Endpoint}, &shown); err != nil || shown.State != "absent" {
			return "starting", nil
		}
		observed = "exited"
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
	var exit ExitResponse
	if err := host.Call(ctx, "exit_record", RunDirRequest{RunDir: r.Dir}, &exit); err != nil {
		return "unknown", nil
	}
	if exit.Found {
		return "exited", x.Exited(ctx, r.ID, exit.Record.Status, exit.Record.Session)
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

// stopGroup stops a run's process group on its host and reports whether
// the group is confirmed gone.
func (x *Executor) stopGroup(ctx context.Context, r *coord.Run) bool {
	host, err := x.Host(r.Host)
	if err != nil {
		return false
	}
	stopped := false
	if r.PID != 0 {
		var state StateResponse
		stopped = host.Call(ctx, "stop", StopRequest{PGID: r.PID}, &state) == nil && state.State == "exited"
	}
	if r.Endpoint != "" {
		closed := host.Call(ctx, "presentation", PresentationRequest{Operation: "close", Endpoint: r.Endpoint}, nil) == nil
		stopped = stopped || r.PID == 0 && closed
	}
	return stopped
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
		if !x.stopGroup(ctx, r) {
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

// Log reads the end of a run's session log from its host.
func (x *Executor) Log(ctx context.Context, r *coord.Run, limit int64) (LogResponse, error) {
	host, err := x.Host(r.Host)
	if err != nil {
		return LogResponse{}, err
	}
	var out LogResponse
	return out, host.Call(ctx, "read_log", RunDirRequest{RunDir: r.Dir, Limit: limit}, &out)
}
