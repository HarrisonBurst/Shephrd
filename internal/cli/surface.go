package cli

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
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
	cmd.AddCommand(submit, replies)
	return cmd
}

func mainMailboxCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "main", Short: "Inspect surface input and record operator replies"}
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
			return coord.ReplySurface(tx, id, body)
		})
	})
	cmd.AddCommand(pending, reply)
	return cmd
}
