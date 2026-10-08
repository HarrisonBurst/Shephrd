package control

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/brief"
	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/repository"
	"shephrd/internal/runner"
	"shephrd/internal/terminal"
)

const subdriverStartupBytes = 48 * 1024
const subdriverTurnTimeout = 20 * time.Minute

func SubdriverEnvironmentFence() (model.SubdriverFence, error) {
	current := [3]string{os.Getenv("SHEPHRD_SUBDRIVER_ID"), os.Getenv("SHEPHRD_SUBDRIVER_GENERATION"), os.Getenv("SHEPHRD_SUBDRIVER_TOKEN")}
	legacy := [3]string{os.Getenv("SHEPHRD_COORDINATOR_ID"), os.Getenv("SHEPHRD_COORDINATOR_GENERATION"), os.Getenv("SHEPHRD_COORDINATOR_TOKEN")}
	if current != [3]string{} && legacy != [3]string{} && current != legacy {
		return model.SubdriverFence{}, fmt.Errorf("conflicting sub-driver environment identities")
	}
	if current == [3]string{} {
		current = legacy
	}
	if current == [3]string{} {
		return model.SubdriverFence{}, nil
	}
	generation, err := strconv.Atoi(current[1])
	if current[0] == "" || current[2] == "" || err != nil || generation < 1 {
		return model.SubdriverFence{}, fmt.Errorf("incomplete or invalid sub-driver environment identity")
	}
	if os.Getenv("SHEPHRD_WORKER") == "1" {
		return model.SubdriverFence{}, fmt.Errorf("ordinary workers cannot inherit a sub-driver identity")
	}
	return model.SubdriverFence{ID: current[0], Generation: generation, Token: current[2]}, nil
}

func (s Service) SubdriverContext(id string) (string, error) {
	page, err := s.Store.SubdriverPage(id, 0)
	if err != nil {
		return "", err
	}
	return s.renderSubdriverContext(page)
}

func (s Service) renderSubdriverContext(page model.SubdriverPage) (string, error) {
	c := page.Subdriver
	id := c.ID
	var out strings.Builder
	fmt.Fprintf(&out, "# Repository sub-driver %s\nDurable owner: %s. Generation: %d.\n", c.ID, c.DriverID(), c.Generation)
	out.WriteString(`You are a sub-driver, not an implementation worker. Interpret the original requests faithfully, investigate technical ambiguity, plan and supervise ordinary workers. Only material results, blockers, user questions and scoped cross-repo proposals return to the main driver. Do not ask the main to plan or supervise your workers.
One sub-driver layer only. You cannot create sub-drivers, adopt tasks, or act in other repositories. No new merge, release, publish, discard or approval authority is granted. Carry scoped read-only limits, selected workflows and memory opt-outs into every worker brief. Descriptive permissions, checkpoints, receipt acknowledgements, results, readiness and verification are not lifecycle authority. Read-only requests remain read-only. Ask the original driver for missing authority. Do not mutate the repository root except permitted memory contributions; implementation belongs in worker worktrees.
Read repository instructions before work. No workflow is selected by discovery. Load only relevant memory topics when enabled and permitted by repository/request guidance. Do not load transcripts, all memory, or completed histories. Check current operational facts before consequential actions. Keep the checkpoint to immediate continuation and evidence pointers, not a task database copy.
Read each pending request in full using 'shephrd subdriver request <request-id> --json' before acting. If an event has read_command, load that exact event before handling it. Original content and context are authoritative intake, not a rewritten worker plan. User replies identify their question by reply_to. Preserve the original scope, actual user replies and existing task objective/acceptance contract; ask main to resolve conflicting delivery scope instead of inventing routing restrictions. Several requests share this owner; keep every return and dispatch correlated. Check explicit lead_request_id and related request/worker links, paging inspect and reading linked tasks, before deduplicating or declaring work absent. A rejected dispatch establishes no new worker for that dispatch, not absence of related work.
Plan directly or use existing records. Dispatch a worker with 'shephrd subdriver dispatch <request-id> --key <stable-step-key> --feature <feature> --deliverable code|report --acceptance <criteria> <objective> --json'. Creation is idempotent and queues the worker. Inspect the returned task, then use 'shephrd worker spawn <task-id> --json' exactly once for a queued task. Confirm the successful spawn receipt and current run before saying assigned or started; a queued task is not a running worker. On replay inspect the existing task; never retry or spawn a replacement merely because the conversation is new. Use existing worker send/recovery and task inspect/annotation mechanics for your own workers only. Gate delivery and hold claims on successful command receipts. A busy worker send rejection means no follow-up was queued or delivered and no hold was applied. Annotations and narrative plans are not delivered instructions.
Return with 'shephrd subdriver return <request-id> --key <stable-return-key> --kind question|result|blocker|handoff <text> --json'. A handoff proposes a scoped sibling assignment to main; main explicitly routes it with --lead-request. A result immediately completes the conversational request; reserve it for actual completion, never assignment or progress. Worker completion is separate and does not complete the request; a result is not a worker artifact or delivery proof. Late corrections, questions, blockers and handoffs remain correlated but do not reopen a done request or permit new dispatch. Ask main to route further work as a new correlated open intake using 'shephrd subdriver handoff --lead-request <completed-request-id>'. Ordinary replies to nonterminal lead questions, blockers or handoffs continue through subdriver reply; existing dispatch-key replay remains idempotent.
Recovery events request inspection of existing goals and workers, never replacement dispatch or new authority. After substantively handling an intake/reply/recovery event, run 'shephrd subdriver handled <event-id> --json'. The runtime owns notification claims for this turn; do not manually drain. Handle supplied worker notifications and acknowledge with 'shephrd wake ack --claim-token <token> --json'. This records handling only, not approval or task disposition. Do not poll, sleep, keep alive, invent work or wait for workers.
Finish this bounded turn after handling the supplied batch. Persist a small structured checkpoint followed by a done event without artifact. This ends only the disposable session, not any request or worker. Use the subdriver return command for user-facing outcomes. Example:
<shephrd-event>{"type":"checkpoint","payload":"Waiting for assigned worker","checkpoint":{"schema_version":1,"summary":"Waiting for assigned worker; inspect recorded task on notification.","completed":[],"next_steps":[],"decisions":[{"decision":"Wait for the existing worker","reason":"The assigned task is still running"}],"changed_paths":[],"checks":[{"command":"shephrd task inspect <task-id> --json","result":"Existing worker is running"}],"blockers":[]}}</shephrd-event>
<shephrd-event>{"type":"done","payload":"Turn handled"}</shephrd-event>
Use actual observed facts, not example claims. Checkpoint fields are exactly type, payload, checkpoint. The checkpoint object fields are exactly schema_version (integer 1), summary (string), completed, next_steps, decisions, changed_paths, checks, blockers. completed, next_steps, changed_paths and blockers are string arrays. decisions is an array of objects with string decision and reason fields; checks is an array of objects with string command and result fields. Never use string arrays for decisions or checks; use [] when there are none. Keep the entire checkpoint within 4096 bytes. Terminal fields are exactly type and payload; no artifact.
Before emission, preflight the exact authored output with 'shephrd protocol validate --role subdriver --file /path/to/authored-output.txt --json', or pass it unchanged on stdin with --file -. Correct rejected fields and validate the full output again. This read-only format check does not publish notifications, acknowledge input, accept artifacts, authorize actions, prove delivery, or guarantee later model compliance. Ingestion still checks current identity and lifecycle state.
`)
	if c.RepoID != "" {
		repo, err := s.Store.Repo(c.RepoID)
		if err != nil {
			return "", err
		}
		overview, err := repository.ProjectContextPath(repo.Path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "\nRepository scope: %s (%s), root %s. Read README.md and registered guidance %s.\n", repo.Name, repo.ID, repo.Path, repo.ContextFile)
		if overview != "" {
			fmt.Fprintf(&out, "Read the lean overview %s. No workflow module is selected by discovery.\n", overview)
		}
		out.WriteString(brief.MemoryGuidance(s.Config.Memory.Enabled))
	} else {
		fmt.Fprintf(&out, "\nExplicit general/research context (no repository authority):\n%s\nResearch directly; do not provision repositories or workers.\n", c.Context)
	}
	if c.RepoID == "" && !s.Config.Memory.Enabled {
		fmt.Fprintln(&out, brief.MemoryDisabledInstruction)
	}
	fmt.Fprintf(&out, "\nCompact checkpoint (context only):\n%s\n", c.Checkpoint)
	for i := range page.Requests {
		page.Requests[i].Original = "Read with subdriver request " + page.Requests[i].ID
		page.Requests[i].Context = "Read with subdriver request " + page.Requests[i].ID
	}
	page.Subdriver.Checkpoint = ""
	body, err := json.Marshal(page)
	if err != nil {
		return "", err
	}
	if len(body)+out.Len() > subdriverStartupBytes-8*1024 {
		page.Events = nil
		page.Requests = nil
		page.Workers = nil
		page.Next = "shephrd subdriver inspect " + id + " --json (page omitted to stay within 48 KiB; read before acting)"
		body, _ = json.Marshal(page)
	}
	fmt.Fprintf(&out, "\nOperational page (all omissions have pointers):\n%s\n", body)
	fmt.Fprintf(&out, "Worker state: shephrd task obligations --driver-id %s --json. Page through subdriver inspect using its next command.\n", c.DriverID())
	return out.String(), nil
}

func (s Service) ResumeSubdriver(id string, foreground bool) (model.Subdriver, error) {
	return s.ResumeSubdriverWithModelSelection(id, foreground, "", false, nil)
}

func (s Service) ResumeSubdriverWithModelSelection(id string, foreground bool, modelID string, modelProvided bool, generation *int) (result model.Subdriver, launchErr error) {
	c, err := s.Store.Subdriver(id)
	if err != nil {
		return c, err
	}
	if modelProvided && generation == nil {
		return c, fmt.Errorf("--model requires --generation with the exact inspected sub-driver generation")
	}
	if generation != nil && (*generation < 0 || c.Generation != *generation) {
		return c, fmt.Errorf("sub-driver generation changed")
	}
	if c.State != "idle" {
		return c, fmt.Errorf("sub-driver %s is %s; inspect and recover only after process absence is proven", id, c.State)
	}
	if s.processAlive(c.RunnerPID) || s.processAlive(c.HarnessPID) {
		return c, fmt.Errorf("sub-driver process is still live or uncertain; retain the current selection")
	}
	if err := s.settleSubdriverEndpoint(c); err != nil {
		return c, err
	}
	pending, err := s.Store.SubdriverPending(id)
	if err != nil {
		return c, err
	}
	if !pending {
		return c, nil
	}
	harness := c.Harness
	if !modelProvided {
		modelID = c.Model
	}
	if harness == "" {
		harness, modelID, err = s.resolveInitialWorkerSelection(c.RepoID, "", modelID, modelProvided)
		if err != nil {
			return c, err
		}
	}
	if err = adapter.Validate(harness); err != nil {
		return c, err
	}
	selection, err := s.selectRuntime(c.Runtime)
	if err != nil {
		return c, err
	}
	if foreground && selection.Backend != "headless" {
		return c, fmt.Errorf("foreground sub-driver execution requires configured headless runtime")
	}
	if _, err = s.SubdriverContext(id); err != nil {
		return c, err
	}
	f, err := s.Store.ReserveSubdriver(id, c.Generation, harness, modelID, selection.Backend)
	if err != nil {
		return c, err
	}
	defer func() {
		if launchErr != nil {
			_ = s.Store.FinishSubdriver(f, c.Checkpoint, launchErr.Error())
		}
	}()
	if foreground {
		err = s.RunSubdriver(f, io.Discard)
		latest, _ := s.Store.Subdriver(id)
		return latest, err
	}
	executable, err := s.workerExecutable()
	if err != nil {
		_ = s.Store.FinishSubdriver(f, c.Checkpoint, err.Error())
		return c, err
	}
	dir := filepath.Join(s.Config.DataDir, id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		_ = s.Store.FinishSubdriver(f, c.Checkpoint, err.Error())
		return c, err
	}
	configPath, err := config.Path()
	if err != nil {
		return c, err
	}
	args := []string{"subdriver", "_run", id, "--generation", strconv.Itoa(f.Generation), "--token", f.Token}
	env := s.subdriverEnvironment(f, executable, configPath)
	if selection.Backend == "headless" {
		log, err := os.OpenFile(filepath.Join(dir, "launcher.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return c, err
		}
		defer log.Close()
		cmd := exec.Command(executable, args...)
		cmd.Env = env
		cmd.Stdout = log
		cmd.Stderr = log
		s.detachProcess(cmd)
		if err = cmd.Start(); err != nil {
			_ = s.Store.FinishSubdriver(f, c.Checkpoint, err.Error())
			return c, err
		}
		_ = cmd.Process.Release()
	} else {
		client, err := s.terminalClient(selection.Backend, selection.Parent.SocketPath)
		if err != nil {
			return c, err
		}
		cwd, err := s.subdriverCWD(c)
		if err != nil {
			return c, err
		}
		label, err := s.subdriverLabel(c)
		if err != nil {
			return c, err
		}
		endpoint, err := client.CreateWorkspace(terminal.WorkspaceSpec{WindowID: selection.Parent.WindowID, WorkspaceID: selection.Parent.WorkspaceID, CWD: cwd, Label: label, Harness: harness, Source: "shephrd:coordinator:" + id + ":" + strconv.Itoa(f.Generation), Generation: f.Generation, Environment: env})
		if err != nil {
			_ = s.Store.FinishSubdriver(f, c.Checkpoint, "terminal create uncertain: "+err.Error())
			return c, err
		}
		if err = selection.Provider.ValidateEndpoint(endpoint); err != nil {
			return c, err
		}
		if err = s.Store.SetSubdriverEndpoint(f, modelEndpoint(endpoint)); err != nil {
			return c, err
		}
		command := shellQuote(executable)
		for _, arg := range args {
			command += " " + shellQuote(arg)
		}
		if err = client.Start(endpoint, "exec "+command); err != nil {
			_ = s.Store.FinishSubdriver(f, c.Checkpoint, "terminal start uncertain: "+err.Error())
			return c, err
		}
	}
	return s.Store.Subdriver(id)
}

func (s Service) subdriverEnvironment(f model.SubdriverFence, executable, configPath string) []string {
	env := s.mergedEnvironment(map[string]string{"SHEPHRD_PI_WATCHER_ENABLED": "0", "SHEPHRD_EXECUTABLE": executable, "SHEPHRD_CONFIG": configPath, "SHEPHRD_WORKER": "", "SHEPHRD_ATTEMPT_ID": ""})
	return append(env, "SHEPHRD_SUBDRIVER_ID="+f.ID, "SHEPHRD_SUBDRIVER_GENERATION="+strconv.Itoa(f.Generation), "SHEPHRD_SUBDRIVER_TOKEN="+f.Token,
		"SHEPHRD_COORDINATOR_ID="+f.ID, "SHEPHRD_COORDINATOR_GENERATION="+strconv.Itoa(f.Generation), "SHEPHRD_COORDINATOR_TOKEN="+f.Token)
}
func (s Service) subdriverLabel(c model.Subdriver) (string, error) {
	if c.RepoID == "" {
		return SubdriverLabel(""), nil
	}
	r, err := s.Store.Repo(c.RepoID)
	if err != nil {
		return "", err
	}
	return SubdriverLabel(r.Name), nil
}
func (s Service) subdriverCWD(c model.Subdriver) (string, error) {
	if c.RepoID != "" {
		r, err := s.Store.Repo(c.RepoID)
		return r.Path, err
	}
	path := filepath.Join(s.Config.DataDir, c.ID, "research")
	return path, os.MkdirAll(path, 0700)
}
func (s Service) RunSubdriver(f model.SubdriverFence, raw io.Writer) (runErr error) {
	c, err := s.Store.Subdriver(f.ID)
	if err != nil {
		return err
	}
	session := adapter.NewSessionID(c.Harness)
	if err = s.Store.StartSubdriver(f, os.Getpid(), session); err != nil {
		return err
	}
	reporting := subdriverReporting{store: s.Store, fence: f, session: session, checkpoint: c.Checkpoint}
	failure := "sub-driver turn did not settle"
	defer func() {
		if runErr != nil {
			failure = runErr.Error()
		}
		if err := s.Store.FinishSubdriver(f, reporting.checkpoint, failure); err != nil && runErr == nil {
			runErr = err
		}
	}()
	page, err := s.Store.SubdriverPage(f.ID, 0)
	if err != nil {
		return err
	}
	prompt, err := s.renderSubdriverContext(page)
	if err != nil {
		return err
	}
	generation := "coordinator:" + strconv.Itoa(f.Generation)
	notices, err := s.Store.DrainNotifications("", c.DriverID(), generation, 5, 30*time.Minute)
	if err != nil {
		failure = err.Error()
		return err
	}
	for _, notice := range notices.Notifications {
		fullCommand := "shephrd subdriver notification " + notice.NotificationID + " --json"
		if len(notice.Payload) > 1024 || len(notice.ReportLifecycle) > 0 {
			notice.Payload = "Full notification content omitted. Read before handling: " + fullCommand
			notice.ReportLifecycle = nil
		}
		body, _ := json.Marshal(notice)
		if len(body)+len(prompt) > subdriverStartupBytes {
			return fmt.Errorf("notification context exceeds 48 KiB; retain claims and recover after inspecting %s", fullCommand)
		}
		prompt += "\nWorker notification:\n" + string(body)
	}
	cwd, err := s.subdriverCWD(c)
	if err != nil {
		failure = err.Error()
		return err
	}
	label, err := s.subdriverLabel(c)
	if err != nil {
		failure = err.Error()
		return err
	}
	attempt := model.Attempt{ID: f.ID, Harness: c.Harness, Model: c.Model, SessionID: session, WorktreePath: cwd, RunGeneration: f.Generation, RuntimeBackend: c.Runtime, TerminalEndpoint: c.Endpoint}
	interactive := isTerminalRuntime(c.Runtime) && (c.Harness == "pi" || c.Harness == "claude-code")
	executable, err := s.workerExecutable()
	if err != nil {
		return err
	}
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	env := s.subdriverEnvironment(f, executable, cfgPath)
	env = append(env, "SHEPHRD_DRIVER_HARNESS="+c.Harness, "SHEPHRD_DRIVER_MODEL="+c.Model)
	dir := filepath.Join(s.Config.DataDir, f.ID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("session-%d.log", f.Generation)), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	reporting.log = log
	state := runner.State{
		SetRunner: func(int) error { return s.Store.CheckSubdriverFence(f) },
		SetSession: func(session string) error {
			if reporting.cursor != 0 || reporting.repair != nil {
				return fmt.Errorf("sub-driver reporting session cannot change")
			}
			if err := s.Store.SubdriverSession(f, session); err != nil {
				return err
			}
			reporting.session = session
			return nil
		},
		UpdateCursor: func(int64) error { return s.Store.CheckSubdriverFence(f) },
		Ingest: func(e model.Event, cursor int64) error {
			_, err := reporting.ingest([]model.Event{e}, cursor, reporting.session, runner.RepairRequest{}, false)
			return err
		},
		IngestCandidate:        reporting.ingest,
		RecordAdapterRejection: reporting.reject,
		RecordControlFailure:   func(reason string) error { failure = reason; return nil },
		RecordRunnerDeath:      func(reason string) error { failure = reason; return nil },
		Finish: func(code int, reason string) error {
			if code != 0 && !interactive || reason != "" || !reporting.finished {
				failure = fmt.Sprintf("sub-driver turn held (exit %d): %s", code, reason)
				return nil
			}
			for _, event := range page.Events {
				handled, err := s.Store.SubdriverEventHandled(event.ID)
				if err != nil {
					return err
				}
				if !handled {
					failure = fmt.Sprintf("event %d remains unhandled; inspect before recovery", event.ID)
					return nil
				}
			}
			for _, notice := range notices.Notifications {
				latest, err := s.Store.Notification(notice.NotificationID)
				if err != nil {
					return err
				}
				if latest.State != model.NotificationAcknowledged && latest.State != model.NotificationSuperseded {
					failure = "worker notification remains unhandled: " + notice.NotificationID
					return nil
				}
			}
			failure = ""
			return nil
		},
	}
	started := func(pid int) error { return s.Store.SubdriverHarnessPID(f, pid) }
	if interactive {
		err = s.runInteractiveSubdriver(runner.InteractiveConfig{
			ParseCandidate: adapter.ParseSubdriverCandidate, ParseCandidateSet: adapter.ParseSubdriverCandidateSet, Attempt: attempt, Output: raw, Log: log,
			Environment: env, State: state, Lifecycle: s.runnerLifecycle(attempt, f.Generation),
			Timeout: subdriverTurnTimeout, HarnessStarted: started,
		}, prompt, label, executable, dir)
	} else {
		invocation, buildErr := adapter.BuildNamed(c.Harness, attempt, prompt, false, label)
		if buildErr != nil {
			return buildErr
		}
		_, err = runner.RunHeadless(runner.HeadlessConfig{
			ParseCandidate: adapter.ParseSubdriverCandidateSet, Attempt: attempt, Invocation: invocation,
			RepairInvocation: func(session, prompt string) (adapter.Invocation, error) {
				if c.Harness != "pi" {
					return adapter.Invocation{}, fmt.Errorf("tool-free sub-driver reporting repair is supported only for Pi")
				}
				repairAttempt := attempt
				repairAttempt.SessionID = session
				invocation, err := adapter.BuildNamed(c.Harness, repairAttempt, prompt+"\nSub-driver session terminal must be done without artifact. Never dispatch, return, acknowledge or repeat prior actions.", true, "")
				invocation.Args = append([]string{"--no-tools"}, invocation.Args...)
				return invocation, err
			},
			Log: log, Raw: raw, Terminal: isTerminalRuntime(c.Runtime), Environment: env, State: state,
			Timeout: subdriverTurnTimeout, HarnessStarted: started,
		})
	}
	if err != nil {
		failure = err.Error()
		return err
	}
	if failure != "" {
		return fmt.Errorf("%s", failure)
	}
	return nil
}

func (s Service) settleSubdriverEndpoint(c model.Subdriver) error {
	if c.Endpoint == nil {
		return nil
	}
	if s.processAlive(c.RunnerPID) || s.processAlive(c.HarnessPID) {
		return fmt.Errorf("sub-driver terminal process is still live or uncertain")
	}
	client, err := s.terminalClient(c.Endpoint.Backend, c.Endpoint.SocketPath)
	if err != nil {
		return err
	}
	endpoint := terminalEndpoint(*c.Endpoint)
	info, err := client.ProcessInfo(endpoint)
	if terminal.Classify(err) == terminal.ErrorEndpointAbsent {
		return nil
	}
	if err != nil {
		return err
	}
	for _, p := range info.ForegroundProcesses {
		if p.PID != info.ShellPID {
			return fmt.Errorf("sub-driver terminal has a foreground process; preserve the exact endpoint")
		}
	}
	if err = client.Close(endpoint); err != nil {
		return err
	}
	_, err = client.Inspect(endpoint)
	if terminal.Classify(err) != terminal.ErrorEndpointAbsent {
		return fmt.Errorf("sub-driver endpoint absence is not proven after close")
	}
	return nil
}

func (s Service) PumpSubdrivers(driver string) error {
	candidates, err := s.Store.SubdriverCandidates(driver)
	if err != nil {
		return err
	}
	started := 0
	for _, c := range candidates {
		if c.State == "running" && c.RunnerPID > 0 && !s.processAlive(c.RunnerPID) {
			if err := s.Store.HoldSubdriverObservation(c, "Recorded sub-driver runner exited; inspect process and endpoint state before explicit recovery"); err != nil {
				return err
			}
			continue
		}
		if c.State == "starting" && s.now().Sub(c.UpdatedAt) > 10*time.Second {
			if err := s.Store.HoldSubdriverObservation(c, "Sub-driver launch did not register a runner within 10 seconds; process identity is uncertain"); err != nil {
				return err
			}
			continue
		}
		if c.State != "idle" {
			continue
		}
		if s.processAlive(c.RunnerPID) || s.processAlive(c.HarnessPID) {
			continue
		}
		if c.Endpoint != nil {
			if err := s.settleSubdriverEndpoint(c); err != nil {
				if holdErr := s.Store.HoldSubdriverObservation(c, err.Error()); holdErr != nil {
					return holdErr
				}
				continue
			}
		}
		pending, err := s.Store.SubdriverPending(c.ID)
		if err != nil {
			return err
		}
		if !pending {
			continue
		}
		if _, err = s.ResumeSubdriver(c.ID, false); err != nil {
			current, readErr := s.Store.Subdriver(c.ID)
			if readErr != nil {
				return readErr
			}
			if current.State == "idle" {
				if holdErr := s.Store.HoldSubdriverObservation(c, err.Error()); holdErr != nil {
					return holdErr
				}
			}
			continue
		}
		started++
		if started == 2 {
			break
		}
	}
	return nil
}
func (s Service) RecoverSubdriver(id string, generation int, launchAbsentReason ...string) error {
	reason := ""
	if len(launchAbsentReason) > 0 {
		reason = strings.TrimSpace(launchAbsentReason[0])
	}
	c, err := s.Store.Subdriver(id)
	if err != nil {
		return err
	}
	if c.Generation != generation {
		return fmt.Errorf("sub-driver generation changed")
	}
	if (c.RunnerPID == 0 || c.HarnessPID == 0 || c.Runtime != "headless" && c.Endpoint == nil) && reason == "" {
		return fmt.Errorf("sub-driver process or endpoint identity is incomplete; inspect launch logs and exact source shephrd:coordinator:%s:%d; retain held state unless absence is explicitly confirmed with --launch-absent <reason>", id, generation)
	}
	if s.processAlive(c.RunnerPID) || s.processAlive(c.HarnessPID) {
		return fmt.Errorf("sub-driver process is still live or PID has been reused; retain held state")
	}
	if c.Endpoint != nil {
		client, err := s.terminalClient(c.Endpoint.Backend, c.Endpoint.SocketPath)
		if err != nil {
			return err
		}
		_, err = client.ProcessInfo(terminalEndpoint(*c.Endpoint))
		if terminal.Classify(err) != terminal.ErrorEndpointAbsent {
			return fmt.Errorf("terminal absence is not proven; inspect the recorded endpoint")
		}
	}
	return s.Store.RecoverSubdriver(id, generation, reason)
}
