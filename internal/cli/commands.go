package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

func newRoot(a *app) *cobra.Command {
	if a == nil {
		a = &app{}
	}
	root := &cobra.Command{
		Use:           "shephrd",
		Short:         "Delegate repository work to sub-drivers and isolated workers",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().String("as", "", "act as driver:<name> (operator only, local only)")
	root.PersistentFlags().String("key", "", "idempotency key for a mutation; generated when absent")
	root.AddCommand(initCommand(a), versionCommand(a), repoCommand(a), taskCommand(a), reportCommand(a), workspaceCommand(a), hostCommand(a), eventsCommand(a), pluginCommand(a), skillCommand(a), inboxCommand(a), daemonCommand(a))
	root.AddCommand(privateRunCommands(a)...)
	a.reserved = []string{"help"}
	for _, cmd := range root.Commands() {
		a.reserved = append(a.reserved, cmd.Name())
	}
	return root
}

func initCommand(a *app) *cobra.Command {
	var host, driver string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create configuration and the store on the home host",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringVar(&host, "host", "", "this host's name (default: the short hostname)")
	cmd.Flags().StringVar(&driver, "driver", "main", "the driver name local commands act as")
	cmd.RunE = a.run(func(*cobra.Command, []string) (any, error) {
		if !a.origin.Local || a.req.RunToken != "" || a.req.CallToken != "" {
			return nil, fault.New("operator_only", "only the operator on the home host can do this")
		}
		path, err := config.Path(a.getenv)
		if err != nil {
			return nil, err
		}
		created := false
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if host == "" {
				if host, err = defaultHost(); err != nil {
					return nil, err
				}
			}
			if !config.NamePattern.MatchString(host) {
				return nil, fault.New("usage", "host %q must match %s", host, config.NamePattern)
			}
			if !config.NamePattern.MatchString(driver) {
				return nil, fault.New("usage", "driver %q must match %s", driver, config.NamePattern)
			}
			if err := config.Write(path, host, driver); err != nil {
				return nil, err
			}
			created = true
		} else if err != nil {
			return nil, err
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		return map[string]any{"config": path, "store": db.Path, "host": a.cfg.Host, "created": created}, nil
	})
	return cmd
}

func versionCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the binary and protocol version",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command, []string) (any, error) {
			return map[string]any{"version": Version(), "protocol": Protocol}, nil
		}),
	}
}

func repoCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "repo", Short: "Register and list repositories"}
	var name, branch, setup string
	add := &cobra.Command{
		Use:   "add <path>",
		Short: "Register a git repository on this host (operator only)",
		Args:  cobra.ExactArgs(1),
	}
	add.Flags().StringVar(&name, "name", "", "repository name (default: the directory name)")
	add.Flags().StringVar(&branch, "default-branch", "", "default branch (default: origin's HEAD, else the current branch)")
	add.Flags().StringVar(&setup, "setup", "", "setup command run once in each new workspace, or - for stdin")
	add.RunE = a.run(func(_ *cobra.Command, args []string) (any, error) {
		if err := a.requireOperator(); err != nil {
			return nil, err
		}
		setup, err := a.text("setup", setup, 4096)
		if err != nil {
			return nil, err
		}
		cfg, err := a.config()
		if err != nil {
			return nil, err
		}
		path, branch, err := coord.InspectRepo(args[0], branch)
		if err != nil {
			return nil, err
		}
		if name == "" {
			name = strings.ToLower(filepath.Base(path))
		}
		if !config.NamePattern.MatchString(name) {
			return nil, fault.New("usage", "repository name %q must match %s; pass --name", name, config.NamePattern)
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			return coord.AddRepo(tx, caller.String(), coord.Repo{Host: cfg.Host, Path: path, Name: name, DefaultBranch: branch, Setup: setup})
		})
	})
	list := &cobra.Command{
		Use:   "list",
		Short: "List registered repositories",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command, []string) (any, error) {
			if _, err := a.identity(); err != nil {
				return nil, err
			}
			db, err := a.store()
			if err != nil {
				return nil, err
			}
			repos, err := coord.ListRepos(a.ctx, db)
			if err != nil {
				return nil, err
			}
			return map[string]any{"repos": repos}, nil
		}),
	}
	cmd.AddCommand(add, list)
	return cmd
}

func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["vcs.revision"] == "" {
		return "(devel)"
	}
	if settings["vcs.modified"] == "true" {
		return settings["vcs.revision"] + "+dirty"
	}
	return settings["vcs.revision"]
}

func defaultHost() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", err
	}
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	return host, nil
}
