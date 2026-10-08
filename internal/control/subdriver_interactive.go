package control

import (
	"os"

	"shephrd/internal/adapter"
	"shephrd/internal/claudebridge"
	"shephrd/internal/pibridge"
	"shephrd/internal/runner"
)

func (s Service) runInteractiveSubdriver(config runner.InteractiveConfig, prompt, label, executable, dir string) error {
	attempt := config.Attempt
	var path string
	var environment []string
	var run func(runner.InteractiveConfig) (runner.Result, error)
	var err error
	if attempt.Harness == "pi" {
		path, err = pibridge.WriteExtension(dir, attempt.ID, attempt.RunGeneration)
		if err != nil {
			return err
		}
		defer os.Remove(path)
		channel, err := pibridge.Open(attempt.ID, attempt.RunGeneration)
		if err != nil {
			return err
		}
		defer channel.Close()
		environment = channel.Environment()
		config.Name = "Pi"
		config.ShutdownGrace = interactivePiShutdownGrace
		run = func(config runner.InteractiveConfig) (runner.Result, error) {
			return runner.RunPi(runner.PiConfig{InteractiveConfig: config, Channel: channel})
		}
	} else {
		path, err = claudebridge.WriteSettings(dir, attempt.ID, attempt.RunGeneration, executable)
		if err != nil {
			return err
		}
		defer os.Remove(path)
		channel, err := claudebridge.Open(attempt.ID, attempt.RunGeneration)
		if err != nil {
			return err
		}
		defer channel.Close()
		for key, value := range channel.Environment() {
			environment = append(environment, key+"="+value)
		}
		config.Name = "Claude"
		config.ShutdownGrace = interactiveClaudeShutdownGrace
		run = func(config runner.InteractiveConfig) (runner.Result, error) {
			return runner.RunClaude(runner.ClaudeConfig{InteractiveConfig: config, Channel: channel, AcceptTimeout: interactiveClaudeAcceptTimeout})
		}
	}
	config.Invocation, err = adapter.BuildNamedForRuntime(attempt.Harness, attempt, prompt, false, label, attempt.RuntimeBackend, path)
	if err != nil {
		return err
	}
	config.Environment = append(config.Environment, environment...)
	_, err = run(config)
	return err
}
