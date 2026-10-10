package cli

import (
	"slices"
	"time"

	"github.com/spf13/cobra"

	"shephrd/internal/artifact"
	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/execution"
	"shephrd/internal/fault"
	"shephrd/internal/guide"
	"shephrd/internal/store"
)

func (a *app) executor() (*execution.Executor, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}
	db, err := a.store()
	if err != nil {
		return nil, err
	}
	reg, err := a.registry()
	if err != nil {
		return nil, err
	}
	x, err := execution.New(db, cfg, reg, a.getenv)
	if err != nil {
		return nil, err
	}
	x.Inputs = artifact.Inputs(db, cfg, x.Host)
	x.Base = artifact.StackBase(db, cfg)
	return x, nil
}

func runCommands(a *app) []*cobra.Command {
	return []*cobra.Command{taskRun(a, "start"), taskRun(a, "resume"), taskRun(a, "retry"), taskStop(a), taskLog(a)}
}

func taskRun(a *app, purpose string) *cobra.Command {
	short := map[string]string{
		"start":  "Start a queued task's first attempt",
		"resume": "Run a held task again in the same attempt and workspace",
		"retry":  "Start a new attempt from a fresh base; the old workspace is kept",
	}[purpose]
	var target config.Target
	cmd := &cobra.Command{Use: purpose + " <task>", Short: short, Args: cobra.ExactArgs(1)}
	if purpose == "retry" {
		cmd.Flags().StringVar(&target.Harness, "harness", "", "run the new attempt with this harness")
		cmd.Flags().StringVar(&target.Model, "model", "", "run the new attempt with this model")
	}
	cmd.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		caller, db, t, err := a.readTask(args[0])
		if err != nil {
			return nil, err
		}
		if _, err := coord.GetOwned(db, caller, args[0]); err != nil {
			return nil, err
		}
		var chosen *config.Target
		if target.Harness != "" || target.Model != "" {
			next := t.Target
			if target.Harness != "" {
				next.Harness, next.Model = target.Harness, target.Model
			} else {
				next.Model = target.Model
			}
			reg, err := a.registry()
			if err != nil {
				return nil, err
			}
			if _, err := execution.CheckHarness(reg, next.Harness); err != nil {
				return nil, err
			}
			chosen = &next
		}
		return a.once(func(caller Caller) (any, error) {
			if err := a.gate("task.start", t.ID, map[string]any{"caller": caller.String(), "task": t.Ref, "purpose": purpose}); err != nil {
				return nil, err
			}
			x, err := a.executor()
			if err != nil {
				return nil, err
			}
			return x.Start(a.ctx, caller.String(), t.ID, purpose, chosen)
		})
	})
	return cmd
}

func taskStop(a *app) *cobra.Command {
	var tree bool
	cmd := &cobra.Command{Use: "stop <task>", Short: "Stop a task's current run, or with --tree its whole subtree", Args: cobra.ExactArgs(1)}
	cmd.Flags().BoolVar(&tree, "tree", false, "stop every running task in the subtree")
	cmd.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		caller, db, t, err := a.readTask(args[0])
		if err != nil {
			return nil, err
		}
		if !tree {
			if _, err := coord.GetOwned(db, caller, args[0]); err != nil {
				return nil, err
			}
		}
		return a.once(func(caller Caller) (any, error) {
			x, err := a.executor()
			if err != nil {
				return nil, err
			}
			targets := []*coord.Task{t}
			if tree {
				if targets, err = coord.Query(db, `t.id IN (WITH RECURSIVE sub(id) AS (SELECT ? UNION SELECT c.id FROM tasks c JOIN sub ON c.parent = sub.id) SELECT id FROM sub)
					AND t.state IN ('running', 'waiting')`, t.ID); err != nil {
					return nil, err
				}
			}
			stopped := []map[string]any{}
			for _, target := range targets {
				run, err := x.Stop(a.ctx, target, "stopped")
				if err != nil && !tree {
					return nil, err
				}
				entry := map[string]any{"task": target.Ref}
				if run != nil {
					entry["run"] = run.Generation
				}
				if err != nil {
					entry["error"] = fault.As(err)
				}
				stopped = append(stopped, entry)
			}
			return map[string]any{"stopped": stopped}, nil
		})
	})
	return cmd
}

func taskLog(a *app) *cobra.Command {
	var generation, limit int64
	cmd := &cobra.Command{Use: "log <task>", Short: "Read a run's session log", Args: cobra.ExactArgs(1)}
	cmd.Flags().Int64Var(&generation, "run", 0, "the run generation (default: the current run)")
	cmd.Flags().Int64Var(&limit, "bytes", 64<<10, "how many bytes from the end of the log")
	cmd.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		_, db, t, err := a.readTask(args[0])
		if err != nil {
			return nil, err
		}
		var run *coord.Run
		if generation == 0 {
			run, err = coord.CurrentRun(db, t)
		} else {
			var runs []*coord.Run
			runs, err = coord.Runs(db, `task = ? AND generation = ?`, t.ID, generation)
			if len(runs) == 1 {
				run = runs[0]
			}
		}
		if err != nil {
			return nil, err
		}
		if run == nil {
			return nil, fault.New("not_found", "%s has no such run", t.Ref)
		}
		x, err := a.executor()
		if err != nil {
			return nil, err
		}
		log, err := x.Log(a.ctx, run, min(max(limit, 1), 1<<20))
		if err != nil {
			return nil, err
		}
		return map[string]any{"task": t.Ref, "run": run, "log": log.Text, "truncated": log.Truncated}, nil
	})
	return cmd
}

func reportCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "report", Short: "Report on your own task from a session Shephrd started"}
	for _, kind := range coord.ReportKinds {
		var request int64
		var files []string
		sub := &cobra.Command{Use: kind + " <text|->", Short: "Report " + kind, Args: cobra.ExactArgs(1)}
		if kind == "question" || kind == "result" {
			sub.Flags().Int64Var(&request, "request", 0, "for a sub-driver, the request this answers")
		}
		if kind == "result" {
			sub.Flags().StringArrayVar(&files, "file", nil, "a report file in your workspace; repeatable")
		}
		sub.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
			limit := coord.MaxReport
			if kind == "note" {
				limit = coord.MaxReportNote
			}
			body, err := a.text(kind, args[0], limit)
			if err != nil {
				return nil, err
			}
			caller, err := a.identity()
			if err != nil {
				return nil, err
			}
			report := &coord.Report{Kind: kind, Body: body, Request: request}
			db, err := a.store()
			if err != nil {
				return nil, err
			}
			if err := a.requireCurrentRun(caller, "report "+kind); err != nil {
				return nil, err
			}
			t, err := coord.ReportTarget(db, caller, report)
			if err != nil {
				return nil, err
			}
			var sealed *artifact.Artifact
			if kind == "result" {
				if err := a.gate("report.result", t.ID, map[string]any{"task": t.Ref, "body": body, "request": report.Request, "files": files}); err != nil {
					return nil, err
				}
				if sealed, err = a.seal(caller, t, body, files); err != nil {
					return nil, err
				}
			}
			return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
				t, err := coord.ReportTarget(tx, caller, report)
				if err != nil {
					return nil, err
				}
				out := map[string]any{"task": t.Ref, "kind": kind}
				if sealed != nil {
					if err := artifact.Record(tx, caller, t, sealed); err != nil {
						return nil, err
					}
					if err := artifact.Sealed(tx, caller, t, sealed); err != nil {
						return nil, err
					}
					report.Extra = map[string]any{"artifact": sealed.Ref}
					out["artifact"] = sealed
				}
				seq, err := coord.RecordReport(tx, caller, t, report)
				out["seq"] = seq
				return out, err
			})
		})
		cmd.AddCommand(sub)
	}
	return cmd
}

func privateRunCommands(a *app) []*cobra.Command {
	var pid int
	var start string
	started := &cobra.Command{Use: "_started", Hidden: true, Args: cobra.NoArgs}
	started.Flags().IntVar(&pid, "pid", 0, "")
	started.Flags().StringVar(&start, "start", "", "")
	started.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		caller, err := a.identity()
		if err != nil {
			return nil, err
		}
		if caller.Kind != "run" {
			return nil, fault.New("not_a_run", "only a supervisor records its run's process")
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		err = db.Write(a.ctx, func(tx *store.Tx) error {
			if err := coord.RequireCurrent(tx, caller); err != nil {
				return err
			}
			r, err := coord.LoadRun(tx, caller.Run)
			if err != nil {
				return err
			}
			a, err := coord.LoadAttempt(tx, r.Task, r.Attempt)
			if err != nil {
				return err
			}
			return coord.MarkLive(tx, r, pid, start, "", a.Harness, a.Model)
		})
		return map[string]any{"run": caller.Run}, err
	})
	var status int
	var session string
	exited := &cobra.Command{Use: "_exited", Hidden: true, Args: cobra.NoArgs}
	exited.Flags().IntVar(&status, "status", 0, "")
	exited.Flags().StringVar(&session, "session", "", "")
	exited.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		caller, err := a.identity()
		if err != nil {
			return nil, err
		}
		if caller.Kind != "run" {
			return nil, fault.New("not_a_run", "only a supervisor records its run's exit")
		}
		x, err := a.executor()
		if err != nil {
			return nil, err
		}
		if err := x.Exited(a.ctx, caller.Run, status, session); err != nil {
			return nil, err
		}
		if r, err := coord.LoadRun(x.DB, caller.Run); err == nil {
			if t, err := coord.Load(x.DB, r.Task); err == nil && t.State == "closed" {
				if s, err := a.artifacts(); err == nil {
					s.Release(a.ctx, t.ID, false)
				}
			}
		}
		return map[string]any{"run": caller.Run}, nil
	})
	turn := &cobra.Command{Use: "_turn", Hidden: true, Args: cobra.NoArgs}
	turn.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		caller, err := a.identity()
		if err != nil {
			return nil, err
		}
		if caller.Kind != "run" {
			return nil, fault.New("not_a_run", "only a supervisor asks whether its run's turn ended")
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		if coord.RequireCurrent(db, caller) != nil {
			return map[string]any{"ended": true}, nil
		}
		r, err := coord.LoadRun(db, caller.Run)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ended": r.TurnReported}, nil
	})
	return []*cobra.Command{started, exited, turn}
}

func workspaceCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "workspace", Short: "Workspace recovery"}
	cmd.AddCommand(&cobra.Command{
		Use:   "reconcile",
		Short: "Classify interrupted workspace and run effects",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command, []string) (any, error) {
			caller, err := a.identity()
			if err != nil {
				return nil, err
			}
			if caller.Kind == "run" {
				return nil, fault.New("not_granted", "reconciliation is for drivers and the operator")
			}
			x, err := a.executor()
			if err != nil {
				return nil, err
			}
			return x.Reconcile(a.ctx)
		}),
	})
	return cmd
}

func hostCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "host", Short: "Hosts that run sessions"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List hosts with reachability, version and installed providers",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command, []string) (any, error) {
			if _, err := a.identity(); err != nil {
				return nil, err
			}
			cfg, err := a.config()
			if err != nil {
				return nil, err
			}
			x, err := a.executor()
			if err != nil {
				return nil, err
			}
			names := []string{cfg.Host}
			for name := range cfg.Hosts {
				names = append(names, name)
			}
			slices.Sort(names[1:])
			hosts := []map[string]any{}
			for _, name := range names {
				entry := map[string]any{"name": name, "home": name == cfg.Host, "reachable": false}
				if _, info, err := x.HostInfo(a.ctx, name); err != nil {
					entry["error"] = fault.As(err)
				} else {
					entry["reachable"], entry["version"], entry["protocol"] = true, info.Version, info.Protocol
					entry["harnesses"], entry["presentations"] = info.Harnesses, info.Presentations
				}
				hosts = append(hosts, entry)
			}
			return map[string]any{"hosts": hosts}, nil
		}),
	})
	return cmd
}

func skillCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "skill", Short: "Print a role's guidance"}
	for _, role := range guide.Roles {
		var section string
		sub := &cobra.Command{Use: role, Short: "Print the " + role + " guidance as configured", Args: cobra.NoArgs}
		sub.Flags().StringVar(&section, "section", "", "print one reference section")
		sub.RunE = a.run(func(*cobra.Command, []string) (any, error) {
			cfg, err := a.config()
			if fault.As(err).Kind == "not_initialized" {
				cfg, err = &config.Config{}, nil
			}
			if err != nil {
				return nil, err
			}
			text, sections, err := guide.Document(cfg, role)
			if err != nil {
				return nil, err
			}
			if section == "" {
				return map[string]any{"role": role, "text": text}, nil
			}
			body, ok := sections[section]
			if !ok {
				return nil, fault.New("not_found", "the %s guidance has no section %q", role, section).WithNext("skill", role)
			}
			return map[string]any{"role": role, "section": section, "text": body}, nil
		})
		cmd.AddCommand(sub)
	}
	return cmd
}

// requireCurrentRun refuses input from a stale run and records it.
func (a *app) requireCurrentRun(caller Caller, command string) error {
	if caller.Kind != "run" {
		return nil
	}
	db, err := a.store()
	if err != nil {
		return err
	}
	err = coord.RequireCurrent(db, caller)
	if fault.As(err).Kind != "stale_run" {
		return err
	}
	if werr := db.Write(a.ctx, func(tx *store.Tx) error { return coord.RecordStale(tx, caller, command) }); werr != nil {
		return werr
	}
	return err
}

// turnBudget warns a sub-driver run, once, that its turn has run long.
func (a *app) turnBudget(caller Caller, r *coord.Run) {
	if r.WarnedLong || r.Liveness == "exited" {
		return
	}
	cfg, err := a.config()
	if err != nil {
		return
	}
	started, err := time.Parse("2006-01-02T15:04:05.000000000Z", r.Started)
	if err != nil || time.Since(started) < cfg.Timeouts.TurnBudget.Duration {
		return
	}
	db, err := a.store()
	if err != nil {
		return
	}
	var role string
	if db.QueryRowContext(a.ctx, `SELECT role FROM tasks WHERE id = ?`, caller.Task).Scan(&role) != nil || role != "driver" {
		return
	}
	if _, err := db.ExecContext(a.ctx, `UPDATE runs SET warned_long = 1 WHERE id = ?`, r.ID); err != nil {
		return
	}
	a.warnings = append(a.warnings, Warning{Kind: "turn_long", Message: "this turn has run longer than " +
		cfg.Timeouts.TurnBudget.String() + "; delegate the remaining work to workers, report, and end the turn"})
}
