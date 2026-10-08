package runner

import (
	"context"
	"time"

	"shephrd/internal/pibridge"
)

type PiConfig struct {
	InteractiveConfig
	Channel         *pibridge.Channel
	AcceptTimeout   time.Duration
	ExitAcceptGrace time.Duration
	RepairTimeout   time.Duration
}

func RunPi(config PiConfig) (Result, error) {
	if config.AcceptTimeout == 0 {
		config.AcceptTimeout = pibridge.AcceptTimeout()
	}
	if config.ExitAcceptGrace == 0 {
		config.ExitAcceptGrace = 100 * time.Millisecond
	}
	if config.RepairTimeout == 0 {
		config.RepairTimeout = 120 * time.Second
	}
	return runInteractive(config.InteractiveConfig, func(execution *bridgeExecution) {
		runPiBridge(execution, config)
	})
}

func runPiBridge(execution *bridgeExecution, config PiConfig) {
	acceptContext, cancelAccept := context.WithTimeout(context.Background(), config.AcceptTimeout)
	execution.addCleanup(cancelAccept)
	connections := readFrame(acceptContext, func() (*pibridge.Connection, error) {
		return config.Channel.Accept(acceptContext)
	})
	var connection *pibridge.Connection
	exitedBeforeConnection := false
	select {
	case <-execution.deadline:
		execution.timeout()
		return
	case accepted := <-connections:
		if accepted.err != nil {
			execution.fail("Pi bridge did not connect: "+accepted.err.Error(), false)
		} else {
			connection = accepted.frame
		}
	case waitErr := <-execution.wait:
		execution.observeExit(waitErr)
		exitedBeforeConnection = true
		select {
		case accepted := <-connections:
			if accepted.err == nil {
				connection = accepted.frame
			} else {
				execution.fail("Pi bridge did not connect: "+accepted.err.Error(), false)
			}
		case <-execution.config.after(config.ExitAcceptGrace):
			execution.fail("Pi exited before the private bridge connected", false)
		}
	}
	if connection == nil {
		execution.stop()
		return
	}
	execution.addCleanup(func() { _ = connection.Close() })
	frameContext, cancelFrames := context.WithCancel(context.Background())
	execution.addCleanup(cancelFrames)
	frames := startFrameSource(frameContext, connection.Next)
	if exitedBeforeConnection {
		execution.wait = nil
	}
	sessionSeen := false
	repairPending := false
	var repairTimer <-chan time.Time
	var terminalTimer <-chan time.Time
	respond := func(sequence uint64, result, repairID string, outcome piCandidateOutcome) bool {
		if err := connection.Respond(sequence, result, repairID, outcome.diagnostic); err != nil {
			execution.fail(err.Error(), false)
			return false
		}
		return true
	}
	for {
		select {
		case <-execution.deadline:
			execution.timeout()
			return
		case waitErr := <-execution.wait:
			execution.observeExit(waitErr)
			if execution.terminal {
				return
			}
			execution.fail("Pi exited without a valid terminal <shephrd-event> envelope", false)
			return
		case <-repairTimer:
			execution.fail("Pi protocol correction timed out after "+config.RepairTimeout.String(), false)
			execution.stop()
			return
		case <-terminalTimer:
			execution.stop()
			return
		case received := <-frames:
			if received.err != nil {
				if execution.terminal {
					if execution.processExited {
						return
					}
					frames = nil
					continue
				}
				execution.fail(received.err.Error(), false)
				execution.stop()
				return
			}
			execution.cursor++
			frame := received.frame
			switch frame.Kind {
			case "session":
				if sessionSeen || frame.SessionID != execution.config.Attempt.SessionID {
					execution.fail("Pi bridge reported an unexpected native session identity", false)
					_ = connection.Respond(frame.Sequence, pibridge.ResultFatal, "", nil)
					execution.stop()
					return
				}
				sessionSeen = true
				payload := "Assignment acknowledged by pi"
				if execution.config.Resume {
					payload = "Follow-up acknowledged by pi"
				}
				if !execution.acknowledge(frame.SessionID, payload, "publish terminal Pi session lifecycle: ") || !respond(frame.Sequence, pibridge.ResultAcceptedNonterminal, "", piCandidateOutcome{}) {
					execution.stop()
					return
				}
			case "event_candidate":
				if !sessionSeen || execution.terminal {
					execution.fail("Pi bridge event ordering is invalid", false)
					_ = connection.Respond(frame.Sequence, pibridge.ResultFatal, "", nil)
					execution.stop()
					return
				}
				outcome := execution.ingestPiCandidate(frame.Envelope, "Pi bridge event candidate is missing a Shephrd envelope")
				switch outcome.result {
				case "accepted_nonterminal":
					if !respond(frame.Sequence, pibridge.ResultAcceptedNonterminal, "", outcome) {
						execution.stop()
						return
					}
				case "accepted_terminal":
					_ = connection.Respond(frame.Sequence, pibridge.ResultAcceptedTerminal, "", nil)
					frames = nil
					repairTimer = nil
					if execution.processExited {
						return
					}
					terminalTimer = execution.grace()
				case "repair":
					if repairPending || !respond(frame.Sequence, pibridge.ResultRepair, outcome.repairID, outcome) {
						if execution.failure == "" {
							execution.fail("Pi bridge requested more than one protocol correction", false)
						}
						execution.stop()
						return
					}
					repairPending = true
					repairTimer = execution.config.after(config.RepairTimeout)
				default:
					_ = connection.Respond(frame.Sequence, pibridge.ResultFatal, "", nil)
					execution.stop()
					return
				}
			case "settled":
				execution.fail("Pi settled without a valid terminal <shephrd-event> envelope", false)
				_ = connection.Respond(frame.Sequence, pibridge.ResultFatal, "", nil)
				execution.stop()
				return
			case "repair_exhausted":
				execution.fail("Pi protocol correction turn ended without an accepted terminal envelope", false)
				_ = connection.Respond(frame.Sequence, pibridge.ResultFatal, "", nil)
				execution.stop()
				return
			case "invalid":
				execution.fail("Pi bridge rejected malformed or oversized structured output", false)
				_ = connection.Respond(frame.Sequence, pibridge.ResultFatal, "", nil)
				execution.stop()
				return
			}
		}
	}
}
