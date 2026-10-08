package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"shephrd/internal/config"
	"shephrd/internal/repository"
)

func repoContextCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "context <path>",
		Short:       "Locate project context and show the configured memory setting",
		Long:        "Resolve a Git repository or worktree from an explicit directory path, locate its optional .shephrd/context.md, and show the global memory setting without reading notes, registering the repository, or opening the task database. Repository and task opt-outs remain agent instructions.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{standaloneAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			context, err := repository.InspectProjectContext(args[0], cfg.Memory)
			if err != nil {
				return err
			}
			jsonOutput, err := cmd.Flags().GetBool("json")
			if err != nil {
				return err
			}
			app := application{json: jsonOutput, out: cmd.OutOrStdout()}
			return app.print(context, func() {
				fmt.Fprintf(app.out, "repository: %s\nmemory.enabled: %t\n", context.RepositoryPath, context.Memory.Enabled)
				if context.OverviewPath != "" {
					fmt.Fprintf(app.out, "overview: %s\n", context.OverviewPath)
				} else {
					fmt.Fprintln(app.out, "overview: absent (optional)")
				}
			})
		},
	}
}
