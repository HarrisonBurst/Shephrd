package cli

import (
	"github.com/spf13/cobra"

	"shephrd/internal/artifact"
	"shephrd/internal/coord"
	"shephrd/internal/execution"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

func (a *app) artifacts() (*artifact.Service, error) {
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
	return &artifact.Service{DB: db, Cfg: cfg, Host: x.Host, Forge: artifact.Forges(reg, a.getenv)}, nil
}

// seal checks a run's result against its deliverable before it is recorded.
func (a *app) seal(caller Caller, t *coord.Task, body string, files []string) (*artifact.Artifact, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}
	db, err := a.store()
	if err != nil {
		return nil, err
	}
	attempt, err := coord.LoadAttempt(db, caller.Task, caller.Attempt)
	if err != nil {
		return nil, err
	}
	host, err := execution.Hosts(cfg, a.getenv)(attempt.Host)
	if err != nil {
		return nil, err
	}
	return artifact.Seal(a.ctx, cfg, host, t, attempt, body, files)
}

func deliveryCommands(a *app) []*cobra.Command {
	deliver := &cobra.Command{Use: "deliver <task>", Short: "Accept a done task's result, or land its code", Args: cobra.ExactArgs(1)}
	deliver.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		caller, db, t, err := a.readTask(args[0])
		if err != nil {
			return nil, err
		}
		if t, err = coord.GetOwned(db, caller, args[0]); err != nil {
			return nil, err
		}
		return a.once(func(caller Caller) (any, error) {
			if err := a.gate("task.deliver", t.ID, map[string]any{"caller": caller.String(), "task": t.Ref, "deliverable": t.Deliverable, "artifact": t.Artifact}); err != nil {
				return nil, err
			}
			s, err := a.artifacts()
			if err != nil {
				return nil, err
			}
			out, err := s.Deliver(a.ctx, caller, t)
			if err != nil {
				return nil, err
			}
			for _, w := range out.Warnings {
				a.warnings = append(a.warnings, Warning{Kind: "checkout_behind", Message: w})
			}
			return out, nil
		})
	})
	verify := &cobra.Command{Use: "verify <task>", Short: "Re-check landing proof for code merged outside Shephrd", Args: cobra.ExactArgs(1)}
	verify.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		_, _, t, err := a.readTask(args[0])
		if err != nil {
			return nil, err
		}
		return a.once(func(caller Caller) (any, error) {
			s, err := a.artifacts()
			if err != nil {
				return nil, err
			}
			return s.Verify(a.ctx, caller, t)
		})
	})
	var attempt int64
	discard := &cobra.Command{Use: "discard <task>", Short: "Close a task as discarded and remove its workspaces", Args: cobra.ExactArgs(1)}
	discard.Flags().Int64Var(&attempt, "attempt", 0, "remove only this attempt's workspace")
	discard.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		caller, db, t, err := a.readTask(args[0])
		if err != nil {
			return nil, err
		}
		if t, err = coord.GetOwned(db, caller, args[0]); err != nil {
			return nil, err
		}
		return a.once(func(caller Caller) (any, error) {
			if err := a.gate("task.discard", t.ID, map[string]any{"caller": caller.String(), "task": t.Ref, "attempt": attempt}); err != nil {
				return nil, err
			}
			s, err := a.artifacts()
			if err != nil {
				return nil, err
			}
			return s.Discard(a.ctx, caller, t, attempt)
		})
	})
	return []*cobra.Command{deliver, verify, discard}
}

func grantCommand(a *app) *cobra.Command {
	var to, reason string
	cmd := &cobra.Command{Use: "grant <land|discard>", Short: "Delegate land or discard authority to a child driver task", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&to, "to", "", "the child driver task whose subtree receives the grant")
	cmd.Flags().StringVar(&reason, "reason", "", "why, such as the user's approval, or - for stdin")
	cmd.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		reason, err := a.text("reason", reason, 4096)
		if err != nil {
			return nil, err
		}
		if to == "" {
			return nil, fault.New("usage", "name the child task with --to")
		}
		caller, db, t, err := a.readTask(to)
		if err != nil {
			return nil, err
		}
		if _, err := coord.GetOwned(db, caller, to); err != nil {
			return nil, err
		}
		if err := a.gate("grant.add", t.ID, map[string]any{"caller": caller.String(), "action": args[0], "to": t.Ref, "reason": reason}); err != nil {
			return nil, err
		}
		cfg, err := a.config()
		if err != nil {
			return nil, err
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			t, err := coord.GetOwned(tx, caller, to)
			if err != nil {
				return nil, err
			}
			return artifact.Delegate(tx, cfg, caller, args[0], t, reason)
		})
	})
	revoke := &cobra.Command{Use: "revoke <grant>", Short: "Stop future uses of a grant you made", Args: cobra.ExactArgs(1)}
	revoke.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			return artifact.Revoke(tx, caller, args[0])
		})
	})
	cmd.AddCommand(revoke)
	return cmd
}

func artifactCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "artifact", Short: "Read sealed results"}
	load := func(ref string) (*artifact.Artifact, error) {
		id, err := artifact.ParseRef(ref)
		if err != nil {
			return nil, err
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		art, err := artifact.Load(db, id)
		if err != nil {
			return nil, err
		}
		if _, _, _, err := a.readTask(art.Task); err != nil {
			return nil, fault.New("not_found", "no artifact %s is visible to this caller", ref)
		}
		return art, nil
	}
	show := &cobra.Command{Use: "show <artifact>", Short: "Print an artifact's metadata", Args: cobra.ExactArgs(1)}
	show.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		return load(args[0])
	})
	var file string
	read := &cobra.Command{Use: "read <artifact>", Short: "Print an artifact's content: a report file or the answer", Args: cobra.ExactArgs(1)}
	read.Flags().StringVar(&file, "file", "", "which report file, when there are several")
	read.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		art, err := load(args[0])
		if err != nil {
			return nil, err
		}
		cfg, err := a.config()
		if err != nil {
			return nil, err
		}
		content, err := artifact.Read(cfg, art, file)
		return map[string]any{"artifact": art.Ref, "file": file, "content": content}, err
	})
	cmd.AddCommand(show, read)
	return cmd
}
