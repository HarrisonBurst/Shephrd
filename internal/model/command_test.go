package model

import "testing"

func TestShellQuote(t *testing.T) {
	if got, want := ShellQuote("driver:$x'owner"), `'driver:$x'"'"'owner'`; got != want {
		t.Fatalf("ShellQuote = %q, want %q", got, want)
	}
}

func TestReportRecoveryContinuationFailureHasExactCommands(t *testing.T) {
	verifyCommand, retryCommand := ReportRecoveryContinuationCommands("task_1", "attempt_1", "driver:$x'owner")
	err := ReportRecoveryContinuationFailure("task_1", "attempt_1", "driver:$x'owner")
	typed, ok := err.(*KindError)
	if !ok || typed.Kind != "report_recovery_continuation_forbidden" || typed.Evidence["verify_command"] != verifyCommand || typed.Evidence["retry_command"] != retryCommand {
		t.Fatalf("failure = %+v", err)
	}
}
