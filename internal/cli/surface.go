package cli

import (
	"database/sql"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/execution"
	"shephrd/internal/fault"
	"shephrd/internal/maincoord"
	"shephrd/internal/store"
)

func (a *app) surfaceCaller() (Caller, error) {
	caller, err := a.identity()
	if err != nil {
		return Caller{}, err
	}
	if caller.Kind != "surface" {
		return Caller{}, fault.New("surface_only", "this command requires a surface identity")
	}
	return caller, nil
}

func surfaceThread(thread string) error {
	if !config.NamePattern.MatchString(thread) {
		return fault.New("usage", "thread must match %s", config.NamePattern)
	}
	return nil
}

func surfaceCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "surface", Short: "Submit surface input and read scoped replies"}
	var submitThread, replyThread string
	submit := &cobra.Command{Use: "submit <text|->", Args: cobra.ExactArgs(1)}
	submit.Flags().StringVar(&submitThread, "thread", "", "conversation thread")
	submit.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		if _, err := a.surfaceCaller(); err != nil {
			return nil, err
		}
		if err := surfaceThread(submitThread); err != nil {
			return nil, err
		}
		body, err := a.text("input", args[0], 8192)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(body) == "" {
			return nil, fault.New("usage", "input cannot be empty")
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			return coord.SubmitSurface(tx, caller.Name, submitThread, body)
		})
	})
	replies := &cobra.Command{Use: "replies", Args: cobra.NoArgs}
	var after int64
	replies.Flags().StringVar(&replyThread, "thread", "", "conversation thread")
	replies.Flags().Int64Var(&after, "after", 0, "reply cursor")
	replies.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		caller, err := a.surfaceCaller()
		if err != nil {
			return nil, err
		}
		if err := surfaceThread(replyThread); err != nil {
			return nil, err
		}
		if after < 0 {
			return nil, fault.New("usage", "after must be nonnegative")
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		items, err := coord.SurfaceReplies(db, caller.Name, replyThread, after)
		return map[string]any{"replies": items}, err
	})
	var ackThread string
	ack := &cobra.Command{Use: "ack <cursor>", Args: cobra.ExactArgs(1)}
	ack.Flags().StringVar(&ackThread, "thread", "", "conversation thread")
	ack.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		if _, err := a.surfaceCaller(); err != nil {
			return nil, err
		}
		if err := surfaceThread(ackThread); err != nil {
			return nil, err
		}
		cursor, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || cursor <= 0 {
			return nil, fault.New("usage", "cursor must be positive")
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			stored, err := coord.AckSurface(tx, caller.Name, ackThread, cursor)
			return map[string]any{"cursor": stored}, err
		})
	})
	cmd.AddCommand(submit, replies, ack)
	return cmd
}

func mainMailboxCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "main", Short: "Operate the home-host main coordinator and its mailbox"}
	start := &cobra.Command{Use: "start", Args: cobra.NoArgs}
	start.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		if err := a.requireOperator(); err != nil {
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
		return maincoord.Start(a.ctx, db, cfg)
	})
	status := &cobra.Command{Use: "status", Args: cobra.NoArgs}
	status.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		if err := a.requireOperator(); err != nil {
			return nil, err
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		state, err := maincoord.Load(db)
		if err == sql.ErrNoRows {
			return map[string]any{"state": "disabled"}, nil
		}
		if err != nil {
			return nil, err
		}
		cfg, err := a.config()
		if err != nil {
			return nil, err
		}
		if state.Turn > 0 {
			state.Log = execution.LogPath(maincoord.TurnDir(cfg, state.Turn))
		}
		return state, nil
	})
	var evidence string
	recoverCmd := &cobra.Command{Use: "recover", Args: cobra.NoArgs}
	recoverCmd.Flags().StringVar(&evidence, "evidence", "", "independent evidence that the turn was handled and its process has exited")
	recoverCmd.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		if err := a.requireOperator(); err != nil {
			return nil, err
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		return maincoord.Recover(a.ctx, db, evidence)
	})
	pending := &cobra.Command{Use: "pending", Args: cobra.NoArgs}
	var after int64
	pending.Flags().Int64Var(&after, "after", 0, "input cursor")
	pending.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		if err := a.requireOperator(); err != nil {
			return nil, err
		}
		if after < 0 {
			return nil, fault.New("usage", "after must be nonnegative")
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		items, err := coord.PendingSurface(db, after)
		return map[string]any{"inputs": items}, err
	})
	reply := &cobra.Command{Use: "reply <input-id> <text|->", Args: cobra.ExactArgs(2)}
	reply.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		if err := a.requireOperator(); err != nil {
			return nil, err
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return nil, fault.New("usage", "input-id must be positive")
		}
		body, err := a.text("reply", args[1], 8192)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(body) == "" {
			return nil, fault.New("usage", "reply cannot be empty")
		}
		return a.mutate(func(tx *store.Tx, _ Caller) (any, error) {
			state, err := maincoord.Load(tx)
			if err != nil && err != sql.ErrNoRows {
				return nil, err
			}
			if err == nil && (state.State == "starting" || state.State == "running") {
				return nil, fault.New("turn_active", "cannot reply while the coordinator turn is active")
			}
			return coord.ReplySurface(tx, id, body)
		})
	})
	cmd.AddCommand(pending, reply, start, status, recoverCmd)
	return cmd
}
