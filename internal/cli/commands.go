package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/execution"
	"shephrd/internal/fault"
	"shephrd/internal/store"
	"shephrd/internal/version"
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
	root.AddCommand(initCommand(a), versionCommand(a), repoCommand(a), taskCommand(a), reportCommand(a), workspaceCommand(a), hostCommand(a), eventsCommand(a), pluginCommand(a), skillCommand(a), inboxCommand(a), daemonCommand(a), grantCommand(a), artifactCommand(a))
	root.AddCommand(privateRunCommands(a)...)
	for _, forced := range []struct{ use, short string }{
		{"serve", "The SSH forced command on the home host: serve --as driver:<name>, --as plugin:<name> or --host <name>"},
		{"agent", "The SSH forced command on a worker host: host operations for the home host"},
	} {
		root.AddCommand(&cobra.Command{Use: forced.use, Short: forced.short, RunE: a.run(func(*cobra.Command, []string) (any, error) {
			return nil, fault.New("usage", "%s runs only as an SSH forced command", forced.use)
		})})
	}
	a.reserved = []string{"help"}
	for _, cmd := range root.Commands() {
		a.reserved = append(a.reserved, cmd.Name())
	}
	return root
}

func initCommand(a *app) *cobra.Command {
	var host, driver, home string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create configuration, and the store on the home host",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringVar(&host, "host", "", "this host's name (default: the short hostname; none for a client host)")
	cmd.Flags().StringVar(&driver, "driver", "main", "the driver name local commands act as")
	cmd.Flags().StringVar(&home, "home", "", "the home host's SSH target, for a client or worker host")
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
			if host == "" && home == "" {
				if host, err = defaultHost(); err != nil {
					return nil, err
				}
			}
			if !config.NamePattern.MatchString(host) && (home == "" || host != "") {
				return nil, fault.New("usage", "host %q must match %s", host, config.NamePattern)
			}
			if !config.NamePattern.MatchString(driver) {
				return nil, fault.New("usage", "driver %q must match %s", driver, config.NamePattern)
			}
			if err := config.Write(path, host, driver, home); err != nil {
				return nil, err
			}
			created = true
		} else if err != nil {
			return nil, err
		}
		cfg, err := a.config()
		if err != nil {
			return nil, err
		}
		if cfg.Home != "" {
			return map[string]any{"config": path, "host": cfg.Host, "home": cfg.Home, "created": created}, nil
		}
		db, err := a.store()
		if err != nil {
			return nil, err
		}
		return map[string]any{"config": path, "store": db.Path, "host": cfg.Host, "created": created}, nil
	})
	return cmd
}

func versionCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the binary and protocol version",
		Args:  cobra.NoArgs,
		RunE: a.run(func(*cobra.Command, []string) (any, error) {
			return map[string]any{"version": version.String(), "protocol": Protocol}, nil
		}),
	}
}

func repoCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "repo", Short: "Register and list repositories"}
	var name, branch, setup, hostName string
	add := &cobra.Command{
		Use:   "add <path>",
		Short: "Register a git repository on a host (operator only)",
		Args:  cobra.ExactArgs(1),
	}
	add.Flags().StringVar(&hostName, "host", "", "the host the repository is on (default: the home host)")
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
		if hostName == "" {
			hostName = cfg.Host
		}
		host, err := execution.Hosts(cfg, a.getenv)(hostName)
		if err != nil {
			return nil, err
		}
		var inspected execution.InspectResponse
		if err := host.Call(a.ctx, "inspect_repo", execution.InspectRequest{Path: args[0], DefaultBranch: branch}, &inspected); err != nil {
			return nil, err
		}
		path, branch := inspected.Path, inspected.DefaultBranch
		if name == "" {
			name = strings.ToLower(filepath.Base(path))
		}
		if !config.NamePattern.MatchString(name) {
			return nil, fault.New("usage", "repository name %q must match %s; pass --name", name, config.NamePattern)
		}
		return a.mutate(func(tx *store.Tx, caller Caller) (any, error) {
			return coord.AddRepo(tx, caller.String(), coord.Repo{Host: hostName, Path: path, Name: name, DefaultBranch: branch, Setup: setup})
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

func defaultHost() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", err
	}
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	return host, nil
}
