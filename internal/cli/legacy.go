package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"shephrd/internal/config"
	"shephrd/internal/store"
)

func legacyCommand(get func() *application) *cobra.Command {
	command := commandGroup("legacy", "Bounded legacy schema support")
	export := &cobra.Command{
		Use:         "export <output-dir>",
		Annotations: map[string]string{standaloneAnnotation: "true"},
		Short:       "Read-only export of a below-floor or bridge-version legacy database",
		Long:        "Copies the legacy database and publishes the raw table names, column orders, row counts, and the content-addressed digest of the retired legacy task-list tables. The database is opened read-only and is never migrated or rewritten. Below-floor exports are bounded by the compiled 30-day window; bridge-version exports stay available for this binary's lifetime so the baseline migration can accept their digest.",
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			export, err := store.ExportLegacyDatabase(cfg.DatabasePath, args[0], time.Now().UTC())
			if err != nil {
				return err
			}
			return get().print(map[string]any{"schema_version": 1, "schema": export.SchemaVersion, "digest": export.Digest, "legacy_rows": export.LegacyRows, "tables": len(export.Tables), "snapshots": len(export.Snapshots), "out": args[0]}, func() {
				fmt.Fprintf(get().out, "exported legacy schema %d database to %s\n", export.SchemaVersion, args[0])
				fmt.Fprintf(get().out, "legacy task-list rows: %d digest=%s\n", export.LegacyRows, export.Digest)
				fmt.Fprintf(get().out, "tables: %d report snapshots: %d\n", len(export.Tables), len(export.Snapshots))
				fmt.Fprintln(get().out, "recovery instructions: "+args[0]+"/recovery-instructions.txt")
			})
		},
	}
	command.AddCommand(export)
	return command
}
