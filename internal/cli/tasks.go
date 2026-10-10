package cli

import (
	"encoding/json"
	"strings"

	"github.com/spf13/cobra"

	"shephrd/internal/artifact"
	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

func taskCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "Create, read and act on delegated tasks"}
	cmd.AddCommand(taskCreate(a), taskShow(a), taskList(a), taskSend(a), taskCancel(a), taskAdopt(a), taskNote(a), taskData(a))
	cmd.AddCommand(runCommands(a)...)
	cmd.AddCommand(deliveryCommands(a)...)
	return cmd
}

// readTask resolves a task the caller may read, before any transaction.
func (a *app) readTask(ref string) (Caller, *store.Store, *coord.Task, error) {
	caller, err := a.identity()
	if err != nil {
		return Caller{}, nil, nil, err
	}
	db, err := a.store()
	if err != nil {
		return Caller{}, nil, nil, err
	}
	t, err := coord.Get(db, caller, ref)
	return caller, db, t, err
}

func taskCreate(a *app) *cobra.Command {
	var spec coord.NewTask
	var target config.Target
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a root task, or a child task from a driver run",
		Args:  cobra.NoArgs,
	}
	f := cmd.Flags()
	f.StringVar(&spec.Role, "role", "", "worker (default) does the work; driver may delegate it")
	f.StringVar(&spec.Repo, "repo", "", "repository name; required for a worker")
	f.StringVar(&spec.Title, "title", "", "display title (default: from the objective)")
	f.StringVar(&spec.Objective, "objective", "", "the outcome wanted, or - for stdin")
	f.StringVar(&spec.Acceptance, "acceptance", "", "acceptance criteria, or - for stdin")
	f.StringVar(&spec.Deliverable, "deliverable", "", "code or report for a worker, answer or report for a driver")
	f.StringArrayVar(&spec.After, "after", nil, "a sibling task this one waits on; repeatable")
	f.StringVar(&spec.Until, "until", "", "what dependencies wait for: published (default) or merged")
	f.Int64Var(&spec.Request, "request", 0, "the parent's request this child serves")
	f.StringVar(&target.Host, "host", "", "host to run on")
	f.StringVar(&target.Harness, "harness", "", "harness to run")
	f.StringVar(&target.Model, "model", "", "model, passed to the harness unchanged")
	cmd.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		var err error
		if spec.Objective, err = a.text("objective", spec.Objective, coord.MaxObjective); err != nil {
			return nil, err
		}
		if spec.Acceptance, err = a.text("acceptance", spec.Acceptance, coord.MaxAcceptance); err != nil {
			return nil, err
		}
		caller, err := a.identity()
		if err != nil {
			return nil, err
		}
		cfg, err := a.config()
		if err != nil {
			return nil, err
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		p, err := coord.Propose(db, cfg, caller, spec)
		if err != nil {
			return nil, err
		}
		resolved, err := a.route(p, target)
		if err != nil {
			return nil, err
		}
		parent := ""
		if p.Parent != nil {
			parent = p.Parent.Ref
		}
		if err := a.gate("task.create", 0, map[string]any{
			"caller": caller.String(), "parent": parent, "role": p.Role, "repo": p.Repo, "title": p.Title,
			"objective": p.Objective, "deliverable": p.Deliverable, "after": spec.After, "target": resolved,
		}); err != nil {
			return nil, err
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			p, err := coord.Propose(tx, cfg, caller, spec)
			if err != nil {
				return nil, err
			}
			return coord.Create(tx, caller, p, resolved)
		})
	})
	return cmd
}

func taskShow(a *app) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "show <task>",
		Short: "Read a task with its dependencies, children, requests and log",
		Args:  cobra.ExactArgs(1),
	}
	cmd.Flags().IntVar(&limit, "events", 50, "how many of the latest log events to include")
	cmd.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		_, db, t, err := a.readTask(args[0])
		if err != nil {
			return nil, err
		}
		deps, ready, err := coord.Dependencies(db, t.ID)
		if err != nil {
			return nil, err
		}
		children, err := coord.Query(db, `t.parent = ?`, t.ID)
		if err != nil {
			return nil, err
		}
		for i, child := range children {
			children[i] = child.Summary()
		}
		requests, err := coord.Requests(db, t.ID)
		if err != nil {
			return nil, err
		}
		events, err := db.TaskEvents(a.ctx, t.ID, limit)
		if err != nil {
			return nil, err
		}
		data, err := coord.PluginData(db, t.ID)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"task": t, "dependencies": deps, "children": children, "events": events, "data": data}
		if t.ArtifactID != 0 {
			if out["artifact"], err = artifact.Load(db, t.ArtifactID); err != nil {
				return nil, err
			}
		}
		if t.State == "queued" {
			out["ready"] = ready
		}
		if t.Role == "driver" {
			out["requests"] = requests
		}
		return out, nil
	})
	return cmd
}

func taskList(a *app) *cobra.Command {
	var parent, role, repo, state string
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your tasks, or a subtree, with filters",
		Args:  cobra.NoArgs,
	}
	f := cmd.Flags()
	f.StringVar(&parent, "parent", "", "list this task's children instead of your own tasks")
	f.StringVar(&role, "role", "", "only worker or driver tasks")
	f.StringVar(&repo, "repo", "", "only tasks in this repository")
	f.StringVar(&state, "state", "", "only tasks in this state")
	f.BoolVar(&all, "all", false, "include every descendant, not only the top level")
	cmd.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		caller, err := a.identity()
		if err != nil {
			return nil, err
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		var base string
		var args []any
		switch {
		case parent != "":
			_, _, p, err := a.readTask(parent)
			if err != nil {
				return nil, err
			}
			base, args = `parent = ?`, []any{p.ID}
		case caller.Kind == "run":
			base, args = `parent = ?`, []any{caller.Task}
		case caller.Kind == "driver" || caller.Kind == "plugin":
			base, args = `driver = ?`, []any{caller.String()}
		default:
			base = `parent IS NULL`
		}
		scope := `SELECT id FROM tasks WHERE ` + base
		if all {
			scope = `WITH RECURSIVE sub(id) AS (` + scope + ` UNION SELECT c.id FROM tasks c JOIN sub ON c.parent = sub.id) SELECT id FROM sub`
		}
		where := []string{`t.id IN (` + scope + `)`}
		for column, value := range map[string]string{"t.role": role, "r.name": repo, "t.state": state} {
			if value != "" {
				where = append(where, column+` = ?`)
				args = append(args, value)
			}
		}
		tasks, err := coord.Query(db, strings.Join(where, " AND "), args...)
		if err != nil {
			return nil, err
		}
		for i, t := range tasks {
			tasks[i] = t.Summary()
		}
		return map[string]any{"tasks": tasks}, nil
	})
	return cmd
}

func taskSend(a *app) *cobra.Command {
	var replyTo, revision int64
	cmd := &cobra.Command{
		Use:   "send <task> <text|->",
		Short: "Send a message, or with --reply-to a reply, to a task you own",
		Args:  cobra.ExactArgs(2),
	}
	cmd.Flags().Int64Var(&replyTo, "reply-to", 0, "the question this answers")
	cmd.Flags().Int64Var(&revision, "if-revision", 0, "refuse unless the task is at this revision")
	cmd.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		body, err := a.text("message", args[1], coord.MaxMessage)
		if err != nil {
			return nil, err
		}
		caller, db, t, err := a.readTask(args[0])
		if err != nil {
			return nil, err
		}
		if _, err := coord.GetOwned(db, caller, args[0]); err != nil {
			return nil, err
		}
		if err := a.gate("task.send", t.ID, map[string]any{"caller": caller.String(), "task": t.Ref, "body": body, "reply_to": replyTo}); err != nil {
			return nil, err
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			t, err := coord.GetOwned(tx, caller, args[0])
			if err != nil {
				return nil, err
			}
			if err := coord.CheckRevision(t, revision); err != nil {
				return nil, err
			}
			seq, err := coord.Send(tx, caller, t, body, replyTo)
			if err != nil {
				return nil, err
			}
			return map[string]any{"task": t.Ref, "seq": seq}, nil
		})
	})
	return cmd
}

func taskCancel(a *app) *cobra.Command {
	var revision int64
	cmd := &cobra.Command{
		Use:   "cancel <task>",
		Short: "Close a task that never started",
		Args:  cobra.ExactArgs(1),
	}
	cmd.Flags().Int64Var(&revision, "if-revision", 0, "refuse unless the task is at this revision")
	cmd.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		caller, db, t, err := a.readTask(args[0])
		if err != nil {
			return nil, err
		}
		if _, err := coord.GetOwned(db, caller, args[0]); err != nil {
			return nil, err
		}
		if err := a.gate("task.cancel", t.ID, map[string]any{"caller": caller.String(), "task": t.Ref}); err != nil {
			return nil, err
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			t, err := coord.GetOwned(tx, caller, args[0])
			if err != nil {
				return nil, err
			}
			if err := coord.CheckRevision(t, revision); err != nil {
				return nil, err
			}
			return t, coord.Cancel(tx, caller, t)
		})
	})
	return cmd
}

func taskAdopt(a *app) *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "adopt <task>",
		Short: "Move a root task to another driver",
		Args:  cobra.ExactArgs(1),
	}
	cmd.Flags().StringVar(&to, "to", "", "the new owner, driver:<name> or plugin:<name> (default: you)")
	cmd.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		caller, _, t, err := a.readTask(args[0])
		if err != nil {
			return nil, err
		}
		if to == "" {
			to = caller.String()
		}
		if err := a.gate("task.adopt", t.ID, map[string]any{"caller": caller.String(), "task": t.Ref, "from": t.Driver, "to": to}); err != nil {
			return nil, err
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			t, err := coord.Get(tx, caller, args[0])
			if err != nil {
				return nil, err
			}
			return t, coord.Adopt(tx, caller, t, to)
		})
	})
	return cmd
}

func taskNote(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "note <task> <text|->",
		Short: "Record an owner's note on a task",
		Args:  cobra.ExactArgs(2),
	}
	cmd.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		body, err := a.text("note", args[1], coord.MaxNote)
		if err != nil {
			return nil, err
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			t, err := coord.GetOwned(tx, caller, args[0])
			if err != nil {
				return nil, err
			}
			seq, err := coord.Note(tx, caller, t, body)
			return map[string]any{"task": t.Ref, "seq": seq}, err
		})
	})
	return cmd
}

func taskData(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "data", Short: "Plugin data on tasks"}
	var unset []string
	var pluginName string
	set := &cobra.Command{
		Use:   "set <task> [key=value]...",
		Short: "Write keys in your plugin's data namespace on a task",
		Args:  cobra.MinimumNArgs(1),
	}
	set.Flags().StringArrayVar(&unset, "unset", nil, "remove a key; repeatable")
	set.Flags().StringVar(&pluginName, "plugin", "", "the namespace to write (operator only; plugins write their own)")
	set.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		caller, err := a.identity()
		if err != nil {
			return nil, err
		}
		namespace := pluginName
		switch {
		case caller.Kind == "plugin" && (pluginName == "" || pluginName == caller.Name):
			namespace = caller.Name
		case caller.Kind == "plugin":
			return nil, fault.New("not_owner", "plugin %s cannot write plugin %s's data", caller.Name, pluginName)
		case !caller.Operator:
			return nil, fault.New("not_owner", "only a plugin or the operator writes plugin data")
		case pluginName == "":
			return nil, fault.New("usage", "the operator names the namespace with --plugin")
		}
		values := map[string]json.RawMessage{}
		for _, pair := range args[1:] {
			key, value, ok := strings.Cut(pair, "=")
			if !ok || key == "" {
				return nil, fault.New("usage", "%q is not key=value", pair)
			}
			if !json.Valid([]byte(value)) {
				encoded, _ := json.Marshal(value)
				value = string(encoded)
			}
			values[key] = json.RawMessage(value)
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			t, err := coord.Get(tx, caller, args[0])
			if err != nil {
				return nil, err
			}
			data, err := coord.SetData(tx, caller, t, namespace, values, unset)
			return map[string]any{"task": t.Ref, "plugin": namespace, "data": data}, err
		})
	})
	cmd.AddCommand(set)
	return cmd
}
