package runner

import (
	"context"
	"errors"
	"time"

	"shephrd/internal/claudebridge"
	"shephrd/internal/model"
)

type ClaudeConfig struct {
	InteractiveConfig
	Channel       *claudebridge.Channel
	AcceptTimeout time.Duration
}

func RunClaude(config ClaudeConfig) (Result, error) {
	return runInteractive(config.InteractiveConfig, func(execution *bridgeExecution) {
		runClaudeBridge(execution, config)
	})
}

func runClaudeBridge(execution *bridgeExecution, config ClaudeConfig) {
	frameContext, cancelFrames := context.WithCancel(context.Background())
	execution.addCleanup(cancelFrames)
	frames := startFrameSource(frameContext, config.Channel.Accept)
	tracker := claudebridge.NewTracker(execution.config.Attempt.SessionID, execution.config.Resume)
	initialTimer := execution.config.after(config.AcceptTimeout)
	var terminalTimer <-chan time.Time
	for {
		select {
		case <-execution.deadline:
			execution.timeout()
			return
		case waitErr := <-execution.wait:
			execution.observeExit(waitErr)
			if execution.terminal || execution.failure != "" {
				return
			}
			execution.fail("Claude Code exited without a valid terminal <shephrd-event> envelope", false)
			return
		case <-initialTimer:
			initialTimer = nil
			execution.fail("Claude bridge did not acknowledge the native session", false)
			execution.stop()
			return
		case <-terminalTimer:
			execution.stop()
			return
		case accepted := <-frames:
			if accepted.err != nil {
				if execution.terminal {
					continue
				}
				execution.fail("Claude bridge failed: "+accepted.err.Error(), false)
				execution.stop()
				return
			}
			exchange := accepted.frame
			if execution.terminal {
				_ = exchange.Respond(true, "Shephrd accepted the worker result")
				continue
			}
			backgroundActive := false
			if exchange.Invalid != "" {
				execution.fail(exchange.Invalid, false)
			} else {
				input, parseErr := claudebridge.ParseHookInput(exchange.Input)
				if parseErr != nil {
					execution.fail(parseErr.Error(), false)
				} else {
					outcome := tracker.Handle(input)
					backgroundActive = outcome.BackgroundActive
					execution.fail(outcome.Failure, false)
					if outcome.Reconciled && execution.failure == "" {
						if err := validateClaudeReconciledEnvelopes(outcome.Envelopes, execution.parseEvent); err != nil {
							execution.fail(err.Error(), false)
						}
					}
					if outcome.Acknowledged && execution.failure == "" {
						initialTimer = nil
						if execution.advance() {
							payload := "Assignment acknowledged by claude-code"
							if execution.config.Resume {
								payload = "Follow-up acknowledged by claude-code"
							}
							execution.acknowledge(execution.config.Attempt.SessionID, payload, "publish terminal Claude session lifecycle: ")
						}
					}
					if outcome.BackgroundActive {
						execution.claudeBackgroundSeen = true
						execution.claudeTerminal = ""
					} else if execution.claudeBackgroundSeen && len(outcome.Envelopes) > 0 {
						execution.claudeTerminal = ""
					}
					for _, envelope := range outcome.Envelopes {
						if execution.failure != "" || execution.terminal {
							break
						}
						if execution.ingestClaudeEnvelope(envelope, "Claude bridge event is missing a Shephrd envelope") && execution.terminal {
							terminalTimer = execution.grace()
						}
					}
					if outcome.Settled && !execution.terminal && execution.failure == "" {
						if !execution.acceptClaudeTerminal() {
							if execution.failure == "" {
								execution.fail("Claude Code settled without a valid terminal <shephrd-event> envelope", false)
							}
						} else {
							terminalTimer = execution.grace()
						}
					}
				}
			}
			terminate := execution.terminal || execution.failure != ""
			stopReason := ""
			if execution.terminal {
				stopReason = "Shephrd accepted the worker result"
			} else if execution.failure != "" {
				stopReason = "Shephrd stopped the invalid worker invocation"
			}
			var respondErr error
			if backgroundActive && !terminate {
				respondErr = exchange.RespondBlock("Claude Code background work is still active")
			} else {
				respondErr = exchange.Respond(terminate, stopReason)
			}
			if respondErr != nil && execution.failure == "" && !execution.terminal {
				execution.fail("respond to Claude bridge hook: "+respondErr.Error(), false)
			}
			if execution.failure != "" {
				execution.stop()
				return
			}
		}
	}
}

func validateClaudeReconciledEnvelopes(envelopes []string, parse func(string) (model.Event, bool, error)) error {
	terminal := false
	for _, envelope := range envelopes {
		event, found, err := parse(envelope)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("Claude bridge Stop reconciliation is missing a Shephrd envelope")
		}
		if terminal {
			return errors.New("Claude bridge Stop reconciliation contains structured output after a terminal envelope")
		}
		terminal = event.Type == "done" || event.Type == "question" || event.Type == "blocked" || event.Type == "failed"
	}
	return nil
}
