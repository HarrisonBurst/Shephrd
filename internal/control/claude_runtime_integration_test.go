package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"shephrd/internal/claudebridge"
	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
)

func TestInteractiveClaudeHerdrInitialAndFollowupUseNativeTUIBridge(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	argsPath := filepath.Join(root, "claude-args.jsonl")
	service, state, task, attempt := interactiveClaudeFixture(t, root, dataDir, "question", argsPath)
	defer state.Close()
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "initial prompt")
	var initialOutput bytes.Buffer
	if err := service.RunAttempt(attempt.ID, inputPath, false, &initialOutput, attempt.RunGeneration); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(initialOutput.String(), "CLAUDE_FAKE_INTERACTIVE_TUI") || strings.Contains(initialOutput.String(), `{"type":"system"`) {
		t.Fatalf("initial pane output=%q", initialOutput.String())
	}
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "waiting" || current.ProcessAlive {
		t.Fatalf("question state=%+v", current)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetTerminalEndpointForRun(attempt.ID, generation, model.TerminalEndpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t3", PaneID: "w7:p3"}); err != nil {
		t.Fatal(err)
	}
	attempt, _ = state.Attempt(attempt.ID)
	reportPath := filepath.Join(dataDir, task.ID, "report.md")
	t.Setenv("SHEPHRD_TEST_CLAUDE_MODE", "done")
	t.Setenv("SHEPHRD_TEST_REPORT", reportPath)
	followupPath := writeInput(t, dataDir, task.ID, "follow-up.md", "follow-up prompt")
	var followupOutput bytes.Buffer
	if err := service.RunAttempt(attempt.ID, followupPath, true, &followupOutput, generation); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(followupOutput.String(), "CLAUDE_FAKE_INTERACTIVE_TUI") || strings.Contains(followupOutput.String(), `{"type":"system"`) {
		t.Fatalf("follow-up pane output=%q", followupOutput.String())
	}
	current, _ = state.Task(task.ID)
	if current.Status != "done" || !current.ClaimedDone || !current.Landed {
		t.Fatalf("done state=%+v", current)
	}
	stored, _ := state.Attempt(attempt.ID)
	if stored.SessionID != attempt.SessionID || stored.RunnerPID != 0 {
		t.Fatalf("attempt=%+v", stored)
	}
	invocations := readArgv(t, argsPath)
	if len(invocations) != 2 {
		t.Fatalf("invocations=%q", invocations)
	}
	for _, args := range invocations {
		settingsPath := argumentValue(args, "--settings")
		if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
			t.Fatalf("private Claude settings were not removed: %s: %v", settingsPath, err)
		}
		for _, forbidden := range []string{"-p", "--print", "--output-format", "stream-json", "--verbose"} {
			if slices.Contains(args, forbidden) {
				t.Fatalf("interactive Claude args contain %q: %q", forbidden, args)
			}
		}
		if !slices.Contains(args, "--settings") || !slices.Contains(args, "--dangerously-skip-permissions") {
			t.Fatalf("interactive Claude bridge args=%q", args)
		}
	}
	if !slices.Contains(invocations[0], "--session-id") || !slices.Contains(invocations[0], "--name") || !slices.Contains(invocations[1], "--resume") || slices.Contains(invocations[1], "--name") {
		t.Fatalf("session argv=%q", invocations)
	}
	calls := herdrFixtureRunner(t, service).joinedCalls()
	for _, want := range []string{"w7:p2 --source shephrd:" + attempt.ID + ":run:1", "--state working --seq 1", "--state working --seq 2 --agent-session-id " + attempt.SessionID, "--state blocked --seq 3", "release-agent w7:p2", "w7:p3 --source shephrd:" + attempt.ID + ":run:2", "--state idle --seq 3", "release-agent w7:p3"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("lifecycle calls missing %q:\n%s", want, calls)
		}
	}
}

func TestInteractiveClaudeBackgroundWorkBlocksUntilFinalHandoff(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	service, state, task, attempt := interactiveClaudeFixture(t, root, dataDir, "background-hold", filepath.Join(root, "args"))
	defer state.Close()
	responsePath := filepath.Join(root, "hook-responses.jsonl")
	releasePath := filepath.Join(root, "release-background")
	t.Setenv("SHEPHRD_TEST_RESPONSE_PATH", responsePath)
	t.Setenv("SHEPHRD_TEST_BACKGROUND_RELEASE", releasePath)
	reportPath := filepath.Join(dataDir, task.ID, "report.md")
	t.Setenv("SHEPHRD_TEST_REPORT", reportPath)
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "prompt")
	runErr := make(chan error, 1)
	go func() { runErr <- service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration) }()
	waitFor(t, func() bool {
		body, err := os.ReadFile(responsePath)
		return err == nil && strings.Contains(string(body), `"decision":"block"`)
	})
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != model.TaskStatusWorking || !current.ProcessAlive {
		t.Fatalf("active background task settled early: %+v", current)
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-runErr; err != nil {
		t.Fatal(err)
	}
	responses, err := os.ReadFile(responsePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(responses)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"decision":"block"`) || !strings.Contains(lines[1], `"continue":false`) {
		t.Fatalf("hook responses=%q", responses)
	}
	current, _ = state.Task(task.ID)
	stored, _ := state.Attempt(attempt.ID)
	if current.Status != model.TaskStatusDone || !current.ClaimedDone || !current.Landed || current.ProcessAlive || stored.RunnerPID != 0 {
		t.Fatalf("task=%+v attempt=%+v", current, stored)
	}
	calls := herdrFixtureRunner(t, service).joinedCalls()
	if !strings.Contains(calls, "--state idle") || !strings.Contains(calls, "release-agent") || strings.Contains(calls, "--state blocked") {
		t.Fatalf("terminal lifecycle calls:\n%s", calls)
	}
}

func TestInteractiveClaudeBridgeFailuresBlockAndPreserveWorktree(t *testing.T) {
	originalTimeout := interactiveClaudeAcceptTimeout
	interactiveClaudeAcceptTimeout = 50 * time.Millisecond
	t.Cleanup(func() { interactiveClaudeAcceptTimeout = originalTimeout })
	for _, mode := range []string{"missing", "stale", "settled", "malformed", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			service, state, task, attempt := interactiveClaudeFixture(t, root, dataDir, mode, filepath.Join(root, "args"))
			defer state.Close()
			inputPath := writeInput(t, dataDir, task.ID, "brief.md", "prompt")
			if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err == nil {
				t.Fatal("bridge failure returned nil")
			}
			current, _ := state.Task(task.ID)
			stored, _ := state.Attempt(attempt.ID)
			if current.Status != "blocked" || current.ProcessAlive || stored.RunnerPID != 0 || stored.FailureReason == "" {
				t.Fatalf("task=%+v attempt=%+v", current, stored)
			}
			if _, err := os.Stat(attempt.WorktreePath); err != nil {
				t.Fatalf("worktree was not preserved: %v", err)
			}
			calls := herdrFixtureRunner(t, service).joinedCalls()
			if !strings.Contains(calls, "--state working --seq 1") || !strings.Contains(calls, "--state blocked") || !strings.Contains(calls, "release-agent") {
				t.Fatalf("failure lifecycle calls:\n%s", calls)
			}
		})
	}
}

func TestInteractiveClaudeCheckpointOnlyStopUsesNormalSettlementFailure(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	service, state, task, attempt := interactiveClaudeFixture(t, root, dataDir, "checkpoint-stop", filepath.Join(root, "args"))
	defer state.Close()
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "prompt")
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err == nil {
		t.Fatal("checkpoint-only Stop returned nil")
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.FailureReason, "settled without a valid terminal") || strings.Contains(stored.FailureReason, "reconciliation conflicts") {
		t.Fatalf("failure=%q", stored.FailureReason)
	}
}

func TestInteractiveClaudeAcceptedDoneSurvivesPostTerminalFailures(t *testing.T) {
	originalGrace := interactiveClaudeShutdownGrace
	interactiveClaudeShutdownGrace = 50 * time.Millisecond
	t.Cleanup(func() { interactiveClaudeShutdownGrace = originalGrace })
	for _, mode := range []string{"done-hold", "done-invalid"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			service, state, task, attempt := interactiveClaudeFixture(t, root, dataDir, mode, filepath.Join(root, "args"))
			defer state.Close()
			reportPath := filepath.Join(dataDir, task.ID, "report.md")
			t.Setenv("SHEPHRD_TEST_REPORT", reportPath)
			inputPath := writeInput(t, dataDir, task.ID, "brief.md", "prompt")
			if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
				t.Fatal(err)
			}
			current, _ := state.Task(task.ID)
			stored, _ := state.Attempt(attempt.ID)
			done, err := state.AcceptedDone(attempt.ID, attempt.RunGeneration)
			if err != nil {
				t.Fatal(err)
			}
			expectedArtifact := "report:" + reportPath
			if current.Status != "done" || !current.ClaimedDone || current.ArtifactRef != expectedArtifact || !current.Landed || current.ProcessAlive {
				t.Fatalf("task=%+v", current)
			}
			if done.ArtifactRef != expectedArtifact || stored.RunnerPID != 0 || stored.FailureReason != "" {
				t.Fatalf("done=%+v attempt=%+v", done, stored)
			}
			calls := herdrFixtureRunner(t, service).joinedCalls()
			if !strings.Contains(calls, "--state idle") || !strings.Contains(calls, "release-agent") || strings.Contains(calls, "--state blocked") {
				t.Fatalf("terminal lifecycle calls:\n%s", calls)
			}
		})
	}
}

func TestInteractiveClaudeRecoversStopBeforeFinalMessageDisplay(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	service, state, task, attempt := interactiveClaudeFixture(t, root, dataDir, "done-stop-before-final", filepath.Join(root, "args"))
	defer state.Close()
	reportPath := filepath.Join(dataDir, task.ID, "report.md")
	t.Setenv("SHEPHRD_TEST_REPORT", reportPath)
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "prompt")
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
		t.Fatal(err)
	}
	current, _ := state.Task(task.ID)
	stored, _ := state.Attempt(attempt.ID)
	done, err := state.AcceptedDoneMessage(task.ID, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := state.VerifiedArtifactForDoneMessage(done.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("report\n"))
	if current.Status != "done" || !current.ClaimedDone || !current.Landed || current.ArtifactRef != "report:"+reportPath {
		t.Fatalf("task=%+v", current)
	}
	if stored.FailureReason != "" || stored.RunnerPID != 0 || stored.ReleaseState != "held" {
		t.Fatalf("attempt=%+v", stored)
	}
	if artifact == nil || artifact.OriginalRef != current.ArtifactRef || artifact.SHA256 != fmt.Sprintf("%x", digest) || artifact.SizeBytes != int64(len("report\n")) {
		t.Fatalf("artifact=%+v", artifact)
	}
	messages, err := state.Messages(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpointCount, doneCount, blockedCount := 0, 0, 0
	var checkpointCursor int64
	for _, message := range messages {
		if message.Direction == "worker-to-driver" && !message.Stale {
			switch message.Type {
			case "checkpoint":
				checkpointCount++
				checkpointCursor = message.SourceCursor
			case "done":
				doneCount++
			case "blocked":
				blockedCount++
			}
		}
	}
	if checkpointCount != 1 || doneCount != 1 || blockedCount != 0 || done.SourceCursor != checkpointCursor+1 {
		t.Fatalf("checkpoint_count=%d done_count=%d blocked_count=%d checkpoint_cursor=%d done=%+v messages=%+v", checkpointCount, doneCount, blockedCount, checkpointCursor, done, messages)
	}
	calls := herdrFixtureRunner(t, service).joinedCalls()
	if !strings.Contains(calls, "--state idle") || !strings.Contains(calls, "release-agent") || strings.Contains(calls, "--state blocked") {
		t.Fatalf("terminal lifecycle calls:\n%s", calls)
	}
}

func TestClaudeBridgeHarnessProcess(t *testing.T) {
	if os.Getenv("SHEPHRD_TEST_CLAUDE_HELPER") != "1" {
		return
	}
	args := os.Args
	if index := slices.Index(args, "--"); index >= 0 {
		args = args[index+1:]
	}
	if path := os.Getenv("SHEPHRD_TEST_ARGS_PATH"); path != "" {
		file, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		encoded, _ := json.Marshal(args)
		fmt.Fprintln(file, string(encoded))
		file.Close()
	}
	settingsPath := argumentValue(args, "--settings")
	settings, err := os.ReadFile(settingsPath)
	if err != nil {
		os.Exit(4)
	}
	var configured struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if json.Unmarshal(settings, &configured) != nil || len(configured.Hooks) != 5 {
		os.Exit(5)
	}
	fmt.Fprintln(os.Stdout, "CLAUDE_FAKE_INTERACTIVE_TUI")
	mode := os.Getenv("SHEPHRD_TEST_CLAUDE_MODE")
	if mode == "missing" {
		os.Exit(0)
	}
	sessionID := argumentValue(args, "--session-id")
	source := "startup"
	if sessionID == "" {
		sessionID = argumentValue(args, "--resume")
		source = "resume"
	}
	if mode == "stale" {
		sessionID = "wrong-session"
	}
	if err := runClaudeHook(map[string]any{"session_id": sessionID, "hook_event_name": "SessionStart", "source": source}); err != nil {
		os.Exit(6)
	}
	if mode == "stale" {
		os.Exit(0)
	}
	if mode == "oversized" {
		if err := claudebridge.RunHook(strings.NewReader(strings.Repeat("x", claudebridge.MaxHookInputBytes+1)), os.Stdout); err != nil {
			os.Exit(7)
		}
		os.Exit(0)
	}
	if mode == "malformed" {
		if err := displayClaudeMessage(sessionID, "message", `<shephrd-event>{}`, true); err != nil {
			os.Exit(8)
		}
		os.Exit(0)
	}
	if mode == "settled" {
		if err := runClaudeHook(map[string]any{"session_id": sessionID, "hook_event_name": "Stop", "stop_hook_active": false, "last_assistant_message": "plain text"}); err != nil {
			os.Exit(9)
		}
		os.Exit(0)
	}
	checkpoint := `<shephrd-event>{"type":"checkpoint","payload":"Claude checkpoint","checkpoint":{"schema_version":1,"summary":"Claude checkpoint","completed":["connected"],"next_steps":["finish"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
	terminal := `<shephrd-event>{"type":"question","payload":"choose"}</shephrd-event>`
	if (mode == "background" || mode == "background-hold") || strings.HasPrefix(mode, "done") {
		report := os.Getenv("SHEPHRD_TEST_REPORT")
		os.MkdirAll(filepath.Dir(report), 0o700)
		os.WriteFile(report, []byte("report\n"), 0o600)
		terminal = `<shephrd-event>{"type":"done","payload":"complete","artifact":"report:` + report + `"}</shephrd-event>`
	}
	if mode == "checkpoint-stop" {
		if err := displayClaudeMessageBatch(sessionID, "message", 0, checkpoint, false); err != nil {
			os.Exit(10)
		}
		if err := runClaudeHook(map[string]any{"session_id": sessionID, "hook_event_name": "Stop", "stop_hook_active": false, "last_assistant_message": checkpoint}); err != nil {
			os.Exit(11)
		}
		os.Exit(0)
	}
	if mode == "done-stop-before-final" {
		if err := displayClaudeMessageBatch(sessionID, "message", 0, checkpoint, false); err != nil {
			os.Exit(10)
		}
		if err := runClaudeHook(map[string]any{"session_id": sessionID, "hook_event_name": "Stop", "stop_hook_active": false, "last_assistant_message": checkpoint + "\n" + terminal}); err != nil {
			os.Exit(11)
		}
		if err := displayClaudeMessageBatch(sessionID, "message", 1, "\n"+terminal, true); err != nil {
			os.Exit(12)
		}
		os.Exit(0)
	}
	if err := displayClaudeMessage(sessionID, "message", checkpoint+"\n"+terminal, true); err != nil {
		os.Exit(10)
	}
	if mode == "background" || mode == "background-hold" {
		active := map[string]any{"session_id": sessionID, "hook_event_name": "Stop", "stop_hook_active": false, "background_tasks": []any{
			map[string]any{"id": "agent-1", "type": "subagent", "status": "running", "description": "review"},
			map[string]any{"id": "watch-1", "type": "monitor", "status": "running", "description": "watch"},
			map[string]any{"id": "build-1", "type": "shell", "status": "running", "description": "build", "command": "nix build"},
		}, "last_assistant_message": terminal}
		if err := runClaudeHookFile(active, os.Getenv("SHEPHRD_TEST_RESPONSE_PATH")); err != nil {
			os.Exit(12)
		}
		if mode == "background-hold" {
			release := os.Getenv("SHEPHRD_TEST_BACKGROUND_RELEASE")
			deadline := time.Now().Add(30 * time.Second)
			for {
				if _, err := os.Stat(release); err == nil {
					break
				}
				if time.Now().After(deadline) {
					os.Exit(13)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		if err := displayClaudeMessage(sessionID, "completed", terminal, true); err != nil {
			os.Exit(13)
		}
		if err := runClaudeHookFile(map[string]any{"session_id": sessionID, "hook_event_name": "Stop", "stop_hook_active": false, "background_tasks": []any{}, "last_assistant_message": terminal}, os.Getenv("SHEPHRD_TEST_RESPONSE_PATH")); err != nil {
			os.Exit(14)
		}
		os.Exit(0)
	}
	if strings.HasPrefix(mode, "done") {
		if err := runClaudeHook(map[string]any{"session_id": sessionID, "hook_event_name": "Stop", "stop_hook_active": false, "background_tasks": []any{}, "last_assistant_message": terminal}); err != nil {
			os.Exit(12)
		}
	}
	if mode == "done-hold" {
		for {
			time.Sleep(time.Second)
		}
	}
	if mode == "done-invalid" {
		if err := claudebridge.RunHook(strings.NewReader(strings.Repeat("x", claudebridge.MaxHookInputBytes+1)), os.Stdout); err != nil {
			os.Exit(11)
		}
		os.Exit(0)
	}
	if !strings.HasPrefix(mode, "done") {
		if err := runClaudeHook(map[string]any{"session_id": sessionID, "hook_event_name": "Stop", "stop_hook_active": false, "background_tasks": []any{}, "last_assistant_message": terminal}); err != nil {
			os.Exit(12)
		}
	}
	os.Exit(0)
}

func interactiveClaudeFixture(t *testing.T, root, dataDir, mode, argsPath string) (Service, *store.Store, model.Task, model.Attempt) {
	t.Helper()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(binDir, "claude")
	body := "#!/bin/sh\nexec \"$SHEPHRD_TEST_BINARY\" -test.run '^TestClaudeBridgeHarnessProcess$' -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "interactive-claude", Objective: "show the real Claude TUI", Deliverable: "report"})
	attempt, _ := state.BeginAttempt(task.ID, "claude-code", "model")
	if err := state.SetRuntimeBackend(attempt.ID, "herdr"); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "2fb65785-bb28-4a78-9961-98545f5de52e", root, "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetTerminalEndpointForRun(attempt.ID, generation, model.TerminalEndpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}); err != nil {
		t.Fatal(err)
	}
	attempt, _ = state.Attempt(attempt.ID)
	runner := &lifecycleFixtureRunner{panes: map[string]string{"w7:p2": "w7:t2", "w7:p3": "w7:t3"}}
	service := New(config.Config{DataDir: dataDir}, state)
	service.TerminalProviders = []terminal.Provider{herdrClientProvider{Client: terminal.NewWithRunner("/socket", runner)}}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHEPHRD_TEST_BINARY", os.Args[0])
	t.Setenv("SHEPHRD_TEST_CLAUDE_HELPER", "1")
	t.Setenv("SHEPHRD_TEST_CLAUDE_MODE", mode)
	t.Setenv("SHEPHRD_TEST_ARGS_PATH", argsPath)
	t.Setenv("SHEPHRD_HERDR_LIFECYCLE_SEQ", "")
	return service, state, task, attempt
}

func displayClaudeMessage(sessionID, messageID, delta string, final bool) error {
	return displayClaudeMessageBatch(sessionID, messageID, 0, delta, final)
}

func displayClaudeMessageBatch(sessionID, messageID string, index int, delta string, final bool) error {
	return runClaudeHook(map[string]any{"session_id": sessionID, "hook_event_name": "MessageDisplay", "turn_id": "turn", "message_id": messageID, "index": index, "final": final, "delta": delta})
}

func runClaudeHook(input map[string]any) error {
	return runClaudeHookFile(input, "")
}

func runClaudeHookFile(input map[string]any, path string) error {
	body, _ := json.Marshal(input)
	output := io.Writer(os.Stdout)
	var file *os.File
	if path != "" {
		var err error
		file, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		output = file
	}
	return claudebridge.RunHook(bytes.NewReader(body), output)
}
