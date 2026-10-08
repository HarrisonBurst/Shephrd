package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed migration_v21.sql
var migration21SQL string

func migrateReportRecoveryV21(ctx context.Context, conn *sql.Conn) error {
	var table, taskColumn, artifactColumn int
	if err := conn.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='report_recovery_attestations'),
		EXISTS(SELECT 1 FROM pragma_table_info('tasks') WHERE name='completion_provenance'),
		EXISTS(SELECT 1 FROM pragma_table_info('verified_artifacts') WHERE name='report_recovery_id')`).Scan(&table, &taskColumn, &artifactColumn); err != nil {
		return err
	}
	if table+taskColumn+artifactColumn == 3 {
		var objects, recoveryView int
		if err := conn.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM sqlite_schema WHERE name IN (
				'report_recovery_attestations_task_idx', 'report_recovery_attestations_immutable_update',
				'report_recovery_attestations_immutable_delete', 'verified_artifacts_report_recovery_idx')),
			EXISTS(SELECT 1 FROM sqlite_schema WHERE type='view' AND name='attempt_landing_projections'
				AND sql LIKE '%report_recovery_attestations%' AND sql LIKE '%report_recovery_id%')`).Scan(&objects, &recoveryView); err != nil {
			return err
		}
		if objects != 4 || recoveryView != 1 {
			return fmt.Errorf("report recovery schema exists without its complete immutable evidence contract")
		}
		return nil
	}
	if table+taskColumn+artifactColumn != 0 {
		return fmt.Errorf("partial report recovery schema exists without migration identity")
	}
	_, err := conn.ExecContext(ctx, migration21SQL)
	return err
}
