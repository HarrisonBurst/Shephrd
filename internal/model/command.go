package model

import (
	"fmt"
	"strings"
)

func ShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func ReportRecoveryContinuationCommands(taskID, attemptID, driverID string) (string, string) {
	return fmt.Sprintf("shephrd task verify-delivery %s --attempt %s --driver-id %s --json", taskID, attemptID, ShellQuote(driverID)),
		fmt.Sprintf("shephrd worker retry %s --json", taskID)
}

func ReportRecoveryContinuationFailure(taskID, attemptID, driverID string) error {
	verifyCommand, retryCommand := ReportRecoveryContinuationCommands(taskID, attemptID, driverID)
	return EvidenceFailure("report_recovery_continuation_forbidden", map[string]string{
		"verify_command":  verifyCommand,
		"retry_command":   retryCommand,
		"inspect_command": "shephrd task inspect " + taskID + " --json",
	}, "attempt %s has immutable report recovery evidence; verify with `%s` or start a clean retry with `%s`; same-worktree worker relaunch and send are forbidden", attemptID, verifyCommand, retryCommand)
}
