package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
	"shephrd/internal/process"
	"shephrd/internal/store"
)

type InteractiveConfig struct {
	ParseCandidate    func(string) (model.Event, bool, *adapter.Diagnostic)
	ParseCandidateSet func(string) ([]model.Event, bool, *adapter.Diagnostic)
	Timeout           time.Duration
	HarnessStarted    func(int) error
	Attempt           model.Attempt
	Invocation        adapter.Invocation
	Resume            bool
	Output            io.Writer
	Log               io.Writer
	Environment       []string
	State             State
	Lifecycle         Lifecycle
	Name              string
	ShutdownGrace     time.Duration
	Finalize          func(Result) (bool, error)
	process           func(InteractiveConfig) bridgeProcess
	after             func(time.Duration) <-chan time.Time
}

type bridgeProcess interface {
	PID() int
	Start() error
	Wait() error
	Kill() error
}

type execBridgeProcess struct {
	cmd   *exec.Cmd
	group bool
}

func (p execBridgeProcess) PID() int {
	return p.cmd.Process.Pid
}

func (p execBridgeProcess) Start() error {
	return p.cmd.Start()
}

func (p execBridgeProcess) Wait() error {
	return p.cmd.Wait()
}

func (p execBridgeProcess) Kill() error {
	if p.group {
		return process.Stop(p.PID())
	}
	return p.cmd.Process.Kill()
}

type bridgeExecution struct {
	config               InteractiveConfig
	process              bridgeProcess
	wait                 <-chan error
	waitErr              error
	processExited        bool
	cursor               int64
	terminal             bool
	failure              string
	failureRecorded      bool
	repairRequest        *RepairRequest
	claudeTerminal       string
	claudeBackgroundSeen bool
	cleanups             []func()
	deadline             <-chan time.Time
}

type frameRead[T any] struct {
	frame T
	err   error
}

func runInteractive(config InteractiveConfig, loop func(*bridgeExecution)) (Result, error) {
	if config.Output == nil {
		config.Output = io.Discard
	}
	if config.Log == nil {
		config.Log = io.Discard
	}
	if config.after == nil {
		config.after = time.After
	}
	if config.process == nil {
		config.process = newBridgeProcess
	}
	if err := config.State.SetRunner(os.Getpid()); err != nil {
		return Result{}, err
	}
	if err := config.Lifecycle.Report("working", ""); err != nil {
		return finishSetupFailure(config.State, "publish terminal working lifecycle: "+err.Error())
	}
	defer func() {
		if err := config.Lifecycle.Release(); err != nil {
			fmt.Fprintln(config.Log, "release terminal lifecycle: "+err.Error())
		}
	}()
	process := config.process(config)
	if err := process.Start(); err != nil {
		_ = config.Lifecycle.Report("blocked", "")
		return finishSetupFailure(config.State, err.Error())
	}
	fmt.Fprintf(config.Log, "interactive %s bridge started for run generation %d\n", config.Name, config.Attempt.RunGeneration)
	wait := make(chan error, 1)
	go func() { wait <- process.Wait() }()
	execution := &bridgeExecution{config: config, process: process, wait: wait, cursor: config.Attempt.Cursor}
	defer execution.cleanup()
	if config.Timeout > 0 {
		timer := time.NewTimer(config.Timeout)
		defer timer.Stop()
		execution.deadline = timer.C
	}
	if config.HarnessStarted != nil {
		if err := config.HarnessStarted(process.PID()); err != nil {
			execution.fail(err.Error(), false)
			execution.stop()
		}
	}
	if execution.failure == "" {
		loop(execution)
	}
	if execution.failure != "" && !execution.terminal {
		_ = config.Lifecycle.Report("blocked", "")
		if !execution.failureRecorded {
			_ = config.State.RecordRunnerDeath(execution.failure)
		}
	}
	outcome := result(execution.waitErr, execution.failure, execution.terminal)
	if err := config.State.Finish(outcome.ExitCode, execution.failure); err != nil {
		return outcome, err
	}
	if config.Finalize != nil {
		proceed, err := config.Finalize(outcome)
		if !proceed || err != nil {
			return outcome, err
		}
	}
	if execution.failure != "" {
		return outcome, errors.New(execution.failure)
	}
	return outcome, nil
}

func newBridgeProcess(config InteractiveConfig) bridgeProcess {
	cmd := exec.Command(config.Invocation.Command, config.Invocation.Args...)
	cmd.Dir = config.Invocation.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, config.Output, config.Output
	cmd.Env = config.Environment
	group := config.Timeout > 0
	if group {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: isatty.IsTerminal(os.Stdin.Fd()), Ctty: int(os.Stdin.Fd())}
	}
	return execBridgeProcess{cmd: cmd, group: group}
}

func startFrameSource[T any](ctx context.Context, next func() (T, error)) <-chan frameRead[T] {
	frames := make(chan frameRead[T], 1)
	go func() {
		for {
			frame, err := next()
			select {
			case frames <- frameRead[T]{frame: frame, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return frames
}

func readFrame[T any](ctx context.Context, next func() (T, error)) <-chan frameRead[T] {
	frame := make(chan frameRead[T], 1)
	go func() {
		value, err := next()
		select {
		case frame <- frameRead[T]{frame: value, err: err}:
		case <-ctx.Done():
		}
	}()
	return frame
}

func (b *bridgeExecution) observeExit(waitErr error) {
	b.waitErr = waitErr
	b.wait = nil
	b.processExited = true
}

func (b *bridgeExecution) stop() {
	if b.processExited {
		return
	}
	err := b.process.Kill()
	b.observeExit(<-b.wait)
	if err != nil && b.config.Timeout > 0 {
		if err = b.process.Kill(); err != nil {
			b.failure = "stop interactive harness: " + err.Error()
		}
	}
}

func (b *bridgeExecution) timeout() {
	b.fail(b.config.Name+" turn timed out after "+b.config.Timeout.String(), false)
	b.stop()
}

func (b *bridgeExecution) parseCandidate(envelope string) (model.Event, bool, *adapter.Diagnostic) {
	if b.config.ParseCandidate != nil {
		return b.config.ParseCandidate(envelope)
	}
	return adapter.ParseEventCandidate(envelope)
}

func (b *bridgeExecution) parseEvent(envelope string) (model.Event, bool, error) {
	event, found, diagnostic := b.parseCandidate(envelope)
	if diagnostic != nil {
		return event, found, diagnostic
	}
	return event, found, nil
}

func (b *bridgeExecution) fail(reason string, recorded bool) {
	if b.terminal || b.failure != "" {
		return
	}
	b.failure = reason
	b.failureRecorded = recorded
}

func (b *bridgeExecution) advance() bool {
	b.cursor++
	if err := b.config.State.UpdateCursor(b.cursor); err != nil {
		b.fail(err.Error(), false)
		return false
	}
	return true
}

func (b *bridgeExecution) acknowledge(sessionID, payload, failurePrefix string) bool {
	if err := b.config.Lifecycle.Report("working", sessionID); err != nil {
		b.fail(failurePrefix+err.Error(), false)
		return false
	}
	_ = b.config.State.Ingest(model.Event{Type: "progress", Payload: payload}, b.cursor)
	return true
}

func (b *bridgeExecution) ingestClaudeEnvelope(envelope, missing string) bool {
	if b.claudeTerminal != "" {
		b.fail("Claude bridge reported structured output after a terminal envelope", false)
		return false
	}
	event, found, parseErr := b.parseEvent(envelope)
	if parseErr != nil {
		b.fail(parseErr.Error(), false)
		return false
	}
	if !found {
		b.fail(missing, false)
		return false
	}
	if event.Type == "done" || event.Type == "question" || event.Type == "blocked" || event.Type == "failed" {
		b.claudeTerminal = envelope
		return true
	}
	if !b.advance() {
		return false
	}
	return b.ingestEnvelope(envelope, missing)
}

func (b *bridgeExecution) acceptClaudeTerminal() bool {
	if b.claudeTerminal == "" || !b.advance() {
		return false
	}
	envelope := b.claudeTerminal
	b.claudeTerminal = ""
	return b.ingestEnvelope(envelope, "Claude bridge event is missing a Shephrd envelope")
}

func (b *bridgeExecution) ingestEnvelope(envelope, missing string) bool {
	event, found, parseErr := b.parseEvent(envelope)
	if parseErr != nil || !found {
		if parseErr != nil {
			b.fail(parseErr.Error(), false)
		} else {
			b.fail(missing, false)
		}
		return false
	}
	if eventErr := b.config.State.Ingest(event, b.cursor); eventErr != nil {
		recorded := errors.Is(eventErr, store.ErrProtocolViolation) || errors.Is(eventErr, store.ErrStaleRun)
		b.fail(eventErr.Error(), recorded)
		return false
	}
	return b.acceptTerminal(event)
}

type piCandidateOutcome struct {
	result     string
	repairID   string
	diagnostic *adapter.Diagnostic
}

func (b *bridgeExecution) ingestPiCandidate(envelope, missing string) piCandidateOutcome {
	parse := b.config.ParseCandidateSet
	if parse == nil {
		parse = adapter.ParseEventCandidateSet
	}
	events, found, diagnostic := parse(envelope)
	if !found {
		b.fail(missing, false)
		return piCandidateOutcome{result: "fatal"}
	}
	request := newRepairRequest(envelope)
	correction := b.repairRequest != nil
	if correction {
		request = *b.repairRequest
		var err error
		events, err = parseCorrection(envelope, parse)
		if err != nil {
			b.fail(err.Error(), false)
			return piCandidateOutcome{result: "fatal"}
		}
	}
	if diagnostic != nil {
		bounded := diagnostic.Bounded()
		diagnostic = &bounded
		if !diagnostic.Repairable() || b.config.State.RecordAdapterRejection == nil {
			b.fail(diagnostic.Error(), false)
			return piCandidateOutcome{result: "fatal"}
		}
		repair, err := b.config.State.RecordAdapterRejection(b.cursor, request, *diagnostic)
		if err != nil {
			b.failIngestion(err)
			return piCandidateOutcome{result: "fatal"}
		}
		b.repairRequest = &request
		return piCandidateOutcome{result: "repair", repairID: request.ID, diagnostic: repair}
	}
	if b.config.State.IngestCandidate == nil {
		b.fail("interactive Pi candidate ingestion is unavailable", false)
		return piCandidateOutcome{result: "fatal"}
	}
	ingestion, err := b.config.State.IngestCandidate(events, b.cursor, b.config.Attempt.SessionID, request, correction)
	if err != nil {
		b.failIngestion(err)
		return piCandidateOutcome{result: "fatal"}
	}
	b.cursor = candidateEndCursor(b.cursor, len(events), ingestion.PersistedCursor)
	if ingestion.Diagnostic != nil {
		bounded := ingestion.Diagnostic.Bounded()
		b.repairRequest = &request
		return piCandidateOutcome{result: "repair", repairID: request.ID, diagnostic: &bounded}
	}
	if b.acceptTerminal(events[len(events)-1]) {
		return piCandidateOutcome{result: "accepted_terminal"}
	}
	return piCandidateOutcome{result: "accepted_nonterminal"}
}

func (b *bridgeExecution) failIngestion(err error) {
	recorded := errors.Is(err, store.ErrProtocolViolation) || errors.Is(err, store.ErrStaleRun)
	b.fail(err.Error(), recorded)
}

func (b *bridgeExecution) acceptTerminal(event model.Event) bool {
	terminalState := ""
	if event.Type == "done" {
		terminalState = "idle"
	} else if event.Type == "question" || event.Type == "blocked" || event.Type == "failed" {
		terminalState = "blocked"
	}
	if terminalState == "" {
		return false
	}
	b.terminal = true
	if err := b.config.Lifecycle.Report(terminalState, ""); err != nil {
		fmt.Fprintln(b.config.Log, "publish terminal lifecycle: "+err.Error())
	}
	return true
}

func (b *bridgeExecution) grace() <-chan time.Time {
	return b.config.after(b.config.ShutdownGrace)
}

func (b *bridgeExecution) addCleanup(cleanup func()) {
	b.cleanups = append(b.cleanups, cleanup)
}

func (b *bridgeExecution) cleanup() {
	for index := len(b.cleanups) - 1; index >= 0; index-- {
		b.cleanups[index]()
	}
}
