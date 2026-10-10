package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/plugin"
	"shephrd/internal/store"
)

func (a *app) pluginCommand(name string, args []string) (json.RawMessage, error) {
	reg, err := a.registry()
	if err != nil {
		return nil, err
	}
	p := reg.Get(name)
	if p == nil || (p.Manifest != nil && p.Manifest.Commands[name] == "") {
		return nil, fault.New("plugin_unknown", "no core command or declared plugin provides %q", name).WithNext("help")
	}
	if p.Unavailable != "" {
		return nil, fault.New("plugin_failed", "plugin %s is unavailable: %s", name, p.Unavailable).WithNext("plugin", "status")
	}
	caller, err := a.identity()
	if err != nil {
		return nil, err
	}
	stdin := ""
	if slices.Contains(args, "-") {
		if stdin, err = a.text("stdin", "-", 1<<20); err != nil {
			return nil, err
		}
	}
	timeout := plugin.Timeouts["command"]
	token, revoke, err := a.issueToken(name, &caller, timeout+time.Minute)
	if err != nil {
		return nil, err
	}
	defer revoke()
	out, err := reg.Call(a.ctx, p, plugin.Call{
		Kind:   "command",
		Body:   map[string]any{"argv": args, "stdin": stdin, "caller": caller},
		Token:  token,
		Getenv: a.getenv,
	})
	if err != nil {
		return nil, fault.New("plugin_failed", "plugin %s: %v", name, err)
	}
	var failure struct {
		Error *fault.Error `json:"error"`
	}
	if err := json.Unmarshal(out, &failure); err == nil && failure.Error != nil {
		if failure.Error.Kind == "" {
			failure.Error.Kind = "plugin_error"
		}
		return nil, failure.Error
	}
	return out, nil
}

func pluginCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "plugin", Short: "Fetch declared packages and inspect declared plugins"}
	list := &cobra.Command{
		Use:   "list",
		Short: "List declared plugins and what they provide",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command, []string) (any, error) {
			if _, err := a.identity(); err != nil {
				return nil, err
			}
			reg, err := a.registry()
			if err != nil {
				return nil, err
			}
			return map[string]any{"plugins": reg.All()}, nil
		}),
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show each declared plugin's availability and log",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command, []string) (any, error) {
			if _, err := a.identity(); err != nil {
				return nil, err
			}
			reg, err := a.registry()
			if err != nil {
				return nil, err
			}
			type row struct {
				Name        string `json:"name"`
				Package     string `json:"package"`
				Available   bool   `json:"available"`
				Unavailable string `json:"unavailable,omitempty"`
				Log         string `json:"log"`
			}
			rows := []row{}
			for _, p := range reg.All() {
				rows = append(rows, row{Name: p.Name, Package: p.Package, Available: p.Unavailable == "", Unavailable: p.Unavailable, Log: reg.LogPath(p.Name)})
			}
			return map[string]any{"plugins": rows}, nil
		}),
	}
	skill := &cobra.Command{
		Use:   "skill <plugin>",
		Short: "Print the skills a plugin ships",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(_ *cobra.Command, args []string) (any, error) {
			if _, err := a.identity(); err != nil {
				return nil, err
			}
			reg, err := a.registry()
			if err != nil {
				return nil, err
			}
			p := reg.Get(args[0])
			if p == nil {
				return nil, fault.New("plugin_unknown", "no declared plugin is named %q", args[0]).WithNext("plugin", "list")
			}
			if p.Unavailable != "" {
				return nil, fault.New("plugin_failed", "plugin %s is unavailable: %s", p.Name, p.Unavailable).WithNext("plugin", "status")
			}
			type skillFile struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			skills := []skillFile{}
			for _, path := range p.Manifest.Skills {
				body, err := os.ReadFile(filepath.Join(p.Dir, path))
				if err != nil {
					return nil, err
				}
				skills = append(skills, skillFile{Path: path, Content: string(body)})
			}
			return map[string]any{"plugin": p.Name, "skills": skills}, nil
		}),
	}
	sync := &cobra.Command{
		Use:   "sync",
		Short: "Fetch declared git packages at their pinned commits and remove undeclared ones (operator only)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command, []string) (any, error) {
			if err := a.requireOperator(); err != nil {
				return nil, err
			}
			cfg, err := a.config()
			if err != nil {
				return nil, err
			}
			statuses, err := plugin.Sync(cfg)
			if err != nil {
				return nil, err
			}
			return map[string]any{"packages": statuses}, nil
		}),
	}
	cmd.AddCommand(list, status, skill, sync)
	return cmd
}

func eventsCommand(a *app) *cobra.Command {
	var after int64
	var limit int
	var follow bool
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Read the public event stream from --after <seq>, or --follow it",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().Int64Var(&after, "after", 0, "return events after this sequence number")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum events to return, without --follow")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream events as JSON lines until interrupted")
	cmd.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		caller, err := a.identity()
		if err != nil {
			return nil, err
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		if limit < 1 || limit > 1000 {
			return nil, fault.New("usage", "--limit must be between 1 and 1000")
		}
		if !follow {
			events, err := a.visibleEvents(db, caller, after, limit)
			if err != nil {
				return nil, err
			}
			next := after
			if len(events) > 0 {
				next = events[len(events)-1].Seq
			}
			return map[string]any{"events": events, "next": next}, nil
		}
		a.streamed = true
		encoder := json.NewEncoder(a.stream)
		for {
			events, err := a.visibleEvents(db, caller, after, 1000)
			if a.ctx.Err() != nil {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			for _, event := range events {
				if err := encoder.Encode(event); err != nil {
					return nil, nil
				}
				after = event.Seq
			}
			if len(events) > 0 {
				continue
			}
			select {
			case <-a.ctx.Done():
				return nil, nil
			case <-time.After(200 * time.Millisecond):
			}
		}
	})
	return cmd
}

func (a *app) visibleEvents(db *store.Store, caller Caller, after int64, limit int) ([]store.Event, error) {
	events, err := db.Events(a.ctx, after, limit)
	if err != nil || caller.Operator || caller.Kind == "plugin" {
		return events, err
	}
	visible := events[:0]
	readable := map[int64]bool{}
	for _, event := range events {
		if event.TaskID != 0 {
			ok, seen := readable[event.TaskID]
			if !seen {
				t, err := coord.Load(db, event.TaskID)
				if err != nil {
					return nil, err
				}
				if ok, err = coord.CanRead(db, caller, t); err != nil {
					return nil, err
				}
				readable[event.TaskID] = ok
			}
			if !ok {
				continue
			}
		}
		visible = append(visible, event)
	}
	return visible, nil
}
