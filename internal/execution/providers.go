package execution

import (
	"context"
	"encoding/json"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/plugin"
	"shephrd/internal/store"
)

// Harnesses resolves a harness name to a built-in or a plugin provider.
func Harnesses(reg *plugin.Registry, getenv config.Getenv) func(context.Context, string, HarnessRequest) (HarnessCommand, error) {
	return func(ctx context.Context, name string, req HarnessRequest) (HarnessCommand, error) {
		if build, ok := Builtin[name]; ok {
			return build(req), nil
		}
		provider, err := CheckHarness(reg, name)
		if err != nil {
			return HarnessCommand{}, err
		}
		out, err := reg.Call(ctx, provider, plugin.Call{Kind: "provide", Type: "harness", Body: req, Getenv: getenv})
		if err != nil {
			return HarnessCommand{}, fault.New("plugin_failed", "harness %s: %v", name, err)
		}
		var command HarnessCommand
		if err := json.Unmarshal(out, &command); err != nil {
			return HarnessCommand{}, fault.New("plugin_failed", "harness %s answered invalidly: %v", name, err)
		}
		return command, nil
	}
}

func CheckHarness(reg *plugin.Registry, name string) (*plugin.Plugin, error) {
	if _, ok := Builtin[name]; ok {
		return nil, nil
	}
	provider := reg.Provider("harness", name)
	if provider == nil {
		return nil, fault.New("unknown_harness", "harness %q is neither built in nor provided by a declared plugin", name)
	}
	return provider, nil
}

// Interrupt stops a live run Shephrd decided to end: a worker whose turn
// was reported but whose process lingers, or a run inactive for too long.
// An inactive run gets the one nudge; an inactive nudge holds the task.
func (x *Executor) Interrupt(ctx context.Context, r *coord.Run, reason string) error {
	if err := x.write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE runs SET stop_reason = ? WHERE id = ?`, reason, r.ID)
		return err
	}); err != nil {
		return err
	}
	r.StopReason = reason
	if !x.stopGroup(ctx, r) {
		return x.write(ctx, func(tx *store.Tx) error { return coord.SetLiveness(tx, r, "unknown") })
	}
	if _, err := x.run(ctx, r.Task, "nudge", func(tx *store.Tx, t *coord.Task) (bool, error) {
		if _, err := coord.RecordExit(tx, r, -1, ""); err != nil {
			return false, err
		}
		switch {
		case reason == "turn_ended" && t.Role == "driver" && t.State == "running":
			return false, coord.SettleDriver(tx, t)
		case reason != "inactive" || (t.State != "running" && t.State != "waiting"):
			return false, nil
		case r.Purpose == "nudge":
			return false, coord.Hold(tx, t, "inactive")
		}
		return true, nil
	}); err != nil {
		return err
	}
	return nil
}

// run applies a decision in one transaction and starts the follow-up run
// it asks for.
func (x *Executor) run(ctx context.Context, taskID int64, purpose string, decide func(*store.Tx, *coord.Task) (bool, error)) (*Started, error) {
	var start bool
	err := x.write(ctx, func(tx *store.Tx) error {
		t, err := coord.Load(tx, taskID)
		if err != nil {
			return err
		}
		start, err = decide(tx, t)
		return err
	})
	if err != nil || !start {
		return nil, err
	}
	return x.Start(ctx, "system", taskID, purpose, nil)
}
