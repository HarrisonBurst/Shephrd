package control

import (
	"fmt"

	"shephrd/internal/model"
	"shephrd/internal/worktree"
)

type nativeWorkspaceBackend struct {
	service Service
}

func (b *nativeWorkspaceBackend) Allocate(repo model.Repo, attempt model.Attempt, sessionID string) error {
	s := b.service
	path, err := s.Native.AllocatePath(repo, attempt)
	if err != nil {
		s.failSetup(attempt, err)
		return err
	}
	branch := worktree.BranchName(attempt)
	if err := s.Store.BeginNativeAllocation(attempt.ID, path); err != nil {
		s.failSetup(attempt, err)
		return err
	}
	failBeforeAdd := func(cause error) error {
		s.failSetup(attempt, cause)
		_ = s.Store.MarkNoWorkspace(attempt.ID, fmt.Sprintf("workspace acquisition failed before any worktree existed: %v", cause))
		return cause
	}
	base, err := s.resolveAttemptBase(repo, attempt)
	if err != nil {
		return failBeforeAdd(err)
	}
	if err := s.Native.CheckSupported(repo, base); err != nil {
		return failBeforeAdd(err)
	}
	if err := s.Store.SetAttemptBaseCommit(attempt.ID, base); err != nil {
		return failBeforeAdd(err)
	}
	if err := s.Native.Add(repo, path, branch, base); err != nil {
		pathExists, registered, stateErr := s.Native.RegistrationState(repo, path)
		if stateErr == nil && !pathExists && !registered {
			return failBeforeAdd(err)
		}
		s.failSetup(attempt, err)
		_ = s.Store.MarkWorkspaceUnknown(attempt.ID, fmt.Sprintf("git worktree add failed with ambiguous external evidence at %s: %v", path, err))
		return err
	}
	identity, err := s.Native.Inspect(repo, path, branch)
	if err == nil && identity.Head != base {
		err = fmt.Errorf("native worktree HEAD %s does not equal the persisted base commit %s", identity.Head, base)
	}
	if err != nil {
		s.failSetup(attempt, err)
		_ = s.Store.MarkWorkspaceUnknown(attempt.ID, fmt.Sprintf("native worktree identity could not be proven after creation at %s: %v", path, err))
		return err
	}
	workspace := model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: sessionID, Path: path,
		GitDir: identity.GitDir, CommonDir: identity.CommonDir, Branch: branch}
	if err := s.Store.ConfigureNativeWorkspace(attempt.ID, workspace); err != nil {
		s.failSetup(attempt, err)
		return err
	}
	if err := s.Native.RunSetupHook(repo, path); err != nil {
		s.failSetup(attempt, err)
		return err
	}
	return nil
}

func (s Service) resolveAttemptBase(repo model.Repo, attempt model.Attempt) (string, error) {
	selection, err := model.NormalizeBaseSelection(model.BaseSelection{Strategy: attempt.BaseStrategy, Ref: attempt.BaseRef})
	if err != nil {
		return "", err
	}
	switch selection.Strategy {
	case model.BaseStrategyDefaultBranch:
		return s.Native.ResolveDefaultBase(repo)
	case model.BaseStrategyBranch:
		return s.Native.ResolveBranchBase(repo, selection.Ref)
	case model.BaseStrategyCommit:
		return s.Native.ResolveCommitBase(repo, selection.Ref)
	case model.BaseStrategyTask:
		return s.resolveTaskBase(repo, attempt, selection.Ref)
	default:
		return "", fmt.Errorf("base strategy %q is invalid", selection.Strategy)
	}
}

func (s Service) resolveTaskBase(repo model.Repo, attempt model.Attempt, taskID string) (string, error) {
	if taskID == attempt.TaskID {
		return "", fmt.Errorf("task %s cannot use itself as a stacked base", attempt.TaskID)
	}
	sourceTask, err := s.Store.Task(taskID)
	if err != nil {
		return "", fmt.Errorf("resolve stacked base task %s: %w", taskID, err)
	}
	if sourceTask.RepoID != repo.ID {
		return "", fmt.Errorf("stacked base task %s belongs to repository %s, not %s", sourceTask.ID, sourceTask.RepoID, repo.ID)
	}
	if sourceTask.CurrentAttemptID == "" {
		return "", fmt.Errorf("stacked base task %s has no current attempt", sourceTask.ID)
	}
	sourceAttempt, err := s.Store.Attempt(sourceTask.CurrentAttemptID)
	if err != nil {
		return "", fmt.Errorf("resolve stacked base task %s attempt: %w", sourceTask.ID, err)
	}
	if sourceAttempt.WorkspaceBackend != model.WorkspaceBackendNative || sourceAttempt.Branch == "" || sourceAttempt.Branch != worktree.BranchName(sourceAttempt) {
		return "", fmt.Errorf("stacked base task %s does not have a current native attempt branch", sourceTask.ID)
	}
	if sourceAttempt.Status == model.AttemptStatusSuperseded || sourceAttempt.Status == model.AttemptStatusWorkspaceUnknown || sourceAttempt.WorkspaceState == model.WorkspaceStateUnknown || sourceAttempt.WorkspaceState == model.WorkspaceStateNoWorkspace {
		return "", fmt.Errorf("stacked base task %s has uncertain workspace identity", sourceTask.ID)
	}
	if sourceAttempt.WorkspaceState == model.WorkspaceStateHeld {
		if _, err := s.Native.VerifyHeld(repo, sourceAttempt); err != nil {
			return "", fmt.Errorf("verify stacked base task %s workspace: %w", sourceTask.ID, err)
		}
	} else if sourceAttempt.WorkspaceState != model.WorkspaceStateReleased {
		return "", fmt.Errorf("stacked base task %s has no proven native workspace", sourceTask.ID)
	}
	return s.Native.ResolveBranchBase(repo, sourceAttempt.Branch)
}

func (b *nativeWorkspaceBackend) VerifyHeld(repo model.Repo, attempt model.Attempt) error {
	if err := requireNativeWorkspace(attempt); err != nil {
		return err
	}
	if attempt.WorkspaceState != model.WorkspaceStateHeld {
		return fmt.Errorf("attempt %s native workspace is %s, not held; reconcile before relaunching", attempt.ID, attempt.WorkspaceState)
	}
	classification, err := b.Classify(repo, attempt)
	if err != nil {
		return err
	}
	if !classification.Exact {
		return fmt.Errorf("attempt %s exact native worktree identity cannot be verified: %s", attempt.ID, classification.FailureReason)
	}
	if pid := b.conflictingProcess(classification, attempt); pid != 0 {
		return fmt.Errorf("attempt %s worktree has a live conflicting process %d", attempt.ID, pid)
	}
	return nil
}

func (b *nativeWorkspaceBackend) Classify(repo model.Repo, attempt model.Attempt) (workspaceClassification, error) {
	if err := requireNativeWorkspace(attempt); err != nil {
		return workspaceClassification{}, err
	}
	s := b.service
	classification := workspaceClassification{}
	switch attempt.WorkspaceState {
	case model.WorkspaceStateAllocating:
		path := attempt.IntendedWorktreePath
		pathExists, registered, err := s.Native.RegistrationState(repo, path)
		classification.PathExists = pathExists
		classification.Registered = registered
		if err != nil {
			classification.FailureReason = err.Error()
			return classification, nil
		}
		if pathExists && registered {
			identity, inspectErr := s.Native.Inspect(repo, path, worktree.BranchName(attempt))
			classification.Identity = identity
			if inspectErr != nil {
				classification.FailureReason = inspectErr.Error()
			} else if attempt.BaseCommit != "" && identity.Head != attempt.BaseCommit {
				classification.FailureReason = fmt.Sprintf("native worktree HEAD %s does not equal the persisted base commit %s", identity.Head, attempt.BaseCommit)
			} else {
				classification.Exact = true
			}
		}
	case model.WorkspaceStateHeld, model.WorkspaceStateReleasing:
		if attempt.WorkspaceState == model.WorkspaceStateReleasing {
			pathExists, registered, err := s.Native.RegistrationState(repo, attempt.WorktreePath)
			classification.PathExists = pathExists
			classification.Registered = registered
			if err != nil {
				classification.FailureReason = err.Error()
				return classification, nil
			}
			if !pathExists || !registered {
				return classification, nil
			}
		} else {
			classification.PathExists = true
			classification.Registered = true
		}
		identity, err := s.Native.VerifyHeld(repo, attempt)
		classification.Identity = identity
		if err != nil {
			classification.FailureReason = err.Error()
			return classification, nil
		}
		classification.Exact = true
		classification.ProcessPIDs = s.Native.LiveProcessPIDs(attempt.WorktreePath)
	}
	return classification, nil
}

func (b *nativeWorkspaceBackend) Release(repo model.Repo, attempt model.Attempt, discard bool) (string, error) {
	if err := requireNativeWorkspace(attempt); err != nil {
		return "", err
	}
	s := b.service
	if attempt.WorktreePath == "" {
		return "native attempt had no worktree path", nil
	}
	classification, err := b.Classify(repo, attempt)
	if err != nil {
		_ = s.Store.ResetReleaseClaim(attempt.ID)
		return "", err
	}
	if !classification.Exact {
		_ = s.Store.ResetReleaseClaim(attempt.ID)
		reason := classification.FailureReason
		if reason == "" {
			reason = "recorded native worktree identity does not match external state"
		}
		_ = s.Store.MarkWorkspaceUnknown(attempt.ID, fmt.Sprintf("native worktree identity could not be verified at release time: %s", reason))
		return "", fmt.Errorf("attempt %s native worktree identity could not be verified at release time: %s", attempt.ID, reason)
	}
	if pid := b.conflictingProcess(classification, attempt); pid != 0 {
		_ = s.Store.ResetReleaseClaim(attempt.ID)
		return "", fmt.Errorf("attempt %s worktree has a live conflicting process %d; release was not performed", attempt.ID, pid)
	}
	if !discard {
		dirty, err := s.Native.Dirty(attempt.WorktreePath)
		if err != nil {
			_ = s.Store.ResetReleaseClaim(attempt.ID)
			return "", err
		}
		if dirty {
			_ = s.Store.ResetReleaseClaim(attempt.ID)
			return "", fmt.Errorf("attempt %s landed with residual workspace changes; the worktree stays held until the residual changes are explicitly discarded with 'shephrd workspace release %s --attempt %s --discard'", attempt.ID, attempt.TaskID, attempt.ID)
		}
	}
	if err := s.Native.Remove(repo, attempt.WorktreePath, discard); err != nil {
		_ = s.Store.ResetReleaseClaim(attempt.ID)
		return "", err
	}
	return "native worktree removal completed", nil
}

func (b *nativeWorkspaceBackend) Reconcile(repo model.Repo, task model.Task, attempt model.Attempt) (workspaceReconcileResult, error) {
	if err := requireNativeWorkspace(attempt); err != nil {
		return workspaceReconcileResult{}, err
	}
	s := b.service
	result := workspaceReconcileResult{Entry: ReconcileEntry{AttemptID: attempt.ID, TaskID: task.ID,
		RunGeneration: attempt.RunGeneration, Path: attempt.WorktreePath, Backend: attempt.WorkspaceBackend,
		WorkspaceState: attempt.WorkspaceState}}
	entry := &result.Entry
	if entry.Path == "" {
		entry.Path = attempt.IntendedWorktreePath
	}
	if attempt.ReleasedAt != nil {
		entry.Classification = "released"
		return result, nil
	}
	switch attempt.WorkspaceState {
	case model.WorkspaceStateNoWorkspace:
		entry.Classification = "no_workspace"
	case model.WorkspaceStateUnknown:
		entry.Classification = "unknown"
		entry.Reason = attempt.FailureReason
		result.Unknown = true
	case model.WorkspaceStateAllocating:
		classification, err := b.Classify(repo, attempt)
		if err != nil {
			return result, err
		}
		switch {
		case classification.FailureReason != "":
			entry.Classification = "unknown"
			entry.Reason = classification.FailureReason
			_ = s.Store.MarkWorkspaceUnknown(attempt.ID, entry.Reason)
			result.Unknown = true
		case !classification.PathExists && !classification.Registered:
			entry.Classification = "no_workspace"
			entry.Reason = "allocation intent was recorded but no worktree or registration exists"
			_ = s.Store.MarkNoWorkspace(attempt.ID, "workspace allocation was interrupted before any worktree existed; recovered as no_workspace")
		case classification.Exact:
			identity := classification.Identity
			if err := s.Store.CompleteNativeAllocation(attempt.ID, attempt.IntendedWorktreePath, identity.GitDir, identity.CommonDir, worktree.BranchName(attempt)); err == nil {
				entry.Classification = "recovered-held"
				entry.Reason = "allocation was interrupted after git worktree add; exact identity re-verified and persisted as held"
				entry.WorkspaceState = model.WorkspaceStateHeld
				result.Verified = true
				break
			}
			fallthrough
		default:
			entry.Classification = "unknown"
			entry.Reason = "allocation left partial or mismatched external evidence; no automatic cleanup was performed"
			_ = s.Store.MarkWorkspaceUnknown(attempt.ID, entry.Reason)
			result.Unknown = true
		}
	case model.WorkspaceStateReleasing:
		if s.processAlive(attempt.ReleaseOwnerPID) {
			entry.Classification = "releasing"
			entry.Reason = fmt.Sprintf("release is owned by live process %d", attempt.ReleaseOwnerPID)
			return result, nil
		}
		classification, err := b.Classify(repo, attempt)
		if err != nil {
			return result, err
		}
		switch {
		case classification.FailureReason != "" && !classification.Exact:
			entry.Classification = "unknown"
			entry.Reason = classification.FailureReason
			if err := s.Store.MarkWorkspaceUnknown(attempt.ID, entry.Reason); err != nil {
				return result, err
			}
			result.Unknown = true
		case !classification.PathExists && !classification.Registered:
			reason := "release owner exited after native worktree removal; recorded recovered release"
			if err := s.Store.MarkReleasedWithReason(attempt.ID, reason); err != nil {
				return result, err
			}
			entry.Classification = "released"
			entry.Reason = reason
			result.Verified = true
		case !classification.PathExists && classification.Registered:
			if err := s.Native.RemoveAbsentRegistration(repo, attempt.WorktreePath); err != nil {
				entry.Classification = "unknown"
				entry.Reason = err.Error()
				if err := s.Store.MarkWorkspaceUnknown(attempt.ID, entry.Reason); err != nil {
					return result, err
				}
				result.Unknown = true
				break
			}
			reason := "release owner exited between worktree removal and registration cleanup; completed exact removal"
			if err := s.Store.MarkReleasedWithReason(attempt.ID, reason); err != nil {
				return result, err
			}
			entry.Classification = "released"
			entry.Reason = reason
			result.Verified = true
		default:
			if _, err := b.Release(repo, attempt, attempt.DiscardAuthorized); err != nil {
				entry.Classification = "releasing-failed"
				entry.Reason = err.Error()
				break
			}
			reason := "release owner exited before native worktree removal; re-verified identity and completed exact removal"
			if err := s.Store.MarkReleasedWithReason(attempt.ID, reason); err != nil {
				return result, err
			}
			entry.Classification = "released"
			entry.Reason = reason
			result.Verified = true
		}
	case model.WorkspaceStateHeld:
		classification, err := b.Classify(repo, attempt)
		if err != nil {
			return result, err
		}
		if !classification.Exact {
			entry.Classification = "unknown"
			entry.Reason = fmt.Sprintf("native worktree identity cannot be verified after restart: %s", classification.FailureReason)
			if attempt.ID == task.CurrentAttemptID && !isTerminal(task.Status) {
				_ = s.recordControlFailure(attempt.ID, attempt.RunGeneration, entry.Reason)
			}
			if err := s.Store.MarkWorkspaceUnknown(attempt.ID, entry.Reason); err != nil {
				return result, err
			}
			result.Unknown = true
			break
		}
		if attempt.ID == task.CurrentAttemptID && !isTerminal(task.Status) {
			if ok, reason, endpointErr := s.reconcileEndpoint(attempt, task); endpointErr != nil {
				return result, endpointErr
			} else if !ok {
				entry.Classification = "unknown"
				entry.Reason = reason
				if entry.Reason == "" {
					entry.Reason = "worker endpoint identity cannot be verified"
				}
				_ = s.recordControlFailure(attempt.ID, attempt.RunGeneration, entry.Reason)
				if err := s.Store.MarkWorkspaceUnknown(attempt.ID, entry.Reason); err != nil {
					return result, err
				}
				result.Unknown = true
				break
			}
		}
		checkpoint, checkpointErr := s.Store.LatestCheckpoint(attempt.ID)
		if checkpointErr == nil {
			entry.CheckpointRevision = checkpoint.Revision
			entry.CheckpointProducer = checkpoint.Producer
		}
		conflictPID := b.conflictingProcess(classification, attempt)
		switch {
		case attempt.ID != task.CurrentAttemptID:
			entry.Classification = "superseded-held"
		case conflictPID != 0:
			entry.Classification = "process-conflict"
			entry.Reason = fmt.Sprintf("worktree has an unrelated live process %d", conflictPID)
		case s.processAlive(attempt.RunnerPID):
			entry.Classification = "working"
		case checkpointErr == nil && checkpoint.Producer == "worker":
			entry.Classification = "resume-eligible"
		default:
			entry.Classification = "resume-degraded"
		}
		result.Verified = true
	default:
		entry.Classification = "unknown"
		entry.Reason = fmt.Sprintf("native attempt has unrecognized workspace state %q", attempt.WorkspaceState)
		result.Unknown = true
	}
	return result, nil
}

func (b *nativeWorkspaceBackend) conflictingProcess(classification workspaceClassification, attempt model.Attempt) int {
	for _, pid := range classification.ProcessPIDs {
		if pid != attempt.RunnerPID && pid != attempt.ReleaseOwnerPID && b.service.processAlive(pid) {
			return pid
		}
	}
	return 0
}
