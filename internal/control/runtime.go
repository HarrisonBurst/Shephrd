package control

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/terminal"
)

func (s Service) requestedRuntime(requested ...string) (string, error) {
	if len(requested) > 1 {
		return "", fmt.Errorf("only one worker runtime may be selected")
	}
	workerRuntime := ""
	if len(requested) == 1 {
		workerRuntime = requested[0]
	}
	if workerRuntime == "" {
		workerRuntime = s.getenv("SHEPHRD_WORKER_RUNTIME")
	}
	if workerRuntime == "" {
		workerRuntime = s.Config.WorkerRuntime
	}
	if workerRuntime == "" {
		workerRuntime = "headless"
	}
	return workerRuntime, nil
}

func isTerminalRuntime(backend string) bool {
	return backend != "" && backend != "headless"
}

func (s Service) terminalProviders() (map[string]terminal.Provider, error) {
	if s.TerminalProviders != nil {
		providers := make(map[string]terminal.Provider, len(s.TerminalProviders))
		for _, provider := range s.TerminalProviders {
			providers[provider.Backend()] = provider
		}
		return providers, nil
	}
	providers := make(map[string]terminal.Provider, 2)
	if configured := s.Config.TerminalExtensions.Herdr; configured != nil {
		providers["herdr"] = terminal.NewHerdrExtensionProvider(configured.Command, configured.SHA256, s.environ())
	}
	if configured := s.Config.TerminalExtensions.Cmux; configured != nil {
		providers["cmux"] = terminal.NewCmuxExtensionProvider(configured.Command, configured.SHA256, s.environ())
	}
	return providers, nil
}

func (s Service) selectRuntime(requested ...string) (terminal.Selection, error) {
	workerRuntime, err := s.requestedRuntime(requested...)
	if err != nil {
		return terminal.Selection{}, err
	}
	if workerRuntime == "headless" {
		return terminal.Selection{Backend: "headless"}, nil
	}
	providers, err := s.terminalProviders()
	if err != nil {
		return terminal.Selection{}, err
	}
	return terminal.SelectProviders(workerRuntime, providers, func(provider terminal.Provider) terminal.ParentContext {
		return terminal.SanitizeParentContext(runtime.GOOS, s.getenv("PATH"), s.environ(), provider.ContextKeys())
	})
}

func (s Service) terminalClient(backend, socketPath string) (terminal.RuntimeClient, error) {
	providers, err := s.terminalProviders()
	if err != nil {
		return nil, err
	}
	provider, exists := providers[backend]
	if !exists {
		if terminal.KnownTerminalBackend(backend) {
			return nil, &terminal.SelectionError{Kind: "terminal_extension_not_configured", Requested: backend}
		}
		return nil, &terminal.SelectionError{Kind: "terminal_provider_unsupported", Requested: backend}
	}
	return provider.WithSocket(socketPath), nil
}

func (s Service) preflightRuntime(backend string) error {
	if !isTerminalRuntime(backend) {
		return nil
	}
	if _, err := s.selectRuntime(backend); err != nil {
		return err
	}
	_, err := s.workerExecutable()
	return err
}

func (s Service) launch(attempt model.Attempt, inputPath string, resume bool) error {
	executable, err := s.workerExecutable()
	if err != nil {
		return err
	}
	if err := s.Store.SetRuntimeExecutableForRun(attempt.ID, attempt.RunGeneration, executable); err != nil {
		return err
	}
	attempt.RuntimeExecutable = executable
	if isTerminalRuntime(attempt.RuntimeBackend) {
		return s.launchTerminal(attempt, inputPath, executable, resume)
	}
	return s.launchHeadless(attempt, inputPath, executable, resume)
}

func (s Service) workerExecutable() (string, error) {
	configured := s.getenv("SHEPHRD_EXECUTABLE")
	if executablePath(configured) {
		return filepath.Clean(configured), nil
	}
	fallback, err := s.executable()
	if err != nil {
		return "", fmt.Errorf("locate shephrd executable: %w", err)
	}
	return selectWorkerExecutable("", fallback)
}

func selectWorkerExecutable(configured, fallback string) (string, error) {
	if executablePath(configured) {
		return filepath.Clean(configured), nil
	}
	fallback, err := filepath.Abs(fallback)
	if err != nil {
		return "", fmt.Errorf("locate shephrd executable: %w", err)
	}
	if !executablePath(fallback) {
		return "", fmt.Errorf("current shephrd executable %q is not an executable file", fallback)
	}
	return filepath.Clean(fallback), nil
}

func executablePath(path string) bool {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

func (s Service) launchTerminal(attempt model.Attempt, inputPath, executable string, resume bool) (err error) {
	selection, err := s.selectRuntime(attempt.RuntimeBackend)
	if err != nil {
		return err
	}
	parent := selection.Parent
	client, err := s.terminalClient(attempt.RuntimeBackend, parent.SocketPath)
	if err != nil {
		return err
	}
	task, err := s.Store.Task(attempt.TaskID)
	if err != nil {
		return err
	}
	configFile, err := config.Path()
	if err != nil {
		return fmt.Errorf("locate Shephrd config: %w", err)
	}
	launcher, err := s.writeTerminalLauncher(task.ID, attempt, inputPath, executable, configFile, resume, selection.Provider.OwnedCleanup())
	if err != nil {
		return err
	}
	generation := attempt.RuntimeGeneration + 1
	environment := []string{
		"SHEPHRD_EXECUTABLE=" + executable,
		"SHEPHRD_ATTEMPT_ID=" + attempt.ID,
		"SHEPHRD_INPUT_PATH=" + inputPath,
		"SHEPHRD_RESUME=" + boolEnv(resume),
		"SHEPHRD_CONFIG=" + configFile,
		"SHEPHRD_HERDR_GENERATION=" + fmt.Sprint(generation),
		"SHEPHRD_RUN_GENERATION=" + fmt.Sprint(attempt.RunGeneration),
		"SHEPHRD_HERDR_LIFECYCLE_SEQ=1",
		"SHEPHRD_RUNNER_PROCESS=1",
		"SHEPHRD_DRIVER_HARNESS=",
		"SHEPHRD_DRIVER_MODEL=",
	}
	if attempt.Harness == "pi" {
		environment = append(environment, "SHEPHRD_PI_WATCHER_ENABLED=0")
	}
	spec := terminal.WorkspaceSpec{
		WindowID:    parent.WindowID,
		WorkspaceID: parent.WorkspaceID,
		CWD:         attempt.WorktreePath,
		Label:       workerLabelFor(task),
		Description: fmt.Sprintf("Shephrd ephemeral worker %s run %d", attempt.ID, attempt.RunGeneration),
		Harness:     attempt.Harness,
		Source:      fmt.Sprintf("shephrd:%s:%d", attempt.ID, generation),
		Generation:  generation,
		Environment: environment,
	}
	if attempt.TerminalCreateIntent != nil && attempt.TerminalCreateIntent.State == model.TerminalCreateIntentPending {
		return fmt.Errorf("attempt %s has an unresolved terminal create intent for run %d (source %s, label %s); a previous launch may have left an orphaned terminal endpoint. Inspect the terminal, close any orphaned endpoint, and clear the intent before launching", attempt.ID, attempt.TerminalCreateIntent.RunGeneration, attempt.TerminalCreateIntent.Source, attempt.TerminalCreateIntent.Label)
	}
	intent := model.TerminalCreateIntent{
		RunGeneration: attempt.RunGeneration,
		Backend:       attempt.RuntimeBackend,
		Source:        spec.Source,
		WindowID:      spec.WindowID,
		WorkspaceID:   spec.WorkspaceID,
		CWD:           spec.CWD,
		Label:         spec.Label,
		Generation:    generation,
	}
	if err := s.Store.SetTerminalCreateIntentForRun(attempt.ID, attempt.RunGeneration, intent); err != nil {
		return err
	}
	resolveIntentAborted := func() error {
		return s.Store.ResolveTerminalCreateIntentForRun(attempt.ID, attempt.RunGeneration, model.TerminalCreateIntentAborted)
	}
	var preparation terminal.PreparedWorkspace
	var endpoint terminal.Endpoint
	if preparer, ok := client.(terminal.WorkspacePreparer); ok {
		preparation, err = preparer.PrepareWorkspace(spec)
		if preparation != nil {
			endpoint = preparation.Endpoint()
		}
	} else {
		endpoint, err = client.CreateWorkspace(spec)
	}
	endpoint.ProviderVersion = selection.Diagnostics.ProviderVersion
	endpoint.ProtocolVersion = selection.Diagnostics.ProtocolVersion
	endpoint.Capabilities = append([]string(nil), selection.Diagnostics.Capabilities...)
	if err != nil {
		if selection.Provider.ValidateEndpoint(endpoint) == nil {
			if _, storeErr := s.Store.SetTerminalEndpointForRun(attempt.ID, attempt.RunGeneration, modelEndpoint(endpoint)); storeErr != nil {
				return fmt.Errorf("%v; persist failed terminal endpoint: %w", err, storeErr)
			}
			if resolveErr := s.Store.ResolveTerminalCreateIntentForRun(attempt.ID, attempt.RunGeneration, model.TerminalCreateIntentCommitted); resolveErr != nil {
				return errors.Join(err, resolveErr)
			}
			return err
		}
		if terminal.CreateEffectFromError(endpoint, err) == terminal.CreateEffectNone {
			if resolveErr := resolveIntentAborted(); resolveErr != nil {
				return errors.Join(err, resolveErr)
			}
		}
		return err
	}
	if err := selection.Provider.ValidateEndpoint(endpoint); err != nil {
		if preparation != nil {
			if abortErr := preparation.Abort(); abortErr != nil {
				return errors.Join(err, fmt.Errorf("exact terminal cleanup is uncertain: %w", abortErr))
			}
		} else {
			if closeErr := client.Close(endpoint); closeErr != nil {
				return errors.Join(err, fmt.Errorf("exact terminal cleanup is uncertain: %w", closeErr))
			}
		}
		if resolveErr := resolveIntentAborted(); resolveErr != nil {
			return errors.Join(err, resolveErr)
		}
		return err
	}
	cleanup := true
	defer func() {
		if !cleanup || err == nil {
			return
		}
		if preparation != nil {
			if abortErr := preparation.Abort(); abortErr != nil {
				err = errors.Join(err, fmt.Errorf("terminal preparation cleanup is uncertain: %w", abortErr))
				return
			}
		}
		latest, latestErr := s.Store.Attempt(attempt.ID)
		if latestErr == nil {
			attempt = latest
		}
		if s.processAlive(attempt.RunnerPID) {
			if stopErr := s.stopProcess(attempt.RunnerPID); stopErr != nil {
				err = errors.Join(err, fmt.Errorf("terminal cleanup is uncertain: %w", stopErr))
				return
			}
		}
		info, infoErr := client.ProcessInfo(endpoint)
		if infoErr != nil {
			err = errors.Join(err, fmt.Errorf("terminal cleanup process attribution is uncertain: %w", infoErr))
			return
		}
		pid, pidErr := runnerPID(info, attempt)
		if pidErr != nil {
			err = errors.Join(err, pidErr)
			return
		}
		if pid > 0 {
			if stopErr := s.stopProcess(pid); stopErr != nil {
				err = errors.Join(err, fmt.Errorf("terminal cleanup is uncertain: %w", stopErr))
				return
			}
			info, infoErr = client.ProcessInfo(endpoint)
			if infoErr != nil {
				err = errors.Join(err, fmt.Errorf("terminal cleanup process attribution is uncertain: %w", infoErr))
				return
			}
		}
		if foregroundWorker(info, attempt) {
			err = errors.Join(err, fmt.Errorf("terminal cleanup is uncertain because a worker process remains"))
			return
		}
		if closeErr := client.Close(endpoint); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("terminal cleanup is uncertain: %w", closeErr))
		}
	}()
	storedGeneration, err := s.Store.SetTerminalEndpointForRun(attempt.ID, attempt.RunGeneration, modelEndpoint(endpoint))
	if err != nil {
		var closeErr error
		if preparation != nil {
			closeErr = preparation.Abort()
		} else {
			closeErr = client.Close(endpoint)
		}
		cleanup = false
		if closeErr != nil {
			return errors.Join(err, fmt.Errorf("exact terminal cleanup after persistence failure is uncertain: %w", closeErr))
		}
		if resolveErr := resolveIntentAborted(); resolveErr != nil {
			return errors.Join(err, resolveErr)
		}
		return err
	}
	if storedGeneration != generation {
		generationErr := fmt.Errorf("terminal runtime generation changed while launching attempt %s", attempt.ID)
		var closeErr error
		if preparation != nil {
			closeErr = preparation.Abort()
		} else {
			closeErr = client.Close(endpoint)
		}
		cleanup = false
		if closeErr != nil {
			return errors.Join(generationErr, fmt.Errorf("exact terminal cleanup is uncertain: %w", closeErr))
		}
		if resolveErr := resolveIntentAborted(); resolveErr != nil {
			return errors.Join(generationErr, resolveErr)
		}
		return generationErr
	}
	if err := s.Store.ResolveTerminalCreateIntentForRun(attempt.ID, attempt.RunGeneration, model.TerminalCreateIntentCommitted); err != nil {
		var closeErr error
		if preparation != nil {
			closeErr = preparation.Abort()
		} else {
			closeErr = client.Close(endpoint)
		}
		cleanup = false
		if closeErr != nil {
			return errors.Join(err, fmt.Errorf("exact terminal cleanup after intent persistence failure is uncertain: %w", closeErr))
		}
		return err
	}
	attempt.RuntimeGeneration = storedGeneration
	attempt.TerminalEndpoint = ptrModelEndpoint(modelEndpoint(endpoint))
	if preparation != nil {
		if err := preparation.Commit(); err != nil {
			return err
		}
	}
	if attempt.Harness == "pi" || attempt.Harness == "claude-code" {
		if err := client.ReportAgent(endpoint, terminal.AgentReport{Source: terminalLifecycleSource(attempt.ID, attempt.RunGeneration), Agent: attempt.Harness, State: "working", Sequence: 1}); err != nil {
			return err
		}
	}
	command := "exec " + shellQuote(launcher)
	if err := client.Start(endpoint, command); err != nil {
		return err
	}
	deadline := s.now().Add(10 * time.Second)
	for s.now().Before(deadline) {
		state, inspectErr := client.Inspect(endpoint)
		if inspectErr != nil {
			if terminal.IsNotFound(inspectErr) {
				return fmt.Errorf("terminal worker endpoint disappeared before runner readiness")
			}
			s.sleep(25 * time.Millisecond)
			continue
		}
		if state.Endpoint.WindowID != endpoint.WindowID || state.Endpoint.WorkspaceID != endpoint.WorkspaceID || state.Endpoint.TabID != endpoint.TabID || state.Endpoint.PaneID != endpoint.PaneID || state.Endpoint.SurfaceID != endpoint.SurfaceID {
			return fmt.Errorf("terminal worker endpoint identity changed before runner readiness")
		}
		info, infoErr := client.ProcessInfo(endpoint)
		if infoErr == nil {
			pid, pidErr := runnerPID(info, attempt)
			if pidErr != nil {
				return pidErr
			}
			if pid > 0 {
				if err := s.Store.SetRunnerForRun(attempt.ID, attempt.RunGeneration, pid, true); err != nil {
					return err
				}
				_, _ = client.Read(endpoint, 200)
				cleanup = false
				return nil
			}
		}
		s.sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("terminal worker runner did not become ready within 10s")
}

func ptrModelEndpoint(endpoint model.TerminalEndpoint) *model.TerminalEndpoint {
	return &endpoint
}

func modelEndpoint(endpoint terminal.Endpoint) model.TerminalEndpoint {
	return model.TerminalEndpoint{Backend: endpoint.Backend, SocketPath: endpoint.SocketPath, WindowID: endpoint.WindowID, WorkspaceID: endpoint.WorkspaceID, TabID: endpoint.TabID, PaneID: endpoint.PaneID, SurfaceID: endpoint.SurfaceID, ProviderVersion: endpoint.ProviderVersion, ProtocolVersion: endpoint.ProtocolVersion, Capabilities: append([]string(nil), endpoint.Capabilities...)}
}

func terminalEndpoint(endpoint model.TerminalEndpoint) terminal.Endpoint {
	return terminal.Endpoint{Backend: endpoint.Backend, SocketPath: endpoint.SocketPath, WindowID: endpoint.WindowID, WorkspaceID: endpoint.WorkspaceID, TabID: endpoint.TabID, PaneID: endpoint.PaneID, SurfaceID: endpoint.SurfaceID, ProviderVersion: endpoint.ProviderVersion, ProtocolVersion: endpoint.ProtocolVersion, Capabilities: append([]string(nil), endpoint.Capabilities...)}
}

func (s Service) writeTerminalLauncher(taskID string, attempt model.Attempt, inputPath, executable, configFile string, resume, ownedCleanup bool) (string, error) {
	dir := filepath.Join(s.Config.DataDir, taskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create terminal launcher directory: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("attempt-%s-run-%d-launcher.sh", attempt.ID, attempt.RunGeneration))
	args := []string{"_run", attempt.ID, "--input", inputPath, "--run-generation", fmt.Sprint(attempt.RunGeneration)}
	if resume {
		args = append(args, "--resume")
	}
	var body strings.Builder
	body.WriteString("#!/bin/sh\nset -eu\nunset SHEPHRD_DRIVER_HARNESS SHEPHRD_DRIVER_MODEL SHEPHRD_SUBDRIVER_ID SHEPHRD_SUBDRIVER_GENERATION SHEPHRD_SUBDRIVER_TOKEN SHEPHRD_COORDINATOR_ID SHEPHRD_COORDINATOR_GENERATION SHEPHRD_COORDINATOR_TOKEN\n")
	fmt.Fprintf(&body, "export SHEPHRD_EXECUTABLE=%s\nexport SHEPHRD_CONFIG=%s\nexport SHEPHRD_RUNNER_PROCESS=1\nexport PATH=%s\n", shellQuote(executable), shellQuote(configFile), shellQuote(s.getenv("PATH")))
	if attempt.Harness == "pi" {
		body.WriteString("export SHEPHRD_PI_WATCHER_ENABLED=0\n")
	}
	if ownedCleanup {
		body.WriteString("export SHEPHRD_TERMINAL_OWNED=1\n")
	}
	body.WriteString("exec ")
	body.WriteString(shellQuote(executable))
	for _, arg := range args {
		body.WriteByte(' ')
		body.WriteString(shellQuote(arg))
	}
	body.WriteByte('\n')
	if err := os.WriteFile(path, []byte(body.String()), 0o700); err != nil {
		return "", fmt.Errorf("write terminal launcher: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return "", fmt.Errorf("protect terminal launcher: %w", err)
	}
	return path, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func terminalLifecycleSource(attemptID string, runGeneration int) string {
	return fmt.Sprintf("shephrd:%s:run:%d", attemptID, runGeneration)
}

func boolEnv(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func (s Service) mergedEnvironment(overrides map[string]string) []string {
	base := s.environ()
	environment := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if workerBoundaryEnvironment(key) {
			continue
		}
		if _, replaced := overrides[key]; !replaced {
			environment = append(environment, entry)
		}
	}
	for key, value := range overrides {
		if !workerBoundaryEnvironment(key) {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}

func workerBoundaryEnvironment(key string) bool {
	return strings.HasPrefix(key, "SHEPHRD_SUBDRIVER_") || strings.HasPrefix(key, "SHEPHRD_COORDINATOR_") || key == "SHEPHRD_DRIVER_HARNESS" || key == "SHEPHRD_DRIVER_MODEL" || key == "SHEPHRD_TERMINAL_OWNED" || key == "CMUX_SOCKET_PASSWORD" || key == "CMUX_SOCKET_CAPABILITY"
}

func driverContextEnvironment(key string) bool {
	return workerBoundaryEnvironment(key)
}

func runnerPID(info terminal.ProcessInfo, attempt model.Attempt) (int, error) {
	matches := make(map[int]bool)
	for _, process := range info.ForegroundProcesses {
		text := strings.Join(append(process.Argv, process.Cmdline), " ")
		matchesInvocation := strings.Contains(text, "_run") && strings.Contains(text, attempt.ID)
		matchesExecutable := process.Path != "" && sameExecutable(process.Path, attempt.RuntimeExecutable)
		if !matchesInvocation && !matchesExecutable {
			continue
		}
		pid := process.PID
		if process.PGID > 0 {
			pid = process.PGID
		} else if info.ForegroundProcessGroup > 0 {
			pid = info.ForegroundProcessGroup
		}
		if pid <= 0 {
			return 0, &terminal.RuntimeError{Kind: terminal.ErrorAmbiguous, Operation: "attribute terminal runner", Cause: fmt.Errorf("matching process has no safe identity")}
		}
		matches[pid] = true
	}
	if len(matches) > 1 {
		return 0, &terminal.RuntimeError{Kind: terminal.ErrorAmbiguous, Operation: "attribute terminal runner", Cause: fmt.Errorf("multiple process groups match the worker")}
	}
	for pid := range matches {
		return pid, nil
	}
	return 0, nil
}

func sameExecutable(first, second string) bool {
	if first == "" || second == "" {
		return false
	}
	firstResolved, firstErr := filepath.EvalSymlinks(first)
	secondResolved, secondErr := filepath.EvalSymlinks(second)
	if firstErr == nil && secondErr == nil {
		return firstResolved == secondResolved
	}
	return filepath.Clean(first) == filepath.Clean(second)
}

func (s Service) ClearTerminalCreateIntent(taskID string) (model.TerminalCreateIntent, error) {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return model.TerminalCreateIntent{}, err
	}
	if task.CurrentAttemptID == "" {
		return model.TerminalCreateIntent{}, fmt.Errorf("task %s has no worker attempt", taskID)
	}
	attempt, err := s.Store.Attempt(task.CurrentAttemptID)
	if err != nil {
		return model.TerminalCreateIntent{}, err
	}
	intent := attempt.TerminalCreateIntent
	if intent == nil || intent.State != model.TerminalCreateIntentPending {
		return model.TerminalCreateIntent{}, fmt.Errorf("attempt %s has no unresolved terminal create intent", attempt.ID)
	}
	if s.processAlive(attempt.RunnerPID) {
		return model.TerminalCreateIntent{}, fmt.Errorf("attempt %s runner is still alive; stop it before clearing the terminal create intent", attempt.ID)
	}
	if err := s.Store.ResolveTerminalCreateIntentForRun(attempt.ID, intent.RunGeneration, model.TerminalCreateIntentAbandoned); err != nil {
		return model.TerminalCreateIntent{}, err
	}
	return *intent, nil
}

func (s Service) endpoint(attempt model.Attempt) terminal.Endpoint {
	if attempt.TerminalEndpoint != nil {
		return terminalEndpoint(*attempt.TerminalEndpoint)
	}
	return terminal.Endpoint{Backend: attempt.RuntimeBackend}
}

func (s Service) endpointComplete(endpoint terminal.Endpoint) bool {
	providers, err := s.terminalProviders()
	if err != nil {
		return false
	}
	provider, exists := providers[endpoint.Backend]
	return exists && provider.ValidateEndpoint(endpoint) == nil
}

func (s Service) endpointGone(attempt model.Attempt) (bool, error) {
	if !isTerminalRuntime(attempt.RuntimeBackend) {
		return true, nil
	}
	endpoint := s.endpoint(attempt)
	if endpoint.SocketPath == "" && endpoint.WindowID == "" && endpoint.WorkspaceID == "" && endpoint.TabID == "" && endpoint.PaneID == "" && endpoint.SurfaceID == "" {
		return true, nil
	}
	if !s.endpointComplete(endpoint) {
		return false, fmt.Errorf("attempt %s has incomplete terminal endpoint identity", attempt.ID)
	}
	client, err := s.terminalClientFor(attempt)
	if err != nil {
		return false, err
	}
	_, err = client.Inspect(endpoint)
	if terminal.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func (s Service) terminalClientFor(attempt model.Attempt) (terminal.RuntimeClient, error) {
	endpoint := s.endpoint(attempt)
	if !s.endpointComplete(endpoint) {
		return nil, fmt.Errorf("attempt %s has incomplete terminal endpoint identity", attempt.ID)
	}
	client, err := s.terminalClient(attempt.RuntimeBackend, endpoint.SocketPath)
	if err != nil {
		return nil, err
	}
	if endpoint.ProtocolVersion == "" && len(endpoint.Capabilities) == 0 {
		return client, nil
	}
	diagnostics, err := client.Diagnostics()
	if err != nil {
		return nil, err
	}
	if endpoint.ProtocolVersion != "" && diagnostics.ProtocolVersion != endpoint.ProtocolVersion {
		return nil, &terminal.RuntimeError{Kind: terminal.ErrorIncompatible, Operation: "validate persisted terminal endpoint", Cause: fmt.Errorf("protocol version changed")}
	}
	available := make(map[string]bool, len(diagnostics.Capabilities))
	for _, capability := range diagnostics.Capabilities {
		available[capability] = true
	}
	for _, capability := range endpoint.Capabilities {
		if !available[capability] {
			return nil, &terminal.RuntimeError{Kind: terminal.ErrorIncompatible, Operation: "validate persisted terminal endpoint", Cause: fmt.Errorf("capability set changed")}
		}
	}
	return client, nil
}

func (s Service) finalizeTerminalEndpoint(attempt model.Attempt) error {
	if !isTerminalRuntime(attempt.RuntimeBackend) {
		return nil
	}
	endpoint := s.endpoint(attempt)
	if endpoint.SocketPath == "" && endpoint.WindowID == "" && endpoint.WorkspaceID == "" && endpoint.TabID == "" && endpoint.PaneID == "" && endpoint.SurfaceID == "" {
		return nil
	}
	if !s.endpointComplete(endpoint) {
		return fmt.Errorf("attempt %s has incomplete terminal endpoint identity", attempt.ID)
	}
	client, err := s.terminalClientFor(attempt)
	if err != nil {
		return err
	}
	if _, err := client.Inspect(endpoint); err != nil {
		if terminal.IsNotFound(err) {
			return nil
		}
		return err
	}
	info, err := client.ProcessInfo(endpoint)
	if err != nil {
		return err
	}
	if foregroundWorker(info, attempt) {
		return fmt.Errorf("attempt %s still has a foreground terminal worker process", attempt.ID)
	}
	return client.Close(endpoint)
}

func foregroundWorker(info terminal.ProcessInfo, attempt model.Attempt) bool {
	for _, process := range info.ForegroundProcesses {
		if process.Path != "" && sameExecutable(process.Path, attempt.RuntimeExecutable) {
			return true
		}
		text := strings.ToLower(strings.Join(append(process.Argv, process.Cmdline), " "))
		if strings.Contains(text, strings.ToLower(attempt.ID)) || strings.Contains(text, "_run ") {
			return true
		}
		switch attempt.Harness {
		case "claude-code":
			if strings.Contains(text, "claude") {
				return true
			}
		case "pi":
			if strings.Contains(text, " pi ") || strings.Contains(text, "/pi ") || strings.HasPrefix(text, "pi ") {
				return true
			}
		case "codex":
			if strings.Contains(text, "codex") {
				return true
			}
		}
	}
	return false
}

func (s Service) requireTerminalEndpointGone(attempt model.Attempt) error {
	gone, err := s.endpointGone(attempt)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("attempt %s terminal endpoint is still present; reconcile it before resuming", attempt.ID)
	}
	return nil
}

func (s Service) waitForTerminalEndpointGone(attempt model.Attempt) error {
	deadline := s.now().Add(4 * time.Second)
	for s.now().Before(deadline) {
		gone, err := s.endpointGone(attempt)
		if err != nil {
			return err
		}
		if gone {
			return nil
		}
		if !s.processAlive(attempt.RunnerPID) {
			if err := s.finalizeTerminalEndpoint(attempt); err == nil {
				return nil
			}
		}
		s.sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("attempt %s terminal endpoint did not disappear after stop", attempt.ID)
}

func (s Service) EndpointStatus(taskID string) (bool, error) {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return false, err
	}
	if task.CurrentAttemptID == "" {
		return false, nil
	}
	attempt, err := s.Store.Attempt(task.CurrentAttemptID)
	if err != nil {
		return false, err
	}
	gone, err := s.endpointGone(attempt)
	return !gone, err
}

func (s Service) Focus(taskID string) error {
	attempt, err := s.currentAttempt(taskID)
	if err != nil {
		return err
	}
	if !isTerminalRuntime(attempt.RuntimeBackend) {
		return fmt.Errorf("task %s does not use a terminal runtime", taskID)
	}
	client, err := s.terminalClientFor(attempt)
	if err != nil {
		return err
	}
	return client.Focus(s.endpoint(attempt))
}

type PeekResult struct {
	SchemaVersion   int    `json:"schema_version"`
	TaskID          string `json:"task_id"`
	AttemptID       string `json:"attempt_id"`
	Label           string `json:"label"`
	Lines           int    `json:"lines"`
	EndpointPresent bool   `json:"endpoint_present"`
	InvocationEnded bool   `json:"invocation_ended"`
	Output          string `json:"output"`
}

func (s Service) Peek(taskID string, lines int) (PeekResult, error) {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return PeekResult{}, err
	}
	attempt, err := s.currentAttempt(taskID)
	if err != nil {
		return PeekResult{}, err
	}
	if !isTerminalRuntime(attempt.RuntimeBackend) {
		return PeekResult{}, fmt.Errorf("task %s does not use a terminal runtime", taskID)
	}
	if lines <= 0 {
		lines = 200
	}
	client, err := s.terminalClientFor(attempt)
	if err != nil {
		return PeekResult{}, err
	}
	output, err := client.Read(s.endpoint(attempt), lines)
	if terminal.IsNotFound(err) {
		return PeekResult{TaskID: task.ID, AttemptID: attempt.ID, Label: workerLabelFor(task), Lines: lines, InvocationEnded: true}, nil
	}
	if err != nil {
		return PeekResult{}, err
	}
	return PeekResult{SchemaVersion: 2, TaskID: task.ID, AttemptID: attempt.ID, Label: workerLabelFor(task), Lines: lines, EndpointPresent: true, Output: output}, nil
}

func (s Service) currentAttempt(taskID string) (model.Attempt, error) {
	task, err := s.Store.Task(taskID)
	if err != nil {
		return model.Attempt{}, err
	}
	if task.CurrentAttemptID == "" {
		return model.Attempt{}, fmt.Errorf("task %s has no worker attempt", taskID)
	}
	return s.Store.Attempt(task.CurrentAttemptID)
}
