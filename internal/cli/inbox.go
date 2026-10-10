package cli

import (
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"shephrd/internal/coord"
	"shephrd/internal/daemon"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

func (a *app) inboxOwner() (Caller, *store.Store, error) {
	caller, err := a.identity()
	if err != nil {
		return Caller{}, nil, err
	}
	if caller.Kind != "driver" && caller.Kind != "plugin" {
		return Caller{}, nil, fault.New("no_inbox", "only a driver or plugin has an inbox; a sub-driver learns about its children in its brief")
	}
	db, err := a.store()
	return caller, db, err
}

func inboxCommand(a *app) *cobra.Command {
	var all bool
	cmd := &cobra.Command{Use: "inbox", Short: "List inbox items that need your attention", Args: cobra.NoArgs}
	cmd.Flags().BoolVar(&all, "all", false, "include acknowledged items")
	cmd.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		caller, db, err := a.inboxOwner()
		if err != nil {
			return nil, err
		}
		where := `i.driver = ? AND i.state = 'pending'`
		if all {
			where = `i.driver = ?`
		}
		items, err := coord.Items(db, where, caller.String())
		return map[string]any{"items": items}, err
	})
	ack := &cobra.Command{Use: "ack <item>", Short: "Acknowledge an item as handled", Args: cobra.ExactArgs(1)}
	ack.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return nil, fault.New("usage", "%q is not an inbox item number", args[0])
		}
		if _, _, err := a.inboxOwner(); err != nil {
			return nil, err
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			return coord.AckItem(tx, caller, id)
		})
	})
	var after int64
	wait := &cobra.Command{Use: "wait", Short: "Stream new inbox items as JSON lines, resuming after --after", Args: cobra.NoArgs}
	wait.Flags().Int64Var(&after, "after", 0, "stream items with a number above this")
	wait.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		caller, db, err := a.inboxOwner()
		if err != nil {
			return nil, err
		}
		a.streamed = true
		encoder := json.NewEncoder(a.stream)
		for {
			items, err := coord.Items(db, `i.driver = ? AND i.state = 'pending' AND i.id > ?`, caller.String(), after)
			if a.ctx.Err() != nil {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				if err := encoder.Encode(item); err != nil {
					return nil, nil
				}
				after = item.ID
			}
			select {
			case <-a.ctx.Done():
				return nil, nil
			case <-time.After(250 * time.Millisecond):
			}
		}
	})
	cmd.AddCommand(ack, wait)
	return cmd
}

func daemonCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Wake owners, push inbox items, deliver plugin events and reconcile, on the home host",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command, []string) (any, error) {
			if err := a.requireOperator(); err != nil {
				return nil, err
			}
			d := &daemon.Daemon{Getenv: a.getenv, Reserved: a.reserved, Log: os.Stderr}
			if err := d.Run(a.ctx); err != nil {
				return nil, err
			}
			return map[string]any{"stopped": true}, nil
		}),
	}
}

// daemonWarning tells every caller when nothing will be pushed or woken.
func (a *app) daemonWarning() {
	if a.cfg == nil || a.db == nil || len(a.req.Argv) == 0 {
		return
	}
	switch a.req.Argv[0] {
	case "daemon", "_started", "_exited":
		return
	}
	if !daemon.Running(a.cfg) {
		a.warnings = append(a.warnings, Warning{Kind: "daemon_not_running",
			Message: "shephrd daemon is not running on the home host: nothing is pushed and no sub-driver is woken until it starts"})
	}
}
