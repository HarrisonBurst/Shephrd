package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/brief"
	"shephrd/internal/delivery"
	"shephrd/internal/model"
	"shephrd/internal/pibridge"
	"shephrd/internal/runner"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
)

var interactivePiShutdownGrace = 10 * time.Second
var interactivePiRepairTimeout = 120 * time.Second
var headlessRepairTimeout = 120 * time.Second

func (s Service) RunAttempt(attemptID, inputPath string, resume bool, raw io.Writer, generation int) error {
	if generation < 1 {
		return fmt.Errorf("run generation must be at least 1")
	}
	attempt, err := s.Store.Attempt(attemptID)
	if err != nil {
		return err
	}
	if generation != attempt.RunGeneration {
		return fmt.Errorf("attempt %s run generation %d is no longer current", attempt.ID, generation)
	}
	if s.getenv("SHEPHRD_RUNNER_PROCESS") == "1" {
		if err := s.containProcess(); err != nil {
			return s.endWithoutResult(attempt, generation, -1, fmt.Sprintf("initialize worker process containment: %v", err))
		}
	}
	prompt, err := os.ReadFile(inputPath)
	if err != nil {
		reason := fmt.Errorf("read worker input %s: %w", inputPath, err)
		return s.endWithoutResult(attempt, generation, -1, reason.Error())
	}
	task, err := s.Store.Task(attempt.TaskID)
	if err != nil {
		return err
	}
	if !s.Config.Memory.Enabled {
		prompt = append(prompt, []byte("\n\n## Current memory setting\n\n"+brief.MemoryDisabledInstruction+"\n")...)
	}
	var runErr error
	if isTerminalRuntime(attempt.RuntimeBackend) && attempt.Harness == "pi" {
		runErr = s.runInteractivePiAttempt(attempt, task, generation, string(prompt), resume, raw)
	} else if isTerminalRuntime(attempt.RuntimeBackend) && attempt.Harness == "claude-code" {
		runErr = s.runInteractiveClaudeAttempt(attempt, task, generation, string(prompt), resume, raw)
	} else {
		runErr = s.runHeadlessAttempt(attempt, task, generation, string(prompt), resume, raw)
	}
	if s.getenv("SHEPHRD_TERMINAL_OWNED") == "1" && isTerminalRuntime(attempt.RuntimeBackend) {
		if cleanupErr := s.closeOwnedTerminalEndpoint(attempt, generation); cleanupErr != nil {
			_ = s.Store.FinishRunnerForRun(attempt.ID, generation, -1, cleanupErr.Error())
			return errors.Join(runErr, cleanupErr)
		}
	}
	return runErr
}

func (s Service) closeOwnedTerminalEndpoint(attempt model.Attempt, generation int) error {
	latest, err := s.Store.Attempt(attempt.ID)
	if err != nil {
		return err
	}
	if latest.RunGeneration != generation {
		return fmt.Errorf("attempt %s terminal cleanup generation is stale", attempt.ID)
	}
	client, err := s.terminalClientFor(latest)
	if err != nil {
		return err
	}
	info, err := client.ProcessInfo(s.endpoint(latest))
	if err != nil {
		return fmt.Errorf("verify terminal cleanup process attribution: %w", err)
	}
	owned := false
	for _, process := range info.ForegroundProcesses {
		if process.PID == os.Getpid() {
			owned = true
			break
		}
	}
	if !owned {
		return fmt.Errorf("terminal cleanup process is not attributed to the exact endpoint")
	}
	probe := latest
	probe.RuntimeExecutable = ""
	if foregroundWorker(info, probe) {
		return fmt.Errorf("terminal endpoint still has a harness process during cleanup")
	}
	if err := client.Close(s.endpoint(latest)); err != nil {
		return fmt.Errorf("close exact terminal endpoint: %w", err)
	}
	return nil
}

func (s Service) runHeadlessAttempt(attempt model.Attempt, task model.Task, generation int, prompt string, resume bool, raw io.Writer) error {
	invocation, err := adapter.BuildNamed(attempt.Harness, attempt, prompt, resume, workerSessionNameFor(task))
	if err != nil {
		return s.endWithoutResult(attempt, generation, -1, err.Error())
	}
	logPath := filepath.Join(s.Config.DataDir, attempt.TaskID, fmt.Sprintf("attempt-%d-runner.log", attempt.Number))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		_ = s.recordControlFailure(attempt.ID, generation, err.Error())
		return err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		reason := fmt.Errorf("open runner log: %w", err)
		_ = s.recordControlFailure(attempt.ID, generation, reason.Error())
		return reason
	}
	defer log.Close()
	if raw == nil {
		raw = io.Discard
	}
	banner := ""
	if isTerminalRuntime(attempt.RuntimeBackend) {
		banner = fmt.Sprintf("%s\n%s\nHarness: %s, attempt: %d\n\n", workerLabelFor(task), runner.Text(task.Objective), attempt.Harness, attempt.Number)
	}
	outcome, runErr := runner.RunHeadless(runner.HeadlessConfig{
		Attempt: attempt, Invocation: invocation,
		RepairInvocation: func(sessionID, repairPrompt string) (adapter.Invocation, error) {
			repairAttempt := attempt
			repairAttempt.SessionID = sessionID
			return adapter.BuildNamed(attempt.Harness, repairAttempt, repairPrompt, true, workerSessionNameFor(task))
		},
		RepairTimeout: headlessRepairTimeout, Resume: resume, Log: log, Raw: raw, Terminal: isTerminalRuntime(attempt.RuntimeBackend), Banner: banner,
		Environment: append(s.mergedEnvironment(invocation.Environment), "SHEPHRD_WORKER=1"), State: s.runnerState(attempt, generation),
	})
	if runErr != nil {
		return runErr
	}
	currentRun := false
	if latest, latestErr := s.Store.Attempt(attempt.ID); latestErr == nil {
		currentRun = latest.RunGeneration == generation && latest.ID == attempt.ID
	}
	currentTask, taskErr := s.Store.Task(attempt.TaskID)
	if currentRun && taskErr == nil && currentTask.Status == model.TaskStatusDone {
		var result delivery.Result
		if isTerminalRuntime(attempt.RuntimeBackend) {
			result, err = s.verifyDelivery(currentTask, attempt)
		} else {
			result, err = s.Verify(currentTask.ID)
		}
		if err != nil {
			fmt.Fprintln(log, err)
			if isTerminalRuntime(attempt.RuntimeBackend) {
				fmt.Fprintln(raw, "Delivery verification pending: "+err.Error())
			}
		} else {
			encoded, _ := json.Marshal(result)
			fmt.Fprintln(log, string(encoded))
			if isTerminalRuntime(attempt.RuntimeBackend) {
				fmt.Fprintln(raw, "Delivery: "+runner.Text(result.Reason))
				writeLifecycleHandlerResults(raw, result)
			}
		}
	}
	if !currentRun {
		return outcome.WaitError
	}
	if isTerminalRuntime(attempt.RuntimeBackend) {
		state := "blocked"
		if current, stateErr := s.Store.Task(attempt.TaskID); stateErr == nil {
			state = current.Status
		}
		if client, clientErr := s.terminalClientFor(attempt); clientErr == nil {
			_ = client.ReportMetadata(s.endpoint(attempt), workerLabelFor(task), attempt.Harness, fmt.Sprintf("shephrd:%s:%d", attempt.ID, attempt.RunGeneration), state)
		}
	}
	return outcome.WaitError
}

func writeLifecycleHandlerResults(out io.Writer, result delivery.Result) {
	for _, invocation := range result.LifecycleHandlers {
		line := fmt.Sprintf("report.accepted handler %s: %s", invocation.HandlerName, invocation.State)
		if invocation.Annotation != "" {
			line += " - " + invocation.Annotation
		}
		if invocation.ReceiptID != "" {
			line += fmt.Sprintf(" receipt %s:%s", invocation.ReceiptSystem, invocation.ReceiptID)
		}
		if invocation.FailureMessage != "" {
			line += " - " + invocation.FailureMessage
		}
		fmt.Fprintln(out, runner.Text(line))
	}
}

func (s Service) runInteractivePiAttempt(attempt model.Attempt, task model.Task, generation int, prompt string, resume bool, raw io.Writer) error {
	dir := filepath.Join(s.Config.DataDir, attempt.TaskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return s.endWithoutResult(attempt, generation, -1, err.Error())
	}
	extensionPath, err := pibridge.WriteExtension(dir, attempt.ID, generation)
	if err != nil {
		return s.endWithoutResult(attempt, generation, -1, "prepare Pi bridge extension: "+err.Error())
	}
	defer os.Remove(extensionPath)
	channel, err := pibridge.Open(attempt.ID, generation)
	if err != nil {
		return s.endWithoutResult(attempt, generation, -1, "open private Pi bridge: "+err.Error())
	}
	defer channel.Close()
	invocation, err := adapter.BuildNamedForRuntime("pi", attempt, prompt, resume, workerSessionNameFor(task), attempt.RuntimeBackend, extensionPath)
	if err != nil {
		return s.endWithoutResult(attempt, generation, -1, err.Error())
	}
	log, err := s.openRunnerLog(attempt, dir)
	if err != nil {
		return s.endWithoutResult(attempt, generation, -1, err.Error())
	}
	defer log.Close()
	lifecycle := s.runnerLifecycle(attempt, generation)
	_, err = runner.RunPi(runner.PiConfig{
		InteractiveConfig: runner.InteractiveConfig{
			Attempt: attempt, Invocation: invocation, Resume: resume, Output: raw, Log: log,
			Environment: s.workerEnvironment(channel.Environment()), State: s.runnerState(attempt, generation),
			Lifecycle: lifecycle, Name: "Pi", ShutdownGrace: interactivePiShutdownGrace,
			Finalize: func(result runner.Result) (bool, error) {
				return s.finalizeInteractiveAttempt(attempt, generation, result, log)
			},
		},
		Channel: channel, AcceptTimeout: pibridge.AcceptTimeout(), RepairTimeout: interactivePiRepairTimeout,
	})
	return err
}

func (s Service) openRunnerLog(attempt model.Attempt, dir string) (*os.File, error) {
	path := filepath.Join(dir, fmt.Sprintf("attempt-%d-runner.log", attempt.Number))
	log, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runner log: %w", err)
	}
	return log, nil
}

func (s Service) runnerState(attempt model.Attempt, generation int) runner.State {
	return runner.State{
		SetRunner: func(pid int) error {
			return s.Store.SetRunnerForRun(attempt.ID, generation, pid, true)
		},
		SetSession: func(sessionID string) error {
			return s.Store.SetSessionForRun(attempt.ID, generation, sessionID)
		},
		UpdateCursor: func(cursor int64) error {
			return s.Store.UpdateCursorForRun(attempt.ID, generation, cursor)
		},
		Ingest: func(event model.Event, cursor int64) error {
			_, err := s.ingestRunnerEvent(attempt, generation, event, cursor)
			return err
		},
		IngestCandidate: func(events []model.Event, cursor int64, sessionID string, request runner.RepairRequest, correction bool) (runner.CandidateIngestion, error) {
			facts := model.WorkspaceFacts{}
			for _, event := range events {
				if event.Checkpoint != nil {
					facts = workspaceFacts(attempt.WorktreePath)
					break
				}
			}
			messages, diagnostic, err := s.Store.AddEventCandidateForRun(attempt.ID, generation, sessionID, events, cursor, facts, store.EventRepairRequest{ID: request.ID, CandidateHash: request.CandidateHash}, correction)
			result := runner.CandidateIngestion{Diagnostic: diagnostic}
			for _, message := range messages {
				result.PersistedCursor = max(result.PersistedCursor, message.SourceCursor)
				if message.Wake {
					s.notifyMessage(message)
				}
			}
			return result, err
		},
		RecordAdapterRejection: func(cursor int64, request runner.RepairRequest, diagnostic adapter.Diagnostic) (*adapter.Diagnostic, error) {
			message, repair, err := s.Store.RecordAdapterRejectionForRun(attempt.ID, generation, cursor, store.EventRepairRequest{ID: request.ID, CandidateHash: request.CandidateHash}, diagnostic)
			if message.Wake {
				s.notifyMessage(message)
			}
			return repair, err
		},
		RecordControlFailure: func(reason string) error {
			return s.recordControlFailure(attempt.ID, generation, reason)
		},
		RecordRunnerDeath: func(reason string) error {
			return s.recordRunnerDeath(attempt.ID, generation, reason)
		},
		Finish: func(exitCode int, failure string) error {
			err := s.Store.FinishRunnerForRun(attempt.ID, generation, exitCode, failure)
			if errors.Is(err, store.ErrStaleRun) {
				return nil
			}
			return err
		},
	}
}

func (s Service) finalizeInteractiveAttempt(attempt model.Attempt, generation int, result runner.Result, log io.Writer) (bool, error) {
	latest, latestErr := s.Store.Attempt(attempt.ID)
	if latestErr != nil || latest.RunGeneration != generation {
		return false, result.WaitError
	}
	currentTask, taskErr := s.Store.Task(attempt.TaskID)
	if taskErr == nil && currentTask.Status == model.TaskStatusDone {
		verified, verifyErr := s.verifyDelivery(currentTask, attempt)
		if verifyErr != nil {
			fmt.Fprintln(log, verifyErr)
		} else {
			encoded, _ := json.Marshal(verified)
			fmt.Fprintln(log, string(encoded))
		}
	}
	return true, nil
}

type terminalLifecycle struct {
	service  Service
	attempt  model.Attempt
	source   string
	sequence uint64
	active   bool
}

func (s Service) runnerLifecycle(attempt model.Attempt, generation int) runner.Lifecycle {
	initialSequence, _ := strconv.ParseUint(s.getenv("SHEPHRD_HERDR_LIFECYCLE_SEQ"), 10, 64)
	lifecycle := &terminalLifecycle{service: s, attempt: attempt, source: terminalLifecycleSource(attempt.ID, generation), sequence: initialSequence}
	return runner.Lifecycle{Report: lifecycle.report, Release: lifecycle.release}
}

func (l *terminalLifecycle) report(state, sessionID string) error {
	l.sequence++
	client, err := l.service.terminalClientFor(l.attempt)
	if err != nil {
		return err
	}
	err = client.ReportAgent(l.service.endpoint(l.attempt), terminal.AgentReport{Source: l.source, Agent: l.attempt.Harness, State: state, Sequence: l.sequence, SessionID: sessionID})
	if err == nil {
		l.active = true
	}
	return err
}

func (l *terminalLifecycle) release() error {
	if !l.active {
		return nil
	}
	l.sequence++
	client, err := l.service.terminalClientFor(l.attempt)
	if err != nil {
		return err
	}
	return client.ReleaseAgent(l.service.endpoint(l.attempt), l.source, l.attempt.Harness, l.sequence)
}
