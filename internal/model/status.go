package model

import (
	"fmt"
	"strings"
)

const (
	TaskStatusQueued   = "queued"
	TaskStatusStarting = "starting"
	TaskStatusWorking  = "working"
	TaskStatusWaiting  = "waiting"
	TaskStatusDone     = "done"
	TaskStatusBlocked  = "blocked"
	TaskStatusFailed   = "failed"
	TaskStatusStopped  = "stopped"

	AttemptStatusStarting         = "starting"
	AttemptStatusWorking          = "working"
	AttemptStatusWaiting          = "waiting"
	AttemptStatusDone             = "done"
	AttemptStatusBlocked          = "blocked"
	AttemptStatusFailed           = "failed"
	AttemptStatusStopped          = "stopped"
	AttemptStatusSuperseded       = "superseded"
	AttemptStatusWorkspaceUnknown = "unknown"

	WorkspaceStateUnassigned  = ""
	WorkspaceStateAllocating  = "allocating"
	WorkspaceStateHeld        = "held"
	WorkspaceStateNoWorkspace = "no_workspace"
	WorkspaceStateUnknown     = "unknown"
	WorkspaceStateReleasing   = "releasing"
	WorkspaceStateReleased    = "released"

	WorkspaceBackendTreehouse = "treehouse"
	WorkspaceBackendNative    = "native_git_worktree"

	LandingKindLocalAttestedAncestry   = "local_default_branch_attested_ancestry"
	CompletionProvenanceWorkerDone     = "worker_done"
	CompletionProvenanceReportRecovery = "driver_report_recovery"

	MaxTaskReportInputs = 16
)

func ValidTaskStatus(status string) bool {
	switch status {
	case TaskStatusQueued, TaskStatusStarting, TaskStatusWorking, TaskStatusWaiting, TaskStatusDone, TaskStatusBlocked, TaskStatusFailed, TaskStatusStopped:
		return true
	default:
		return false
	}
}

func ValidAttemptStatus(status string) bool {
	switch status {
	case AttemptStatusStarting, AttemptStatusWorking, AttemptStatusWaiting, AttemptStatusDone, AttemptStatusBlocked, AttemptStatusFailed, AttemptStatusStopped, AttemptStatusSuperseded, AttemptStatusWorkspaceUnknown:
		return true
	default:
		return false
	}
}

func ValidWorkspaceState(state string) bool {
	switch state {
	case WorkspaceStateUnassigned, WorkspaceStateAllocating, WorkspaceStateHeld, WorkspaceStateNoWorkspace, WorkspaceStateUnknown, WorkspaceStateReleasing, WorkspaceStateReleased:
		return true
	default:
		return false
	}
}

func IsTerminalTaskStatus(status string) bool {
	return status == TaskStatusDone || status == TaskStatusBlocked || status == TaskStatusFailed || status == TaskStatusStopped
}

func WithWorkerRecoveryGuidance(reason string) string {
	if strings.Contains(reason, "shephrd worker relaunch") {
		return reason
	}
	return reason + "\nRecovery: use `shephrd worker relaunch <task-id>` after exact held-worktree identity verification to continue with the same filesystem state. Use `shephrd worker retry <task-id>` only when intentionally choosing a clean attempt without filesystem transfer."
}

func ValidateTransition(from, to string, retry bool) error {
	if from == to {
		return nil
	}
	allowed := map[string]map[string]bool{
		TaskStatusQueued:   {TaskStatusStarting: true, TaskStatusStopped: true},
		TaskStatusStarting: {TaskStatusWorking: true, TaskStatusBlocked: true, TaskStatusFailed: true, TaskStatusStopped: true},
		TaskStatusWorking:  {TaskStatusWaiting: true, TaskStatusDone: true, TaskStatusBlocked: true, TaskStatusFailed: true, TaskStatusStopped: true},
		TaskStatusWaiting:  {TaskStatusWorking: true, TaskStatusDone: true, TaskStatusBlocked: true, TaskStatusFailed: true, TaskStatusStopped: true},
	}
	if retry && (IsTerminalTaskStatus(from) || from == TaskStatusWaiting) && to == TaskStatusQueued {
		return nil
	}
	if allowed[from][to] {
		return nil
	}
	if IsTerminalTaskStatus(from) {
		return fmt.Errorf("task is terminal (%s); use worker retry to create a new attempt", from)
	}
	return fmt.Errorf("invalid task status transition %s -> %s", from, to)
}
