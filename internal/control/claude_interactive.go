package control

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/claudebridge"
	"shephrd/internal/model"
	"shephrd/internal/runner"
)

var (
	interactiveClaudeAcceptTimeout = 5 * time.Minute
	interactiveClaudeShutdownGrace = 10 * time.Second
)

func (s Service) runInteractiveClaudeAttempt(attempt model.Attempt, task model.Task, generation int, prompt string, resume bool, raw io.Writer) error {
	dir := filepath.Join(s.Config.DataDir, attempt.TaskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return s.endWithoutResult(attempt, generation, -1, err.Error())
	}
	executable, err := s.executable()
	if err != nil {
		return s.endWithoutResult(attempt, generation, -1, "locate Claude bridge executable: "+err.Error())
	}
	settingsPath, err := claudebridge.WriteSettings(dir, attempt.ID, generation, executable)
	if err != nil {
		return s.endWithoutResult(attempt, generation, -1, "prepare Claude bridge settings: "+err.Error())
	}
	defer os.Remove(settingsPath)
	channel, err := claudebridge.Open(attempt.ID, generation)
	if err != nil {
		return s.endWithoutResult(attempt, generation, -1, "open private Claude bridge: "+err.Error())
	}
	defer channel.Close()
	invocation, err := adapter.BuildNamedForRuntime("claude-code", attempt, prompt, resume, workerSessionNameFor(task), attempt.RuntimeBackend, settingsPath)
	if err != nil {
		return s.endWithoutResult(attempt, generation, -1, err.Error())
	}
	log, err := s.openRunnerLog(attempt, dir)
	if err != nil {
		return s.endWithoutResult(attempt, generation, -1, err.Error())
	}
	defer log.Close()
	_, err = runner.RunClaude(runner.ClaudeConfig{
		InteractiveConfig: runner.InteractiveConfig{
			Attempt: attempt, Invocation: invocation, Resume: resume, Output: raw, Log: log,
			Environment: s.mergedEnvironment(channel.Environment()), State: s.runnerState(attempt, generation),
			Lifecycle: s.runnerLifecycle(attempt, generation), Name: "Claude", ShutdownGrace: interactiveClaudeShutdownGrace,
			Finalize: func(result runner.Result) (bool, error) {
				return s.finalizeInteractiveAttempt(attempt, generation, result, log)
			},
		},
		Channel: channel, AcceptTimeout: interactiveClaudeAcceptTimeout,
	})
	return err
}
