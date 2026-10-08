package runner

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
	"shephrd/internal/process"
	"shephrd/internal/store"
)

type HeadlessConfig struct {
	ParseCandidate   func(string) ([]model.Event, bool, *adapter.Diagnostic)
	Timeout          time.Duration
	HarnessStarted   func(int) error
	Attempt          model.Attempt
	Invocation       adapter.Invocation
	RepairInvocation func(string, string) (adapter.Invocation, error)
	RepairTimeout    time.Duration
	Resume           bool
	Log              *os.File
	Raw              io.Writer
	Terminal         bool
	Banner           string
	Environment      []string
	State            State
}

type headlessTurn struct {
	attempt          *model.Attempt
	config           HeadlessConfig
	output           io.Writer
	cursor           *int64
	acknowledged     *bool
	correction       bool
	request          RepairRequest
	diagnostic       *adapter.Diagnostic
	candidateEvents  []model.Event
	candidateCursor  int64
	terminal         bool
	terminalAccepted bool
	finalAccepted    bool
	failure          string
	adapterFailure   string
	waitErr          error
}

func RunHeadless(config HeadlessConfig) (Result, error) {
	if config.Raw == nil {
		config.Raw = io.Discard
	}
	harnessOutput := output(config.Log, config.Raw, !config.Terminal)
	if config.Terminal && config.Banner != "" {
		fmt.Fprint(config.Raw, config.Banner)
	}
	if err := config.State.SetRunner(os.Getpid()); err != nil {
		return Result{}, err
	}
	attempt := config.Attempt
	cursor := attempt.Cursor
	acknowledged := false
	turn := headlessTurn{attempt: &attempt, config: config, output: harnessOutput, cursor: &cursor, acknowledged: &acknowledged}
	started := time.Now()
	turn.run(config.Invocation)
	if config.Timeout > 0 {
		remaining := config.Timeout - time.Since(started)
		if remaining <= 0 && turn.diagnostic != nil {
			turn.failure = "headless turn timed out before protocol correction"
		}
		if config.RepairTimeout <= 0 {
			config.RepairTimeout = 120 * time.Second
		}
		config.RepairTimeout = min(config.RepairTimeout, remaining)
	}
	if turn.diagnostic != nil && turn.failure == "" {
		if config.RepairInvocation == nil || attempt.SessionID == "" || strings.HasPrefix(attempt.SessionID, "pending:") {
			turn.failure = "headless protocol repair cannot resume the assigned worker session"
		} else {
			invocation, err := config.RepairInvocation(attempt.SessionID, headlessRepairPrompt(turn.request, *turn.diagnostic))
			if err != nil {
				turn.failure = "build headless protocol repair turn: " + err.Error()
			} else {
				correction := headlessTurn{attempt: &attempt, config: config, output: harnessOutput, cursor: &cursor, acknowledged: &acknowledged, correction: true, request: turn.request}
				correction.run(invocation)
				turn.waitErr = correction.waitErr
				turn.terminal = correction.terminal
				turn.terminalAccepted = correction.terminalAccepted
				turn.finalAccepted = correction.finalAccepted
				turn.adapterFailure = correction.adapterFailure
				turn.failure = correction.failure
			}
		}
	}
	adapterFailure := turn.adapterFailure
	if turn.failure != "" {
		adapterFailure = turn.failure
		if err := config.State.RecordControlFailure(turn.failure); err != nil && !errors.Is(err, store.ErrStaleRun) {
			fmt.Fprintln(config.Raw, err)
		}
		if config.Terminal {
			fmt.Fprintf(config.Raw, "Blocked: %s\n", Text(turn.failure))
		}
	}
	if !turn.terminal && turn.failure == "" {
		reason := adapterFailure
		outcome := result(turn.waitErr, adapterFailure, turn.terminalAccepted)
		if reason == "" && turn.waitErr != nil {
			reason = fmt.Sprintf("%s exited with code %d before a terminal result", attempt.Harness, outcome.ExitCode)
		}
		if reason == "" {
			reason = fmt.Sprintf("%s exited without a valid terminal <shephrd-event> envelope", attempt.Harness)
		}
		if err := config.State.RecordRunnerDeath(reason); err != nil && !errors.Is(err, store.ErrStaleRun) {
			fmt.Fprintln(config.Raw, err)
		}
		if config.Terminal {
			fmt.Fprintf(config.Raw, "Blocked: %s\n", Text(reason))
		}
		adapterFailure = reason
	}
	outcome := result(turn.waitErr, adapterFailure, turn.terminalAccepted)
	if err := config.State.Finish(outcome.ExitCode, adapterFailure); err != nil {
		return outcome, err
	}
	return outcome, nil
}

func (turn *headlessTurn) run(invocation adapter.Invocation) {
	cmd := exec.Command(invocation.Command, invocation.Args...)
	cmd.Dir = invocation.Dir
	cmd.Env = turn.config.Environment
	process.Detach(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		turn.failure = err.Error()
		return
	}
	cmd.Stderr = turn.output
	if err := cmd.Start(); err != nil {
		turn.failure = err.Error()
		return
	}
	if turn.config.HarnessStarted != nil {
		if err := turn.config.HarnessStarted(cmd.Process.Pid); err != nil {
			turn.failure = err.Error()
			_ = stopHeadlessProcess(cmd)
			return
		}
	}
	var repairTimer *time.Timer
	var repairTimedOut chan struct{}
	var repairStopped chan error
	if turn.correction || turn.config.Timeout > 0 {
		timeout := turn.config.Timeout
		if turn.correction {
			timeout = turn.config.RepairTimeout
		}
		if timeout <= 0 {
			timeout = 120 * time.Second
		}
		repairTimedOut = make(chan struct{})
		repairStopped = make(chan error, 1)
		repairTimer = time.AfterFunc(timeout, func() {
			close(repairTimedOut)
			repairStopped <- process.Stop(cmd.Process.Pid)
		})
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	stop := false
	piCompleted := false
	for scanner.Scan() {
		*turn.cursor++
		line := append([]byte(nil), scanner.Bytes()...)
		fmt.Fprintln(turn.output, string(line))
		parsed := adapter.ParseLine(turn.attempt.Harness, line)
		if !turn.acceptSession(parsed) {
			stop = true
			break
		}
		if parsed.Acknowledged && !*turn.acknowledged {
			*turn.acknowledged = true
			if !turn.correction {
				payload := "Assignment acknowledged by " + turn.attempt.Harness
				if turn.config.Resume {
					payload = "Follow-up acknowledged by " + turn.attempt.Harness
				}
				_ = turn.config.State.Ingest(model.Event{Type: "progress", Payload: payload}, *turn.cursor)
				if turn.config.Terminal {
					fmt.Fprintln(turn.config.Raw, payload)
				}
			}
		}
		if parsed.Text != "" && parsed.EventAuthority == adapter.EventAuthorityAssignedWorker {
			if turn.correction {
				if !turn.captureCorrection(parsed.Text) {
					stop = true
					break
				}
			} else if !turn.handleCandidate(parsed.Text) {
				stop = true
				break
			}
		}
		if turn.attempt.Harness == "pi" {
			if parsed.Failure != "" {
				piCompleted = false
			} else if parsed.EventAuthority == adapter.EventAuthorityAssignedWorker {
				piCompleted = true
			}
			if parsed.Terminal && piCompleted {
				turn.adapterFailure = ""
			}
		}
		if parsed.Failure != "" && !turn.finalAccepted {
			turn.adapterFailure = parsed.Failure
		}
		if !turn.correction {
			if err := turn.config.State.UpdateCursor(*turn.cursor); err != nil && !errors.Is(err, store.ErrStaleRun) {
				fmt.Fprintln(turn.config.Raw, err)
			}
			turn.attempt.Cursor = *turn.cursor
		}
	}
	if scanErr := scanner.Err(); scanErr != nil && turn.failure == "" && turn.adapterFailure == "" {
		turn.failure = "read harness event stream: " + scanErr.Error()
		stop = true
	}
	timedOut := false
	if repairTimer != nil {
		if !repairTimer.Stop() {
			<-repairTimedOut
		}
		select {
		case <-repairTimedOut:
			timedOut = true
		default:
		}
	}
	if timedOut {
		if err := waitHeadlessProcessStop(cmd, repairStopped, process.Stop); err != nil {
			turn.failure = "terminate timed-out headless repair process tree: " + err.Error()
		} else if turn.failure == "" {
			turn.failure = "headless turn timed out"
			if turn.correction {
				turn.failure = "headless protocol correction timed out"
			}
		}
		turn.waitErr = nil
		return
	}
	if stop {
		if err := stopHeadlessProcess(cmd); err != nil && turn.failure == "" {
			turn.failure = "terminate headless worker process tree: " + err.Error()
		}
	} else {
		turn.waitErr = cmd.Wait()
	}
	if turn.correction && turn.failure == "" {
		turn.acceptCorrection()
	}
}

func (turn *headlessTurn) acceptSession(parsed adapter.Parsed) bool {
	if parsed.SessionID == "" || !parsed.Acknowledged && parsed.EventAuthority != adapter.EventAuthorityAssignedWorker {
		return true
	}
	if turn.correction {
		if parsed.SessionID != turn.attempt.SessionID {
			turn.failure = "protocol correction came from a different worker session"
			return false
		}
		return true
	}
	if parsed.SessionID != turn.attempt.SessionID {
		if err := turn.config.State.SetSession(parsed.SessionID); err != nil {
			turn.failure = err.Error()
			return false
		}
		turn.attempt.SessionID = parsed.SessionID
	}
	return true
}

func (turn *headlessTurn) handleCandidate(text string) bool {
	if turn.finalAccepted {
		return true
	}
	parse := turn.config.ParseCandidate
	if parse == nil {
		parse = adapter.ParseEventCandidateSet
	}
	events, found, diagnostic := parse(text)
	request := newRepairRequest(text)
	if diagnostic != nil {
		return turn.requestRepair(request, *diagnostic)
	}
	if !found {
		_ = turn.config.State.Ingest(model.Event{Type: "progress", Payload: text}, *turn.cursor)
		return true
	}
	if turn.config.Terminal {
		if readable := Text(readableAssistantText(text)); readable != "" {
			fmt.Fprintf(turn.config.Raw, "Assistant: %s\n", readable)
		}
	}
	if turn.config.State.IngestCandidate == nil {
		turn.failure = "headless event candidate ingestion is unavailable"
		return false
	}
	ingestion, err := turn.config.State.IngestCandidate(events, *turn.cursor, turn.attempt.SessionID, request, false)
	if err != nil {
		turn.failure = err.Error()
		return false
	}
	alignedCursor := candidateEndCursor(*turn.cursor, len(events), ingestion.PersistedCursor)
	if alignedCursor > *turn.cursor {
		*turn.cursor = alignedCursor
		turn.attempt.Cursor = alignedCursor
	}
	if ingestion.Diagnostic != nil {
		turn.request = request
		bounded := ingestion.Diagnostic.Bounded()
		turn.diagnostic = &bounded
		return false
	}
	last := events[len(events)-1]
	if last.Type == "question" || last.Type == "done" || last.Type == "blocked" || last.Type == "failed" {
		turn.terminal = true
		turn.terminalAccepted = true
		turn.finalAccepted = last.Type != "question"
		if turn.config.Terminal {
			fmt.Fprintf(turn.config.Raw, "Terminal %s: %s\n", last.Type, Text(last.Payload))
			if last.Artifact != "" {
				fmt.Fprintf(turn.config.Raw, "Artifact: %s\n", Text(last.Artifact))
			}
		}
	}
	return true
}

func (turn *headlessTurn) requestRepair(request RepairRequest, diagnostic adapter.Diagnostic) bool {
	bounded := diagnostic.Bounded()
	if !bounded.Repairable() || turn.config.State.RecordAdapterRejection == nil {
		turn.failure = diagnostic.Error()
		return false
	}
	repair, err := turn.config.State.RecordAdapterRejection(*turn.cursor, request, bounded)
	if err != nil {
		turn.failure = err.Error()
		return false
	}
	if repair == nil {
		turn.failure = "headless event repair request was not recorded"
		return false
	}
	turn.request = request
	bounded = repair.Bounded()
	turn.diagnostic = &bounded
	return false
}

func (turn *headlessTurn) captureCorrection(text string) bool {
	if turn.candidateEvents != nil {
		turn.failure = "protocol correction contains multiple authoritative assistant records"
		return false
	}
	parse := turn.config.ParseCandidate
	if parse == nil {
		parse = adapter.ParseEventCandidateSet
	}
	events, err := parseCorrection(text, parse)
	if err != nil {
		turn.failure = err.Error()
		return false
	}
	turn.candidateEvents = events
	turn.candidateCursor = *turn.cursor
	*turn.cursor++
	return true
}

func (turn *headlessTurn) acceptCorrection() {
	if turn.adapterFailure != "" {
		turn.failure = turn.adapterFailure
		return
	}
	if turn.candidateEvents == nil {
		turn.failure = "worker session ended without the requested protocol correction"
		return
	}
	if turn.config.State.IngestCandidate == nil {
		turn.failure = "headless event candidate ingestion is unavailable"
		return
	}
	ingestion, err := turn.config.State.IngestCandidate(turn.candidateEvents, turn.candidateCursor, turn.attempt.SessionID, turn.request, true)
	if err != nil {
		turn.failure = err.Error()
		return
	}
	if ingestion.Diagnostic != nil {
		turn.failure = "protocol correction failed strict revalidation: " + ingestion.Diagnostic.Code
		return
	}
	alignedCursor := candidateEndCursor(turn.candidateCursor, len(turn.candidateEvents), ingestion.PersistedCursor)
	if alignedCursor > *turn.cursor {
		*turn.cursor = alignedCursor
	}
	last := turn.candidateEvents[1]
	turn.terminal = true
	turn.terminalAccepted = true
	turn.finalAccepted = last.Type != "question"
	if err := turn.config.State.UpdateCursor(*turn.cursor); err != nil && !errors.Is(err, store.ErrStaleRun) {
		turn.failure = err.Error()
		return
	}
	turn.attempt.Cursor = *turn.cursor
	if turn.config.Terminal {
		fmt.Fprintf(turn.config.Raw, "Terminal %s: %s\n", last.Type, Text(last.Payload))
		if last.Artifact != "" {
			fmt.Fprintf(turn.config.Raw, "Artifact: %s\n", Text(last.Artifact))
		}
	}
}

func parseCorrection(text string, parse func(string) ([]model.Event, bool, *adapter.Diagnostic)) ([]model.Event, error) {
	events, found, diagnostic := parse(text)
	if diagnostic != nil {
		return nil, fmt.Errorf("protocol correction rejected: %s", diagnostic.Error())
	}
	if !found || len(events) != 2 || events[0].Type != "checkpoint" || !onlyEventEnvelopes(text) {
		return nil, fmt.Errorf("protocol correction must contain exactly one checkpoint followed by one terminal envelope and no prose")
	}
	last := events[1]
	if last.Type != "question" && last.Type != "done" && last.Type != "blocked" && last.Type != "failed" {
		return nil, fmt.Errorf("protocol correction must end with one terminal envelope")
	}
	return events, nil
}

func candidateEndCursor(start int64, eventCount int, persisted int64) int64 {
	if persisted > 0 {
		return max(start, persisted)
	}
	return start + int64(eventCount) - 1
}

func onlyEventEnvelopes(text string) bool {
	const open = "<shephrd-event>"
	const close = "</shephrd-event>"
	for {
		start := strings.Index(text, open)
		if start < 0 {
			return strings.TrimSpace(text) == ""
		}
		if strings.TrimSpace(text[:start]) != "" {
			return false
		}
		end := strings.Index(text[start+len(open):], close)
		if end < 0 {
			return false
		}
		text = text[start+len(open)+end+len(close):]
	}
}

func stopHeadlessProcess(cmd *exec.Cmd) error {
	return stopHeadlessProcessWith(cmd, process.Stop)
}

func stopHeadlessProcessWith(cmd *exec.Cmd, stop func(int) error) error {
	stopped := make(chan error, 1)
	go func() { stopped <- stop(cmd.Process.Pid) }()
	return waitHeadlessProcessStop(cmd, stopped, stop)
}

func waitHeadlessProcessStop(cmd *exec.Cmd, stopped <-chan error, stop func(int) error) error {
	_ = cmd.Wait()
	if err := <-stopped; err != nil {
		return stop(cmd.Process.Pid)
	}
	return nil
}

func newRepairRequest(candidate string) RepairRequest {
	digest := sha256.Sum256([]byte(candidate))
	return RepairRequest{ID: store.NewID("repair"), CandidateHash: hex.EncodeToString(digest[:])}
}

func headlessRepairPrompt(request RepairRequest, diagnostic adapter.Diagnostic) string {
	diagnostic = diagnostic.Bounded()
	lines := []string{
		"SHEPHRD_PROTOCOL_REPAIR",
		"The prior Shephrd event candidate was rejected and no part of it was accepted.",
		"Repair ID: " + request.ID,
		"Candidate SHA-256: " + request.CandidateHash,
		"Code: " + diagnostic.Code,
		"Phase: " + diagnostic.Phase,
		"Diagnostic: " + diagnostic.Error(),
	}
	if diagnostic.Field != "" {
		lines = append(lines, "Rejected field: "+diagnostic.Field)
	}
	lines = append(lines,
		"Do not use tools, modify files, rerun checks, repeat dispatches or other side effects, repeat project work, or include prose. Preserve the report's facts; do not drop rejected fields.",
		`decisions entries must be objects {"decision":"choice made","reason":"why"}; checks entries must be objects {"command":"test command","result":"observed result"}, never strings.`,
		"Respond with exactly two envelopes in this order: one valid current checkpoint, then one corrected question, done, blocked, or failed terminal.",
		"Checkpoint fields are exactly type, payload, and checkpoint. Terminal fields are exactly type, payload, and optional artifact.",
		"This is correction attempt 1 of 1.",
	)
	return strings.Join(lines, "\n")
}
