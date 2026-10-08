package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/brief"
	"shephrd/internal/config"
	"shephrd/internal/delivery"
	forgegithub "shephrd/internal/forge/github"
	"shephrd/internal/lifecycle"
	domain "shephrd/internal/model"
	"shephrd/internal/notification"
	"shephrd/internal/repository"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
	"shephrd/internal/verify"
	"shephrd/internal/worktree"
)

type Service struct {
	Config            config.Config
	Store             *store.Store
	Native            worktree.NativeManager
	Verifier          delivery.Verifier
	TerminalProviders []terminal.Provider
	Notifier          *notification.Notifier
	clock             clockCapability
	environment       environmentCapability
	processes         processCapability
}

type SpawnResult = domain.SpawnResult

type ReconcileEntry struct {
	AttemptID          string `json:"attempt_id"`
	TaskID             string `json:"task_id"`
	RunGeneration      int    `json:"run_generation"`
	CheckpointRevision int    `json:"checkpoint_revision"`
	CheckpointProducer string `json:"checkpoint_producer"`
	Path               string `json:"path"`
	LeaseID            string `json:"lease_id"`
	LeaseHolder        string `json:"lease_holder"`
	Backend            string `json:"workspace_backend,omitempty"`
	WorkspaceState     string `json:"workspace_state,omitempty"`
	Classification     string `json:"classification"`
	Reason             string `json:"reason,omitempty"`
}

type ReconcileResult struct {
	SchemaVersion        int                       `json:"schema_version"`
	MutatesRecoveryState bool                      `json:"mutates_recovery_state"`
	Verified             []string                  `json:"verified"`
	Unknown              []string                  `json:"unknown"`
	Classifications      []ReconcileEntry          `json:"classifications"`
	NotificationCounts   domain.NotificationCounts `json:"notification_counts"`
}

type AttestDeliveryResult struct {
	SchemaVersion       int    `json:"schema_version"`
	TaskID              string `json:"task_id"`
	AttemptID           string `json:"attempt_id"`
	AttestationID       string `json:"attestation_id"`
	Idempotent          bool   `json:"idempotent"`
	OriginalArtifactRef string `json:"original_artifact_ref"`
	SealedCommit        string `json:"sealed_commit"`
	CheckpointRevision  int    `json:"checkpoint_revision"`
	PRURL               string `json:"pr_url"`
	PRHeadCommit        string `json:"pr_head_commit"`
	CommitRelation      string `json:"commit_relation"`
	MergeCommit         string `json:"merge_commit"`
	TargetRef           string `json:"target_ref"`
	LandingProven       bool   `json:"landing_proven"`
	ReleaseState        string `json:"release_state"`
	VerificationEnabled bool   `json:"verification_enabled"`
	NextCommand         string `json:"next_command"`
}

type AttestReportRecoveryResult struct {
	SchemaVersion                 int    `json:"schema_version"`
	TaskID                        string `json:"task_id"`
	AttemptID                     string `json:"attempt_id"`
	RunGeneration                 int    `json:"run_generation"`
	AttestationID                 string `json:"attestation_id"`
	Idempotent                    bool   `json:"idempotent"`
	CheckpointRevision            int    `json:"checkpoint_revision"`
	CheckpointSourceCursor        int64  `json:"checkpoint_source_cursor"`
	CheckpointSessionID           string `json:"checkpoint_session_id"`
	CheckpointBranch              string `json:"checkpoint_branch"`
	CheckpointHeadCommit          string `json:"checkpoint_head_commit"`
	CheckpointDirty               bool   `json:"checkpoint_worktree_dirty"`
	CheckpointWorkspaceFactsError string `json:"checkpoint_workspace_facts_error"`
	WorkspaceBackend              string `json:"workspace_backend"`
	WorktreePath                  string `json:"worktree_path"`
	CanonicalReportPath           string `json:"canonical_report_path"`
	FileIdentity                  string `json:"file_identity"`
	SHA256                        string `json:"sha256"`
	SizeBytes                     int64  `json:"size_bytes"`
	Reason                        string `json:"reason"`
	AttestedByDriverID            string `json:"attested_by_driver_id"`
	CompletionProvenance          string `json:"completion_provenance"`
	LandingProven                 bool   `json:"landing_proven"`
	ReleaseState                  string `json:"release_state"`
	VerificationEnabled           bool   `json:"verification_enabled"`
	NextCommand                   string `json:"next_command"`
}

func New(cfg config.Config, state *store.Store) Service {
	service := Service{Config: cfg, Store: state, Native: worktree.NewNative(cfg.WorktreeRoot), Verifier: delivery.New(cfg.DataDir)}
	if extension := cfg.GitHubObservation; extension != nil {
		service.Verifier.Forge = forgegithub.NewClient(extension.Command, extension.SHA256, service.environ())
	}
	if extension := cfg.Notifications.Extension; cfg.Notifications.Enabled && extension != nil {
		service.Notifier = notification.New(notification.Options{
			Details: cfg.Notifications.Details, TaskPerMinute: cfg.Notifications.TaskPerMinute,
			GlobalPerMinute: cfg.Notifications.GlobalPerMinute,
			Presenter:       notification.NewExtensionPresenter(extension.Command, extension.SHA256, service.environ()),
		})
	}
	return service
}

func (s Service) CreateTaskWithReports(task domain.Task, producerTaskIDs []string) (domain.Task, error) {
	if len(producerTaskIDs) == 0 {
		return s.Store.CreateTask(task)
	}
	if len(producerTaskIDs) > domain.MaxTaskReportInputs {
		return domain.Task{}, fmt.Errorf("a task can attach at most %d verified reports", domain.MaxTaskReportInputs)
	}
	inputs := make([]domain.ReportArtifactSelection, 0, len(producerTaskIDs))
	seen := make(map[string]struct{}, len(producerTaskIDs))
	for _, producerTaskID := range producerTaskIDs {
		producerTaskID = strings.TrimSpace(producerTaskID)
		if producerTaskID == "" {
			return domain.Task{}, fmt.Errorf("--with-report-from requires a producer task ID")
		}
		if _, exists := seen[producerTaskID]; exists {
			return domain.Task{}, fmt.Errorf("producer task %s was selected more than once", producerTaskID)
		}
		seen[producerTaskID] = struct{}{}
		artifact, err := s.Store.EligibleVerifiedReport(producerTaskID)
		if err != nil {
			return domain.Task{}, err
		}
		if _, err := s.Verifier.ReadVerifiedReport(artifact); err != nil {
			return domain.Task{}, err
		}
		inputs = append(inputs, domain.ReportArtifactSelection{ProducerTaskID: producerTaskID, ArtifactID: artifact.ID})
	}
	return s.Store.CreateTaskWithReportInputs(task, inputs)
}

func (s Service) ArchiveTask(taskID string) (domain.Task, error) {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return domain.Task{}, err
	}
	if task.CurrentAttemptID != "" {
		attempt, err := s.Store.Attempt(task.CurrentAttemptID)
		if err != nil {
			return domain.Task{}, err
		}
		if s.processAlive(attempt.RunnerPID) {
			return domain.Task{}, fmt.Errorf("task %s has a live worker; wait for the worker to exit before archiving", task.ID)
		}
	}
	return s.Store.ArchiveTask(task.ID)
}

func (s Service) loadReportBriefs(taskID string) ([]brief.Report, error) {
	inputs, err := s.Store.ReportInputs(taskID)
	if err != nil {
		return nil, err
	}
	reports := make([]brief.Report, 0, len(inputs))
	for _, input := range inputs {
		body, err := s.Verifier.ReadReportInput(input)
		if err != nil {
			return nil, err
		}
		reports = append(reports, brief.Report{Input: input, Body: body})
	}
	return reports, nil
}

func (s Service) SpawnWithModel(taskID, harness, model string, requestedRuntime ...string) (SpawnResult, error) {
	return s.SpawnWithModelSelection(taskID, harness, model, model != "", requestedRuntime...)
}

func (s Service) SpawnWithModelSelection(taskID, harness, model string, modelProvided bool, requestedRuntime ...string) (SpawnResult, error) {
	return s.SpawnWithBaseSelection(taskID, harness, model, modelProvided, domain.DefaultBaseSelection(), requestedRuntime...)
}

func (s Service) SpawnWithBaseSelection(taskID, harness, modelID string, modelProvided bool, baseSelection domain.BaseSelection, requestedRuntime ...string) (SpawnResult, error) {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return SpawnResult{}, err
	}
	reports, err := s.loadReportBriefs(task.ID)
	if err != nil {
		return SpawnResult{}, err
	}
	harness, modelID, err = s.resolveInitialWorkerSelection(task.RepoID, harness, modelID, modelProvided)
	if err != nil {
		return SpawnResult{}, err
	}
	return s.spawnWithModel(task, harness, modelID, reports, baseSelection, requestedRuntime...)
}

func (s Service) resolveInitialWorkerSelection(repoID, harness, model string, modelProvided bool) (string, string, error) {
	selection, repoOverride := s.Config.RepositoryModels[repoID]
	if harness == "" {
		harness = s.Config.DefaultHarness
		if repoOverride {
			harness = selection.Harness
		}
		if harness == "current" {
			harness = s.getenv("SHEPHRD_DRIVER_HARNESS")
			if harness == "" {
				return "", "", fmt.Errorf("SHEPHRD_DRIVER_HARNESS is required when default_harness is current; set it to claude-code, pi, or codex")
			}
			if !config.ValidHarness(harness) {
				return "", "", fmt.Errorf("SHEPHRD_DRIVER_HARNESS must be claude-code, pi, or codex, got %q", harness)
			}
		}
	}
	if !modelProvided {
		switch {
		case repoOverride && harness == selection.Harness:
			model = selection.Model
		case harness == s.Config.DefaultHarness && s.Config.DefaultModel != "":
			model = s.Config.DefaultModel
		default:
			model = s.driverModelForHarness(harness)
		}
	}
	return harness, model, nil
}

func (s Service) resolveRetryWorkerSelection(attempt domain.Attempt, harness, model string, modelProvided bool) (string, string) {
	if harness == "" {
		harness = attempt.Harness
	}
	if !modelProvided {
		if harness == attempt.Harness {
			model = attempt.Model
		} else {
			model = s.driverModelForHarness(harness)
		}
	}
	return harness, model
}

func (s Service) driverModelForHarness(harness string) string {
	driverHarness := s.getenv("SHEPHRD_DRIVER_HARNESS")
	if !config.ValidHarness(driverHarness) || harness != driverHarness {
		return ""
	}
	return s.getenv("SHEPHRD_DRIVER_MODEL")
}

func (s Service) spawnWithModel(task domain.Task, harness, modelID string, reports []brief.Report, baseSelection domain.BaseSelection, requestedRuntime ...string) (result SpawnResult, err error) {
	baseSelection, err = domain.NormalizeBaseSelection(baseSelection)
	if err != nil {
		return result, err
	}
	if task.Status != domain.TaskStatusQueued {
		if isTerminal(task.Status) {
			return result, fmt.Errorf("task %s is terminal (%s); use worker retry to create a new attempt", task.ID, task.Status)
		}
		return result, fmt.Errorf("task %s is %s; only queued tasks can be spawned", task.ID, task.Status)
	}
	if err := adapter.Validate(harness); err != nil {
		return result, err
	}
	selection, err := s.selectRuntime(requestedRuntime...)
	if err != nil {
		return result, err
	}
	workerRuntime := selection.Backend
	if isTerminalRuntime(workerRuntime) {
		if _, err := s.workerExecutable(); err != nil {
			return result, err
		}
	}
	attempt, err := s.Store.BeginAttemptWithBaseSelection(task.ID, harness, modelID, baseSelection)
	if err != nil {
		return result, err
	}
	if err := s.Store.SetRuntimeBackend(attempt.ID, workerRuntime); err != nil {
		s.failSetup(attempt, err)
		return result, err
	}
	repo, err := s.Store.Repo(task.RepoID)
	if err != nil {
		s.failSetup(attempt, err)
		return result, err
	}
	if err := s.workspaceBackend().Allocate(repo, attempt, adapter.NewSessionID(harness)); err != nil {
		return result, err
	}
	attempt, _ = s.Store.Attempt(attempt.ID)
	generation, err := s.Store.ReserveRunGeneration(attempt.ID)
	if err != nil {
		s.failSetup(attempt, err)
		return result, err
	}
	attempt, _ = s.Store.Attempt(attempt.ID)
	attempt.ReportPath, err = s.assignReportDestination(task, attempt)
	if err != nil {
		s.failSetup(attempt, err)
		return result, err
	}
	facts := workspaceFacts(attempt.WorktreePath)
	if task.Deliverable == "code" {
		if facts.Error != "" || facts.HeadCommit == "" {
			err := fmt.Errorf("record attempt base commit: %s", facts.Error)
			s.failSetup(attempt, err)
			return result, err
		}
		if err := s.Store.SetAttemptBaseCommit(attempt.ID, facts.HeadCommit); err != nil {
			s.failSetup(attempt, err)
			return result, err
		}
	}
	if _, err := s.Store.RecordSystemCheckpoint(attempt.ID, generation, "Assignment accepted by Shephrd", []string{"Complete the task objective and acceptance criteria."}, facts); err != nil {
		s.failSetup(attempt, err)
		return result, err
	}
	var briefPath string
	var warnings []string
	if attempt.Number > 1 {
		briefPath, warnings, err = s.renderRecoveryBriefWithReports(task, repo, attempt, "clean retry", "Driver intentionally selected a new attempt and clean worktree.", reports)
	} else {
		briefPath, warnings, err = s.renderBriefWithReports(task, repo, attempt, reports)
	}
	if err != nil {
		s.failSetup(attempt, err)
		return result, err
	}
	if _, err := s.Store.AddOutboundForRun(task.ID, attempt.ID, generation, "assign", "Brief: "+briefPath); err != nil {
		s.failSetup(attempt, err)
		return result, err
	}
	if err := s.launch(attempt, briefPath, false); err != nil {
		latest, latestErr := s.Store.Attempt(attempt.ID)
		if latestErr != nil {
			latest = attempt
		}
		s.failSetup(latest, err)
		return result, err
	}
	if harness == "codex" {
		native, waitErr := s.awaitNativeSession(attempt.ID)
		if waitErr != nil {
			running, _ := s.Store.Attempt(attempt.ID)
			s.stopProcess(running.RunnerPID)
			s.failSetup(running, waitErr)
			return result, waitErr
		}
		attempt = native
	}
	task, _ = s.Store.Task(task.ID)
	attempt, _ = s.Store.Attempt(attempt.ID)
	return SpawnResult{Task: task, Attempt: attempt, BriefPath: briefPath, Label: workerLabelFor(task), Warnings: warnings}, nil
}

func (s Service) Send(taskID, text string) (domain.Attempt, error) {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return domain.Attempt{}, err
	}
	if task.CurrentAttemptID == "" {
		return domain.Attempt{}, fmt.Errorf("task %s has no attempt; run 'shephrd worker spawn %s' first", task.ID, task.ID)
	}
	attempt, err := s.Store.Attempt(task.CurrentAttemptID)
	if err != nil {
		return domain.Attempt{}, err
	}
	if err := s.Store.GuardReportRecoveryContinuation(task.ID, attempt.ID); err != nil {
		return domain.Attempt{}, err
	}
	if isTerminal(task.Status) {
		return domain.Attempt{}, fmt.Errorf("task %s is terminal (%s); use worker retry instead of worker send", task.ID, task.Status)
	}
	if s.processAlive(attempt.RunnerPID) {
		return domain.Attempt{}, fmt.Errorf("task %s worker is busy; follow-up was not queued or delivered and no hold was applied; wait for a question before sending a follow-up", task.ID)
	}
	if attempt.RunnerPID != 0 {
		if err := s.Store.ClearRunnerForRun(attempt.ID, attempt.RunGeneration); err != nil && !errors.Is(err, store.ErrStaleRun) {
			return domain.Attempt{}, err
		}
		attempt, err = s.Store.Attempt(attempt.ID)
		if err != nil {
			return domain.Attempt{}, err
		}
	}
	if task.Status != domain.TaskStatusWaiting {
		return domain.Attempt{}, fmt.Errorf("task %s is %s; follow-ups can be sent only while it is waiting", task.ID, task.Status)
	}
	if isTerminalRuntime(attempt.RuntimeBackend) {
		if err := s.requireTerminalEndpointGone(attempt); err != nil {
			return domain.Attempt{}, err
		}
		if err := s.preflightRuntime(attempt.RuntimeBackend); err != nil {
			return domain.Attempt{}, err
		}
	}
	if attempt.SessionID == "" || strings.HasPrefix(attempt.SessionID, "pending:") {
		return domain.Attempt{}, fmt.Errorf("attempt %s has no resumable harness session", attempt.ID)
	}
	projectContextPath, err := repository.ProjectContextPath(attempt.WorktreePath)
	if err != nil {
		return domain.Attempt{}, err
	}
	message, generation, err := s.Store.ReserveFollowUpRun(task.ID, attempt.ID, attempt.RunGeneration, text)
	if err != nil {
		return domain.Attempt{}, err
	}
	path := filepath.Join(s.Config.DataDir, task.ID, fmt.Sprintf("follow-up-%d.md", message.ID))
	attempt.RunGeneration = generation
	reportPath, err := s.assignReportDestination(task, attempt)
	if err != nil {
		_ = s.recordControlFailure(attempt.ID, generation, err.Error())
		return domain.Attempt{}, err
	}
	prompt := text + "\n" + brief.ProjectGuidance(projectContextPath, s.Config.Memory.Enabled)
	if reportPath != "" {
		prompt += fmt.Sprintf("\n## Current report run\n\n- Task: %s\n- Attempt: %s\n- Run generation: %d\n\n", task.ID, attempt.ID, generation) + brief.ReportDestination(reportPath)
	}
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		return domain.Attempt{}, fmt.Errorf("write follow-up: %w", err)
	}
	if err := s.launch(attempt, path, true); err != nil {
		_ = s.recordControlFailure(attempt.ID, generation, err.Error())
		return domain.Attempt{}, err
	}
	return s.Store.Attempt(attempt.ID)
}

func (s Service) Relaunch(taskID string) (result SpawnResult, err error) {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return SpawnResult{}, err
	}
	if task.CurrentAttemptID == "" {
		return SpawnResult{}, fmt.Errorf("task %s has no current attempt", taskID)
	}
	attempt, err := s.Store.Attempt(task.CurrentAttemptID)
	if err != nil {
		return SpawnResult{}, err
	}
	if err := s.Store.GuardReportRecoveryContinuation(task.ID, attempt.ID); err != nil {
		return SpawnResult{}, err
	}
	if task.Status == domain.TaskStatusDone {
		return SpawnResult{}, fmt.Errorf("task %s is done; recovery relaunch is not allowed", taskID)
	}
	reports, err := s.loadReportBriefs(task.ID)
	if err != nil {
		return SpawnResult{}, err
	}
	if attempt.ReleasedAt != nil || attempt.Status == domain.AttemptStatusSuperseded || attempt.Status == domain.AttemptStatusWorkspaceUnknown {
		return SpawnResult{}, fmt.Errorf("attempt %s is not eligible for same-worktree relaunch", attempt.ID)
	}
	if err := s.preflightRuntime(attempt.RuntimeBackend); err != nil {
		return SpawnResult{}, err
	}
	if s.processAlive(attempt.RunnerPID) {
		return SpawnResult{}, fmt.Errorf("attempt %s runner is still alive", attempt.ID)
	}
	repo, err := s.Store.Repo(task.RepoID)
	if err != nil {
		return SpawnResult{}, err
	}
	if err := s.workspaceBackend().VerifyHeld(repo, attempt); err != nil {
		return SpawnResult{}, err
	}
	if attempt.RunnerPID != 0 {
		if err := s.Store.ClearRunnerForRun(attempt.ID, attempt.RunGeneration); err != nil {
			return SpawnResult{}, err
		}
		attempt, err = s.Store.Attempt(attempt.ID)
		if err != nil {
			return SpawnResult{}, err
		}
	}
	if isTerminalRuntime(attempt.RuntimeBackend) {
		if err := s.finalizeTerminalEndpoint(attempt); err != nil {
			return SpawnResult{}, err
		}
	}
	generation, err := s.Store.ReserveRunGeneration(attempt.ID)
	if err != nil {
		return SpawnResult{}, err
	}
	attempt, err = s.Store.Attempt(attempt.ID)
	if err != nil {
		return SpawnResult{}, err
	}
	attempt.ReportPath, err = s.assignReportDestination(task, attempt)
	if err != nil {
		_ = s.recordControlFailure(attempt.ID, generation, err.Error())
		return SpawnResult{}, err
	}
	sessionID := adapter.NewSessionID(attempt.Harness)
	if err := s.Store.SetSessionForRun(attempt.ID, generation, sessionID); err != nil {
		_ = s.recordControlFailure(attempt.ID, generation, err.Error())
		return SpawnResult{}, err
	}
	attempt.SessionID = sessionID
	attempt.RunGeneration = generation
	if _, err := s.Store.AddOutboundForRun(task.ID, attempt.ID, generation, "relaunch", "Driver requested same-worktree recovery; preserve the existing filesystem state."); err != nil {
		_ = s.recordControlFailure(attempt.ID, generation, err.Error())
		return SpawnResult{}, err
	}
	facts := workspaceFacts(attempt.WorktreePath)
	if _, err := s.Store.RecordSystemCheckpoint(attempt.ID, generation, "Preparing same-worktree recovery", []string{"Continue from the latest checkpoint without resetting this worktree."}, facts); err != nil {
		_ = s.recordControlFailure(attempt.ID, generation, err.Error())
		return SpawnResult{}, err
	}
	briefPath, warnings, err := s.renderRecoveryBriefWithReports(task, repo, attempt, "same-worktree relaunch", "Driver requested a fresh session after the prior runner stopped.", reports)
	if err != nil {
		_ = s.recordControlFailure(attempt.ID, generation, err.Error())
		return SpawnResult{}, err
	}
	if err := s.workspaceBackend().VerifyHeld(repo, attempt); err != nil {
		_ = s.recordControlFailure(attempt.ID, generation, err.Error())
		return SpawnResult{}, err
	}
	if err := s.launch(attempt, briefPath, false); err != nil {
		_ = s.recordControlFailure(attempt.ID, generation, err.Error())
		return SpawnResult{}, err
	}
	task, _ = s.Store.Task(taskID)
	attempt, _ = s.Store.Attempt(attempt.ID)
	return SpawnResult{Task: task, Attempt: attempt, BriefPath: briefPath, Label: workerLabelFor(task), Warnings: warnings}, nil
}

func (s Service) RetryWithModelSelection(taskID, harness, modelID string, modelProvided bool, requestedRuntime ...string) (SpawnResult, error) {
	return s.RetryWithBaseSelection(taskID, harness, modelID, modelProvided, domain.BaseSelection{}, false, requestedRuntime...)
}

func (s Service) RetryWithBaseSelection(taskID, harness, modelID string, modelProvided bool, baseSelection domain.BaseSelection, baseProvided bool, requestedRuntime ...string) (SpawnResult, error) {
	if baseProvided {
		var err error
		baseSelection, err = domain.NormalizeBaseSelection(baseSelection)
		if err != nil {
			return SpawnResult{}, err
		}
	}
	task, err := s.Store.Task(taskID)
	if err != nil {
		return SpawnResult{}, err
	}
	reports, err := s.loadReportBriefs(task.ID)
	if err != nil {
		return SpawnResult{}, err
	}
	selection, err := s.selectRuntime(requestedRuntime...)
	if err != nil {
		return SpawnResult{}, err
	}
	workerRuntime := selection.Backend
	if isTerminalRuntime(workerRuntime) {
		if _, err := s.workerExecutable(); err != nil {
			return SpawnResult{}, err
		}
	}
	if task.CurrentAttemptID != "" {
		attempt, err := s.Store.Attempt(task.CurrentAttemptID)
		if err != nil {
			return SpawnResult{}, err
		}
		if s.processAlive(attempt.RunnerPID) {
			if isTerminal(task.Status) {
				return SpawnResult{}, fmt.Errorf("task %s is still finalizing its terminal result; retry after the worker process exits", task.ID)
			}
			if err := s.stopProcess(attempt.RunnerPID); err != nil {
				return SpawnResult{}, fmt.Errorf("stop current worker before retry: %w", err)
			}
			if err := s.Store.Stop(taskID, "Stopped for retry"); err != nil {
				return SpawnResult{}, err
			}
		}
		if isTerminalRuntime(attempt.RuntimeBackend) {
			if err := s.waitForTerminalEndpointGone(attempt); err != nil {
				return SpawnResult{}, err
			}
		}
		harness, modelID = s.resolveRetryWorkerSelection(attempt, harness, modelID, modelProvided)
		if !baseProvided {
			baseSelection = domain.BaseSelection{Strategy: attempt.BaseStrategy, Ref: attempt.BaseRef}
		}
	}
	if !baseProvided && task.CurrentAttemptID == "" {
		baseSelection = domain.DefaultBaseSelection()
	}
	if task.CurrentAttemptID != "" {
		if current, currentErr := s.Store.Attempt(task.CurrentAttemptID); currentErr == nil {
			if _, messageErr := s.Store.AddOutboundForRun(task.ID, current.ID, current.RunGeneration, "retry", "Clean retry requested by driver; filesystem state will not be transferred implicitly."); messageErr != nil {
				return SpawnResult{}, messageErr
			}
		}
	}
	if err := s.Store.PrepareRetry(taskID); err != nil {
		return SpawnResult{}, err
	}
	task, err = s.Store.Task(taskID)
	if err != nil {
		return SpawnResult{}, err
	}
	return s.spawnWithModel(task, harness, modelID, reports, baseSelection, workerRuntime)
}

func (s Service) Stop(taskID, reason string, discard bool) error {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return err
	}
	if isTerminal(task.Status) {
		return fmt.Errorf("task %s is already terminal (%s); use worker retry to create a new attempt", task.ID, task.Status)
	}
	if task.CurrentAttemptID != "" {
		attempt, err := s.Store.Attempt(task.CurrentAttemptID)
		if err != nil {
			return err
		}
		if err := s.stopProcess(attempt.RunnerPID); err != nil {
			return fmt.Errorf("stop worker process: %w", err)
		}
		if isTerminalRuntime(attempt.RuntimeBackend) {
			if err := s.waitForTerminalEndpointGone(attempt); err != nil {
				return err
			}
		}
	}
	if reason == "" {
		reason = "Stopped by driver"
	}
	if err := s.Store.Stop(taskID, reason); err != nil {
		return err
	}
	if discard && task.CurrentAttemptID != "" {
		return s.Release(taskID, task.CurrentAttemptID, true)
	}
	return nil
}

func (s Service) AttestDelivery(taskID, attemptID, driverID, prURL, sealedCommit string, explicitAttempt bool) (AttestDeliveryResult, error) {
	if s.workerContext() {
		return AttestDeliveryResult{}, domain.Failure("worker_context_forbidden", "external delivery attestation is driver-only and cannot run in worker context")
	}
	candidate, err := s.Store.DeliveryAttestationCandidate(taskID, attemptID, driverID, sealedCommit, explicitAttempt)
	if err != nil {
		return AttestDeliveryResult{}, err
	}
	if candidate.Existing != nil {
		if candidate.Existing.PRURL != prURL || candidate.Existing.SealedCommit != sealedCommit {
			return AttestDeliveryResult{}, domain.Failure("attestation_conflict", "attempt %s already has an immutable external delivery attestation for %s", candidate.Attempt.ID, candidate.Existing.PRURL)
		}
		return attestDeliveryResult(candidate, *candidate.Existing, true), nil
	}
	evidence, err := s.Verifier.ValidateExternalDelivery(candidate.Repo, prURL, sealedCommit)
	if err != nil {
		return AttestDeliveryResult{}, err
	}
	stored, idempotent, err := s.Store.RecordExternalDeliveryAttestation(candidate, evidence)
	if err != nil {
		return AttestDeliveryResult{}, err
	}
	return attestDeliveryResult(candidate, stored, idempotent), nil
}

func attestDeliveryResult(candidate domain.DeliveryAttestationCandidate, attestation domain.ExternalDeliveryAttestation, idempotent bool) AttestDeliveryResult {
	relation := "ancestor"
	if attestation.SealedCommit == attestation.PRHeadCommit {
		relation = "identical"
	}
	next := "shephrd task verify-delivery " + candidate.Task.ID + " --json"
	if candidate.Task.CurrentAttemptID != candidate.Attempt.ID {
		next = "shephrd task verify-delivery " + candidate.Task.ID + " --attempt " + candidate.Attempt.ID + " --json"
	}
	return AttestDeliveryResult{SchemaVersion: 1, TaskID: candidate.Task.ID, AttemptID: candidate.Attempt.ID,
		AttestationID: attestation.ID, Idempotent: idempotent, OriginalArtifactRef: attestation.OriginalArtifactRef,
		SealedCommit: attestation.SealedCommit, CheckpointRevision: attestation.CheckpointRevision, PRURL: attestation.PRURL,
		PRHeadCommit: attestation.PRHeadCommit, CommitRelation: relation, MergeCommit: attestation.MergeCommit,
		TargetRef: "refs/heads/" + attestation.RegisteredDefaultBranch, LandingProven: candidate.Attempt.LandedProven,
		ReleaseState: candidate.Attempt.ReleaseState, VerificationEnabled: true, NextCommand: next}
}

func (s Service) AttestReportRecovery(taskID, attemptID, driverID, reason string, runGeneration, checkpointRevision int, checkpointCursor int64) (AttestReportRecoveryResult, error) {
	if s.workerContext() {
		return AttestReportRecoveryResult{}, domain.Failure("worker_context_forbidden", "report recovery attestation is driver-only and cannot run in worker context")
	}
	if err := store.ValidateReportRecoveryReason(reason); err != nil {
		return AttestReportRecoveryResult{}, err
	}
	candidate, err := s.Store.ReportRecoveryCandidateFor(taskID, attemptID, driverID, runGeneration, checkpointRevision, checkpointCursor)
	if err != nil {
		return AttestReportRecoveryResult{}, err
	}
	if s.processAlive(candidate.Attempt.RunnerPID) {
		return AttestReportRecoveryResult{}, reportRecoveryEvidenceFailure(candidate, "worker_process_alive", "attempt %s worker process is still alive", candidate.Attempt.ID)
	}
	gone, err := s.endpointGone(candidate.Attempt)
	if err != nil {
		return AttestReportRecoveryResult{}, err
	}
	if !gone {
		return AttestReportRecoveryResult{}, domain.EvidenceFailure("terminal_endpoint_alive", map[string]string{
			"recovery_command":  candidate.RecoveryCommand,
			"reconcile_command": "shephrd workspace reconcile --json",
			"inspect_command":   "shephrd task inspect " + candidate.Task.ID + " --json",
		}, "attempt %s exact terminal endpoint is still present", candidate.Attempt.ID)
	}
	if err := s.validateReportRecoveryWorkspace(candidate); err != nil {
		return AttestReportRecoveryResult{}, reportRecoveryEvidenceFailure(candidate, classifiedErrorKind(err), "%v", err)
	}
	file, err := s.Verifier.ValidateReportRecovery(candidate.Task.ID, candidate.Attempt)
	if err != nil {
		return AttestReportRecoveryResult{}, reportRecoveryEvidenceFailure(candidate, "report_file_invalid", "%v", err)
	}
	evidence := domain.ReportRecoveryAttestation{CanonicalReportPath: file.CanonicalPath, FileIdentity: file.FileIdentity,
		FileMode: file.FileMode, FileModTimeUnixNano: file.FileModTimeUnixNano, SHA256: file.SHA256, SizeBytes: file.SizeBytes,
		Reason: strings.TrimSpace(reason), ValidationKind: file.ValidationKind, EvidenceValidatedAt: time.Now().UTC()}
	confirmed, err := s.Verifier.ValidateReportRecovery(candidate.Task.ID, candidate.Attempt)
	if err != nil || !delivery.SameReportFileEvidence(evidence, confirmed) {
		return AttestReportRecoveryResult{}, reportRecoveryEvidenceFailure(candidate, "report_file_changed", "canonical report identity or bytes changed during attestation")
	}
	stored, idempotent, err := s.Store.RecordReportRecoveryAttestation(candidate, evidence)
	if err != nil {
		return AttestReportRecoveryResult{}, err
	}
	return attestReportRecoveryResult(candidate, stored, idempotent), nil
}

func classifiedErrorKind(err error) string {
	type classified interface {
		ErrorKind() string
	}
	var typed classified
	if errors.As(err, &typed) {
		return typed.ErrorKind()
	}
	return ""
}

func reportRecoveryEvidenceFailure(candidate domain.ReportRecoveryCandidate, kind, format string, args ...any) error {
	if kind == "" {
		kind = "report_recovery_failed"
	}
	return domain.EvidenceFailure(kind, map[string]string{
		"recovery_command": candidate.RecoveryCommand,
		"inspect_command":  "shephrd task inspect " + candidate.Task.ID + " --json",
	}, format, args...)
}

func attestReportRecoveryResult(candidate domain.ReportRecoveryCandidate, attestation domain.ReportRecoveryAttestation, idempotent bool) AttestReportRecoveryResult {
	next := fmt.Sprintf("shephrd task verify-delivery %s --attempt %s --driver-id %s --json", candidate.Task.ID, candidate.Attempt.ID, domain.ShellQuote(candidate.Task.DriverID))
	return AttestReportRecoveryResult{SchemaVersion: 1, TaskID: candidate.Task.ID, AttemptID: candidate.Attempt.ID,
		RunGeneration: attestation.RunGeneration, AttestationID: attestation.ID, Idempotent: idempotent,
		CheckpointRevision: attestation.CheckpointRevision, CheckpointSourceCursor: attestation.CheckpointSourceCursor,
		CheckpointSessionID: attestation.CheckpointSessionID, CheckpointBranch: attestation.CheckpointBranch,
		CheckpointHeadCommit: attestation.CheckpointHeadCommit, CheckpointDirty: attestation.CheckpointWorktreeDirty,
		CheckpointWorkspaceFactsError: attestation.CheckpointWorkspaceFactsError, WorkspaceBackend: attestation.WorkspaceBackend, WorktreePath: attestation.WorktreePath,
		CanonicalReportPath: attestation.CanonicalReportPath, FileIdentity: attestation.FileIdentity, SHA256: attestation.SHA256,
		SizeBytes: attestation.SizeBytes, Reason: attestation.Reason, AttestedByDriverID: attestation.AttestedByDriverID,
		CompletionProvenance: domain.CompletionProvenanceReportRecovery, LandingProven: candidate.Attempt.LandedProven,
		ReleaseState: candidate.Attempt.ReleaseState, VerificationEnabled: true, NextCommand: next}
}

func (s Service) Verify(taskID string) (delivery.Result, error) {
	return s.VerifyAttempt(taskID, "", "")
}

func (s Service) VerifyAttempt(taskID, attemptID, driverID string) (delivery.Result, error) {
	return s.verificationBoundary().Remote(taskID, attemptID, driverID)
}

func (s Service) VerifyLocal(taskID, attemptID, driverID string) (delivery.Result, error) {
	return s.verificationBoundary().Local(taskID, attemptID, driverID)
}

func (s Service) verifyDelivery(task domain.Task, attempt domain.Attempt) (delivery.Result, error) {
	return s.verificationBoundary().VerifyDelivery(task, attempt)
}

func (s Service) verificationBoundary() verify.Boundary {
	dispatcher := lifecycle.NewDispatcher(s.Store, s.Verifier, s.Config.LifecycleHandlers.ReportAccepted, s.environ())
	dispatcher.Now = s.now
	return verify.Boundary{Store: s.Store, Verifier: s.Verifier, FinalizeEndpoint: s.finalizeTerminalEndpoint,
		Release: func(taskID, attemptID string) error { return s.Release(taskID, attemptID, false) }, WorkerContext: s.workerContext,
		ValidateReportRecoveryWorkspace: s.validateReportRecoveryWorkspace, ReportRecoveryEndpointGone: s.endpointGone,
		ReportAccepted: dispatcher, PresentReport: s.presentAcceptedReport, PresentReportVerificationFailure: s.notifyMessage}
}

func (s Service) validateReportRecoveryWorkspace(candidate domain.ReportRecoveryCandidate) error {
	if candidate.Attempt.ReleaseState == domain.WorkspaceStateReleased {
		return nil
	}
	identity, err := s.Native.VerifyHeld(candidate.Repo, candidate.Attempt)
	if err != nil {
		return domain.Failure("workspace_identity_mismatch", "verify exact held worktree: %v", err)
	}
	dirty, err := s.Native.Dirty(candidate.Attempt.WorktreePath)
	if err != nil {
		return domain.Failure("workspace_identity_mismatch", "%v", err)
	}
	if identity.Head != candidate.Checkpoint.HeadCommit || dirty != candidate.Checkpoint.WorktreeDirty {
		return domain.Failure("checkpoint_workspace_changed", "attempt %s worktree HEAD or dirty state no longer matches final checkpoint revision %d", candidate.Attempt.ID, candidate.Checkpoint.Revision)
	}
	for _, pid := range s.Native.LiveProcessPIDs(candidate.Attempt.WorktreePath) {
		if pid != candidate.Attempt.RunnerPID && s.processAlive(pid) {
			return domain.Failure("worker_process_alive", "attempt %s worktree still has live process %d", candidate.Attempt.ID, pid)
		}
	}
	return nil
}

func (s Service) workerContext() bool {
	for _, name := range []string{"SHEPHRD_ATTEMPT_ID", "SHEPHRD_RUN_GENERATION", "SHEPHRD_BRIDGE_ATTEMPT_ID", "SHEPHRD_CLAUDE_BRIDGE_ATTEMPT_ID"} {
		if strings.TrimSpace(s.getenv(name)) != "" {
			return true
		}
	}
	return false
}

func (s Service) ResolveWorkspaceAttempt(taskID, attemptID string) (domain.Attempt, error) {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return domain.Attempt{}, err
	}
	if attemptID == "" {
		attemptID = task.CurrentAttemptID
	}
	if attemptID == "" {
		return domain.Attempt{}, fmt.Errorf("task %s has no workspace attempt", task.ID)
	}
	attempt, err := s.Store.Attempt(attemptID)
	if err != nil {
		return domain.Attempt{}, err
	}
	if attempt.TaskID != task.ID {
		return domain.Attempt{}, fmt.Errorf("attempt %s does not belong to task %s", attempt.ID, task.ID)
	}
	return attempt, nil
}

func (s Service) Release(taskID, attemptID string, discard bool) error {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return err
	}
	attempt, err := s.ResolveWorkspaceAttempt(task.ID, attemptID)
	if err != nil {
		return err
	}
	attemptID = attempt.ID
	if attempt.Status == domain.AttemptStatusWorkspaceUnknown || attempt.WorkspaceState == domain.WorkspaceStateUnknown {
		return fmt.Errorf("attempt %s has an unprovable workspace identity; reconcile identity before releasing it", attempt.ID)
	}
	if attempt.WorkspaceState == domain.WorkspaceStateAllocating {
		return fmt.Errorf("attempt %s workspace allocation is unresolved; run 'shephrd workspace reconcile' to classify it before releasing", attempt.ID)
	}
	if attempt.ReleaseState == "no_workspace" {
		// The acquisition failed before any worktree or lease existed; there
		// is nothing to release and nothing to discard.
		return nil
	}
	if s.processAlive(attempt.RunnerPID) {
		return fmt.Errorf("attempt %s is still running; stop it before releasing its worktree", attempt.ID)
	}
	if err := s.finalizeTerminalEndpoint(attempt); err != nil {
		return err
	}
	if discard {
		if err := s.Store.AuthorizeDiscard(task.ID, attempt.ID); err != nil {
			return err
		}
		attempt.DiscardAuthorized = true
	}
	authorized := attempt.DiscardAuthorized
	if !authorized {
		if _, proofErr := s.Store.CompleteLandingProof(attempt.ID); proofErr == nil {
			authorized = true
		} else if attempt.LandedProven {
			return fmt.Errorf("refusing to release attempt %s: %v; use an explicit discard to authorize cleanup without landing evidence", attempt.ID, proofErr)
		}
	}
	if !authorized {
		return fmt.Errorf("refusing to release attempt %s: no validated landing/report proof or explicit discard authorization is recorded", attempt.ID)
	}
	claimed, state, err := s.Store.ClaimRelease(attempt.ID)
	if err != nil {
		return err
	}
	if !claimed {
		if state == "released" {
			return nil
		}
		if state != "releasing" {
			return fmt.Errorf("attempt %s has invalid release state %q", attempt.ID, state)
		}
		deadline := s.now().Add(5 * time.Second)
		for s.now().Before(deadline) {
			current, currentErr := s.Store.Attempt(attempt.ID)
			if currentErr != nil {
				return currentErr
			}
			if current.ReleaseState == "released" {
				return nil
			}
			if current.ReleaseState == "held" {
				return s.Release(taskID, attemptID, false)
			}
			s.sleep(10 * time.Millisecond)
		}
		return fmt.Errorf("attempt %s release is still in progress", attempt.ID)
	}
	repo, err := s.Store.Repo(task.RepoID)
	if err != nil {
		_ = s.Store.ResetReleaseClaim(attempt.ID)
		return err
	}
	reason, err := s.workspaceBackend().Release(repo, attempt, attempt.DiscardAuthorized)
	if err != nil {
		return err
	}
	return s.Store.MarkReleasedWithReason(attempt.ID, reason)
}

func (s Service) SweepDeaths() error {
	attempts, err := s.Store.RecordedRunnerAttempts()
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		if s.processAlive(attempt.RunnerPID) {
			continue
		}
		if attempt.Status != domain.AttemptStatusStarting && attempt.Status != domain.AttemptStatusWorking {
			if err := s.Store.FinishDeadRunnerForRun(attempt.ID, attempt.RunGeneration); err != nil && !errors.Is(err, store.ErrStaleRun) {
				return err
			}
			continue
		}
		reason := fmt.Sprintf("worker runner pid %d died before a terminal result", attempt.RunnerPID)
		if err := s.recordRunnerDeath(attempt.ID, attempt.RunGeneration, reason); err != nil && !errors.Is(err, store.ErrStaleRun) {
			return err
		}
	}
	return nil
}

func (s Service) Reconcile() (ReconcileResult, error) {
	result := ReconcileResult{SchemaVersion: 1, MutatesRecoveryState: true, Verified: make([]string, 0), Unknown: make([]string, 0), Classifications: make([]ReconcileEntry, 0)}
	if err := s.SweepDeaths(); err != nil {
		return result, err
	}
	counts, err := s.Store.NotificationCounts()
	if err != nil {
		return result, err
	}
	result.NotificationCounts = counts
	attempts, err := s.Store.NativeWorkspaceAttempts()
	if err != nil {
		return result, err
	}
	backend := s.workspaceBackend()
	for _, attempt := range attempts {
		task, err := s.Store.Task(attempt.TaskID)
		if err != nil {
			return result, err
		}
		repo, err := s.Store.Repo(task.RepoID)
		if err != nil {
			return result, err
		}
		classification, err := backend.Reconcile(repo, task, attempt)
		if err != nil {
			return result, err
		}
		if classification.Verified {
			result.Verified = append(result.Verified, attempt.ID)
		}
		if classification.Unknown {
			result.Unknown = append(result.Unknown, attempt.ID)
		}
		result.Classifications = append(result.Classifications, classification.Entry)
	}
	return result, nil
}

func (s Service) workerEnvironment(extra []string) []string {
	candidates := append([]string{"SHEPHRD_PI_WATCHER_ENABLED=0", "SHEPHRD_WORKER=1"}, extra...)
	replacements := make([]string, 0, len(candidates))
	keys := make(map[string]struct{}, len(candidates))
	for _, value := range candidates {
		if index := strings.IndexByte(value, '='); index > 0 {
			key := value[:index]
			if driverContextEnvironment(key) {
				continue
			}
			keys[key] = struct{}{}
		}
		replacements = append(replacements, value)
	}
	base := s.environ()
	environment := make([]string, 0, len(base)+len(replacements))
	for _, value := range base {
		key := value
		if index := strings.IndexByte(value, '='); index >= 0 {
			key = value[:index]
		}
		if driverContextEnvironment(key) {
			continue
		}
		if _, replaced := keys[key]; !replaced {
			environment = append(environment, value)
		}
	}
	return append(environment, replacements...)
}

func (s Service) awaitNativeSession(attemptID string) (domain.Attempt, error) {
	deadline := s.now().Add(10 * time.Second)
	for s.now().Before(deadline) {
		attempt, err := s.Store.Attempt(attemptID)
		if err != nil {
			return domain.Attempt{}, err
		}
		if attempt.SessionID != "" && !strings.HasPrefix(attempt.SessionID, "pending:") {
			return attempt, nil
		}
		if !s.processAlive(attempt.RunnerPID) {
			return attempt, fmt.Errorf("Codex exited before reporting a native thread ID")
		}
		s.sleep(25 * time.Millisecond)
	}
	attempt, _ := s.Store.Attempt(attemptID)
	return attempt, fmt.Errorf("Codex did not report a native thread ID within 10s")
}

func (s Service) launchHeadless(attempt domain.Attempt, inputPath, executable string, resume bool) error {
	if err := os.MkdirAll(filepath.Dir(inputPath), 0o700); err != nil {
		return err
	}
	args := []string{"_run", attempt.ID, "--input", inputPath, "--run-generation", fmt.Sprint(attempt.RunGeneration)}
	if resume {
		args = append(args, "--resume")
	}
	logPath := filepath.Join(s.Config.DataDir, attempt.TaskID, fmt.Sprintf("attempt-%d-runner.log", attempt.Number))
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open runner log: %w", err)
	}
	defer log.Close()
	cmd := exec.Command(executable, args...)
	environment := map[string]string{"SHEPHRD_EXECUTABLE": executable, "SHEPHRD_RUNNER_PROCESS": "1"}
	if attempt.Harness == "pi" {
		environment["SHEPHRD_PI_WATCHER_ENABLED"] = "0"
	}
	cmd.Env = s.mergedEnvironment(environment)
	cmd.Stdout, cmd.Stderr = log, log
	s.detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start worker runner: %w", err)
	}
	if err := s.Store.SetRunnerForRun(attempt.ID, attempt.RunGeneration, cmd.Process.Pid, true); err != nil {
		s.stopProcess(cmd.Process.Pid)
		return err
	}
	return cmd.Process.Release()
}

func (s Service) renderBrief(task domain.Task, repo domain.Repo, attempt domain.Attempt) (string, []string, error) {
	reports, err := s.loadReportBriefs(task.ID)
	if err != nil {
		return "", nil, err
	}
	return s.renderBriefWithReports(task, repo, attempt, reports)
}

func (s Service) renderBriefWithReports(task domain.Task, repo domain.Repo, attempt domain.Attempt, reports []brief.Report) (string, []string, error) {
	mode, reason := "initial assignment", "Driver assigned this task for the first run."
	if attempt.Number > 1 {
		mode, reason = "clean retry", "Driver intentionally selected a new attempt and clean worktree."
	}
	return s.renderBriefAt(task, repo, attempt, filepath.Join(s.Config.DataDir, task.ID, "brief.md"), mode, reason, reports)
}

func (s Service) renderRecoveryBrief(task domain.Task, repo domain.Repo, attempt domain.Attempt, mode, reason string) (string, []string, error) {
	reports, err := s.loadReportBriefs(task.ID)
	if err != nil {
		return "", nil, err
	}
	return s.renderRecoveryBriefWithReports(task, repo, attempt, mode, reason, reports)
}

func (s Service) renderRecoveryBriefWithReports(task domain.Task, repo domain.Repo, attempt domain.Attempt, mode, reason string, reports []brief.Report) (string, []string, error) {
	path := filepath.Join(s.Config.DataDir, task.ID, fmt.Sprintf("resume-brief-attempt-%d-run-%d.md", attempt.Number, attempt.RunGeneration))
	return s.renderBriefAt(task, repo, attempt, path, mode, reason, reports)
}

func (s Service) renderBriefAt(task domain.Task, repo domain.Repo, attempt domain.Attempt, path, mode, reason string, reports []brief.Report) (string, []string, error) {
	dir := filepath.Join(s.Config.DataDir, task.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create task data directory: %w", err)
	}
	var checkpoint *domain.AttemptCheckpoint
	storedCheckpoint, checkpointErr := s.Store.LatestCheckpoint(attempt.ID)
	if checkpointErr == nil {
		checkpoint = &storedCheckpoint
	}
	sourceWorkerCheckpoint := false
	if attempt.ResumeSourceAttemptID != "" {
		if source, sourceErr := s.Store.LatestCheckpoint(attempt.ResumeSourceAttemptID); sourceErr == nil && source.Revision == attempt.ResumeSourceRevision && source.Producer == "worker" {
			sourceWorkerCheckpoint = true
		}
	}
	var latestAnnotation *domain.Annotation
	if mode != "initial assignment" {
		annotation, annotationErr := s.Store.LatestAnnotation(domain.AnnotationScope{TaskID: task.ID})
		if annotationErr != nil {
			return "", nil, annotationErr
		}
		latestAnnotation = annotation
	}
	facts := workspaceFacts(attempt.WorktreePath)
	messages, err := s.Store.Messages(task.ID)
	if err != nil {
		return "", nil, err
	}
	reportPath, err := s.Verifier.CanonicalReportPath(task.ID, attempt)
	if err != nil {
		return "", nil, err
	}
	readmePresent := false
	if _, err := os.Stat(filepath.Join(attempt.WorktreePath, "README.md")); err == nil {
		readmePresent = true
	}
	projectContextPath, err := repository.ProjectContextPath(attempt.WorktreePath)
	if err != nil {
		return "", nil, err
	}
	rendered := brief.Render(brief.Input{
		Task: task, Repo: repo, Attempt: attempt, ReportPath: reportPath,
		Mode: mode, Reason: reason, Reports: reports, Checkpoint: checkpoint, LatestAnnotation: latestAnnotation,
		SourceWorkerCheckpoint: sourceWorkerCheckpoint, WorkspaceFacts: facts,
		Messages: messages, ReadmePresent: readmePresent,
		ProjectContextPath: projectContextPath, MemoryEnabled: s.Config.Memory.Enabled,
	})
	if err := os.WriteFile(path, rendered.Content, 0o600); err != nil {
		return "", nil, fmt.Errorf("write brief: %w", err)
	}
	return path, rendered.Warnings, nil
}

func (s Service) assignReportDestination(task domain.Task, attempt domain.Attempt) (string, error) {
	if task.Deliverable != "report" {
		return "", nil
	}
	path, err := s.Store.AssignReportDestination(task.ID, attempt.ID, attempt.RunGeneration, s.Config.DataDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create report directory: %w", err)
	}
	return path, nil
}

func (s Service) ingestRunnerEvent(attempt domain.Attempt, generation int, event domain.Event, cursor int64) (domain.Message, error) {
	facts := domain.WorkspaceFacts{}
	if event.Checkpoint != nil {
		facts = workspaceFacts(attempt.WorktreePath)
	}
	message, err := s.Store.AddEventForRun(attempt.ID, generation, event, cursor, facts)
	if message.Wake {
		s.notifyMessage(message)
	}
	return message, err
}

func (s Service) recordControlFailure(attemptID string, generation int, reason string) error {
	message, err := s.Store.RecordControlFailureMessage(attemptID, generation, reason)
	if err == nil {
		s.notifyMessage(message)
	}
	return err
}

func (s Service) recordRunnerDeath(attemptID string, generation int, reason string) error {
	message, err := s.Store.RecordRunnerDeathMessage(attemptID, generation, reason)
	if err == nil {
		s.notifyMessage(message)
	}
	return err
}

func (s Service) notifyMessage(message domain.Message) {
	if s.Notifier == nil || !message.Wake {
		return
	}
	notice, err := s.Store.Notification("wake:" + fmt.Sprint(message.ID))
	if err != nil {
		return
	}
	presentable, err := s.Store.NotificationPresentable(notice.NotificationID)
	if err != nil || !presentable {
		return
	}
	recorded, err := s.Store.NotificationPresentationRecorded(notice.NotificationID)
	if err != nil || recorded {
		return
	}
	summary := ""
	if s.Config.Notifications.Details {
		parts := make([]string, 0, len(notice.ReportLifecycle))
		for _, invocation := range notice.ReportLifecycle {
			if invocation.Annotation != "" {
				parts = append(parts, invocation.Annotation)
			} else if invocation.FailureMessage != "" {
				parts = append(parts, invocation.HandlerName+": "+invocation.FailureMessage)
			}
		}
		if len(parts) == 0 {
			summary = message.Payload
		} else {
			summary = strings.Join(parts, "; ")
		}
	}
	result, notifyErr := s.Notifier.Notify(context.Background(), notification.Notice{ID: notice.NotificationID, TaskID: notice.TaskID, TaskLabel: notice.TaskLabel, Kind: notice.Kind, ShortSummary: summary, CreatedAt: notice.CreatedAt, PayloadFingerprint: notification.Fingerprint(message.Payload + "\x00" + summary)})
	log := domain.NotificationDeliveryLog{NotificationID: notice.NotificationID, Operation: "notify", Result: "sent", Detail: fmt.Sprintf("extension %s presented=%t", s.Notifier.Name(), result.Presented)}
	if notifyErr != nil {
		log.Result = "failed"
		log.Detail = notifyErr.Error()
		if notification.IsSuppressed(notifyErr) {
			log.Operation = "dedupe"
			log.Result = "suppressed"
		}
	}
	_ = s.Store.RecordNotificationDelivery(log)
}

func (s Service) presentAcceptedReport(artifact domain.VerifiedArtifact) {
	message, err := s.Store.AcceptedDoneMessage(artifact.ProducerTaskID, artifact.ProducerAttemptID)
	if err == nil && message.ID == artifact.DoneMessageID {
		s.notifyMessage(message)
	}
}

func (s Service) failSetup(attempt domain.Attempt, cause error) {
	generation := attempt.RunGeneration
	_ = s.recordControlFailure(attempt.ID, generation, cause.Error())
	_ = s.Store.FinishRunnerForRun(attempt.ID, generation, -1, cause.Error())
}

func (s Service) endWithoutResult(attempt domain.Attempt, generation, exitCode int, reason string) error {
	_ = s.recordControlFailure(attempt.ID, generation, reason)
	_ = s.Store.FinishRunnerForRun(attempt.ID, generation, exitCode, reason)
	return errors.New(reason)
}

func isTerminal(status string) bool {
	return domain.IsTerminalTaskStatus(status)
}
