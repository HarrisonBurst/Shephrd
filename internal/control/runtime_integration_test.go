package control

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/pibridge"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
)

type lifecycleFixtureRunner struct {
	mu    sync.Mutex
	calls []string
	panes map[string]string
}

func (r *lifecycleFixtureRunner) Run(_ string, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, strings.Join(args, " "))
	if len(args) >= 3 && args[0] == "pane" && args[1] == "get" {
		pane := args[2]
		tab := r.panes[pane]
		return []byte(fmt.Sprintf(`{"result":{"pane":{"pane_id":%q,"tab_id":%q,"workspace_id":"w7"}}}`, pane, tab)), nil, nil
	}
	if len(args) >= 2 && args[0] == "pane" && (args[1] == "report-agent" || args[1] == "release-agent") {
		return []byte(`{"result":{"type":"ok"}}`), nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected Herdr call: %s", strings.Join(args, " "))
}

func (r *lifecycleFixtureRunner) joinedCalls() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.calls, "\n")
}

func TestInteractivePiHerdrInitialAndFollowupUseBridgeAndLifecycle(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	argsPath := filepath.Join(root, "pi-args.jsonl")
	releasePath := filepath.Join(root, "release")
	service, state, task, attempt := interactivePiFixture(t, root, dataDir, "question-hold", argsPath, releasePath)
	defer state.Close()
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "initial prompt")
	runDone := make(chan error, 1)
	go func() { runDone <- service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration) }()
	waitFor(t, func() bool {
		checkpoint, err := state.LatestCheckpoint(attempt.ID)
		return err == nil && checkpoint.Producer == "worker" && checkpoint.Summary == "bridge checkpoint"
	})
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "working" || !current.ProcessAlive {
		t.Fatalf("checkpoint became terminal: %+v", current)
	}
	calls := herdrFixtureRunner(t, service).joinedCalls()
	if !strings.Contains(calls, "--state working --seq 1") || !strings.Contains(calls, "--state working --seq 2 --agent-session-id "+attempt.SessionID) {
		t.Fatalf("working lifecycle calls:\n%s", calls)
	}
	if err := os.WriteFile(releasePath, []byte("continue"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	current, _ = state.Task(task.ID)
	if current.Status != "waiting" || current.ProcessAlive {
		t.Fatalf("question state = %+v", current)
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
	t.Setenv("SHEPHRD_TEST_BRIDGE_MODE", "done")
	t.Setenv("SHEPHRD_TEST_REPORT", reportPath)
	followupPath := writeInput(t, dataDir, task.ID, "follow-up.md", "follow-up prompt")
	if err := service.RunAttempt(attempt.ID, followupPath, true, nil, generation); err != nil {
		t.Fatal(err)
	}
	current, _ = state.Task(task.ID)
	if current.Status != "done" || !current.ClaimedDone {
		t.Fatalf("done state = %+v", current)
	}
	stored, _ := state.Attempt(attempt.ID)
	if stored.SessionID != attempt.SessionID || stored.RunnerPID != 0 {
		t.Fatalf("attempt = %+v", stored)
	}
	invocations := readArgv(t, argsPath)
	if len(invocations) != 2 {
		t.Fatalf("invocations = %q", invocations)
	}
	for _, args := range invocations {
		if slices.Contains(args, "--mode") || slices.Contains(args, "json") || slices.Contains(args, "--print") {
			t.Fatalf("interactive Pi args = %q", args)
		}
		if !slices.Contains(args, "--extension") || !slices.Contains(args, "--approve") {
			t.Fatalf("interactive Pi missing explicit trusted bridge: %q", args)
		}
	}
	if !slices.Contains(invocations[0], "--session-id") || !slices.Contains(invocations[1], "--session") {
		t.Fatalf("session argv = %q", invocations)
	}
	calls = herdrFixtureRunner(t, service).joinedCalls()
	for _, want := range []string{"w7:p2 --source shephrd:" + attempt.ID + ":run:1", "--state blocked --seq 3", "release-agent w7:p2", "w7:p3 --source shephrd:" + attempt.ID + ":run:2", "--state idle --seq 3", "release-agent w7:p3"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("lifecycle calls missing %q:\n%s", want, calls)
		}
	}
}

func TestWorkerEnvironmentsStripDriverContext(t *testing.T) {
	service := Service{environment: environmentCapability{list: func() []string {
		return []string{"SHEPHRD_PI_WATCHER_ENABLED=1", "SHEPHRD_DRIVER_HARNESS=pi", "SHEPHRD_DRIVER_MODEL=driver-model"}
	}}}
	for _, environment := range [][]string{
		service.workerEnvironment([]string{"EXTRA=value", "SHEPHRD_DRIVER_HARNESS=codex", "SHEPHRD_DRIVER_MODEL=worker-model"}),
		service.mergedEnvironment(map[string]string{"EXTRA": "value", "SHEPHRD_PI_WATCHER_ENABLED": "0", "SHEPHRD_DRIVER_HARNESS": "codex", "SHEPHRD_DRIVER_MODEL": "worker-model"}),
	} {
		watcher, extra := "", ""
		for _, value := range environment {
			if strings.HasPrefix(value, "SHEPHRD_DRIVER_HARNESS=") || strings.HasPrefix(value, "SHEPHRD_DRIVER_MODEL=") {
				t.Fatalf("driver context leaked into worker environment: %q", value)
			}
			if strings.HasPrefix(value, "SHEPHRD_PI_WATCHER_ENABLED=") {
				watcher = strings.TrimPrefix(value, "SHEPHRD_PI_WATCHER_ENABLED=")
			}
			if strings.HasPrefix(value, "EXTRA=") {
				extra = strings.TrimPrefix(value, "EXTRA=")
			}
		}
		if extra != "value" {
			t.Fatalf("extra=%q", extra)
		}
		if watcher != "" && watcher != "0" {
			t.Fatalf("watcher=%q", watcher)
		}
	}
}

func TestHeadlessPiRemainsMachineReadable(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(root, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$SHEPHRD_TEST_ARGS_PATH"
printf '%s' "${SHEPHRD_PI_WATCHER_ENABLED:-}" > "$SHEPHRD_TEST_ENV"
printf '%s\n' '{"type":"session","id":"native-session"}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"headless checkpoint\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"headless checkpoint\",\"completed\":[],\"next_steps\":[\"finish\"],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>"}],"stopReason":"stop"}}'
touch "$SHEPHRD_TEST_REPORT"
printf '%s\n' "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"<shephrd-event>{\\\"type\\\":\\\"done\\\",\\\"payload\\\":\\\"complete\\\",\\\"artifact\\\":\\\"report:$SHEPHRD_TEST_REPORT\\\"}</shephrd-event>\"}],\"stopReason\":\"stop\"}}"
printf '%s\n' '{"type":"agent_end"}'
`
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "headless", Objective: "machine output", Deliverable: "report"})
	attempt, _ := state.BeginAttempt(task.ID, "pi", "")
	if err := configureAttempt(t, state, attempt.ID, "native-session", root, "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dataDir, task.ID, "report.md")
	environmentPath := filepath.Join(dataDir, task.ID, "worker-environment")
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "prompt")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHEPHRD_PI_WATCHER_ENABLED", "1")
	t.Setenv("SHEPHRD_TEST_REPORT", reportPath)
	t.Setenv("SHEPHRD_TEST_ARGS_PATH", argsPath)
	t.Setenv("SHEPHRD_TEST_ENV", environmentPath)
	service := New(config.Config{DataDir: dataDir}, state)
	var output bytes.Buffer
	if err := service.RunAttempt(attempt.ID, inputPath, false, &output, generation); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--mode\njson\n--print") || !strings.Contains(output.String(), `{"type":"session"`) {
		t.Fatalf("args=%q output=%q", args, output.String())
	}
	environment, err := os.ReadFile(environmentPath)
	if err != nil || string(environment) != "0" {
		t.Fatalf("Pi worker watcher override = %q, err = %v", environment, err)
	}
	current, _ := state.Task(task.ID)
	if current.Status != "done" || !current.Landed {
		t.Fatalf("task = %+v", current)
	}
}

func TestReportWriteSurvivesHeadlessAndInteractiveProtocolHandoffFailure(t *testing.T) {
	t.Run("headless", func(t *testing.T) {
		state, task, attempt, reportBody, runErr := runHeadlessEventFixture(t, "pi", "checkpoint-invalid", 0, 0)
		if runErr != nil {
			t.Fatal(runErr)
		}
		assertProtocolReportRecoveryShape(t, state, task, attempt, reportBody)
	})
	t.Run("interactive", func(t *testing.T) {
		root := t.TempDir()
		dataDir := filepath.Join(root, "data")
		service, state, task, attempt := interactivePiFixture(t, root, dataDir, "report-handoff-failure", filepath.Join(root, "args"), filepath.Join(root, "release"))
		defer state.Close()
		reportPath := filepath.Join(dataDir, task.ID, "report.md")
		t.Setenv("SHEPHRD_TEST_REPORT", reportPath)
		inputPath := writeInput(t, dataDir, task.ID, "brief.md", "write report then hand off")
		err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration)
		if err == nil {
			t.Fatal("expected interactive protocol error")
		}
		switch err.Error() {
		case "Pi bridge closed before a terminal envelope", "Pi exited without a valid terminal <shephrd-event> envelope":
		default:
			t.Fatalf("interactive protocol error = %v", err)
		}
		body, err := os.ReadFile(reportPath)
		if err != nil {
			t.Fatal(err)
		}
		assertProtocolReportRecoveryShape(t, state, task, attempt, body)
	})
}

func assertProtocolReportRecoveryShape(t *testing.T, state *store.Store, task model.Task, attempt model.Attempt, reportBody []byte) {
	t.Helper()
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := state.LatestCheckpoint(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := state.Messages(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	acceptedDone := 0
	for _, message := range messages {
		acceptedDone += boolInt(message.AttemptID == attempt.ID && message.Direction == "worker-to-driver" && message.Type == "done" && !message.Stale)
	}
	if current.Status != model.TaskStatusBlocked || current.ClaimedDone || current.ArtifactRef != "" || current.Landed || current.CurrentAttemptID != attempt.ID || current.ProcessAlive ||
		stored.Status != model.AttemptStatusBlocked || stored.RunnerPID != 0 || stored.ReleaseState != "held" || stored.WorktreePath == "" || checkpoint.Producer != "worker" ||
		checkpoint.AttemptID != attempt.ID || checkpoint.RunGeneration != stored.RunGeneration || acceptedDone != 0 || len(reportBody) == 0 {
		t.Fatalf("task=%+v attempt=%+v checkpoint=%+v accepted_done=%d report_bytes=%d", current, stored, checkpoint, acceptedDone, len(reportBody))
	}
}

func TestHeadlessCompoundEventsAcrossPiAndCodexRuntimeBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		delay    time.Duration
		exitCode int
		done     bool
	}{
		{name: "compound immediate", mode: "compound", done: true},
		{name: "compound delayed", mode: "compound", delay: 100 * time.Millisecond, done: true},
		{name: "compound nonzero", mode: "compound", exitCode: 7, done: true},
		{name: "split records", mode: "split", done: true},
		{name: "later record cursor", mode: "cursor", done: true},
		{name: "question then done", mode: "question", done: true},
		{name: "duplicate terminal", mode: "duplicate"},
		{name: "invalid suffix", mode: "invalid"},
		{name: "no envelope", mode: "plain"},
	}
	for _, harness := range []string{"pi", "codex"} {
		for _, test := range tests {
			t.Run(harness+"/"+test.name, func(t *testing.T) {
				state, task, attempt, reportBody, runErr := runHeadlessEventFixture(t, harness, test.mode, test.delay, test.exitCode)
				if (runErr != nil) != (test.exitCode != 0) {
					t.Fatalf("run error=%v exit=%d", runErr, test.exitCode)
				}
				current, err := state.Task(task.ID)
				if err != nil {
					t.Fatal(err)
				}
				stored, err := state.Attempt(attempt.ID)
				if err != nil {
					t.Fatal(err)
				}
				messages, err := state.Messages(task.ID)
				if err != nil {
					t.Fatal(err)
				}
				notifications, err := state.Notifications(task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !test.done {
					workerCheckpoints, done, plainProgress := 0, 0, 0
					for _, message := range messages {
						workerCheckpoints += boolInt(message.Direction == "worker-to-driver" && message.Type == "checkpoint" && !message.Stale)
						done += boolInt(message.Type == "done" && !message.Stale)
						plainProgress += boolInt(message.Type == "progress" && message.Payload == "ordinary assistant progress" && !message.Stale)
					}
					if current.Status != model.TaskStatusBlocked || current.ProcessAlive || stored.Status != model.AttemptStatusBlocked || workerCheckpoints != 0 || done != 0 || len(notifications) != 1 || notifications[0].Kind != "blocked" || test.mode == "plain" && plainProgress != 1 {
						t.Fatalf("task=%+v attempt=%+v worker_checkpoints=%d done=%d plain_progress=%d notifications=%+v messages=%+v", current, stored, workerCheckpoints, done, plainProgress, notifications, messages)
					}
					return
				}
				firstCheckpointCursor, checkpointCursor, questionCursor, doneCursor := int64(0), int64(0), int64(0), int64(0)
				doneCount, questionCount, failedCount := 0, 0, 0
				for _, message := range messages {
					switch {
					case message.Direction == "worker-to-driver" && message.Type == "checkpoint" && !message.Stale:
						if firstCheckpointCursor == 0 {
							firstCheckpointCursor = message.SourceCursor
						}
						checkpointCursor = message.SourceCursor
					case message.Type == "question" && !message.Stale:
						questionCursor = message.SourceCursor
						questionCount++
					case message.Type == "done" && !message.Stale:
						doneCursor = message.SourceCursor
						doneCount++
					case message.Type == "failed" && !message.Stale:
						failedCount++
					}
				}
				if current.Status != model.TaskStatusDone || !current.ClaimedDone || !current.Landed || current.ProcessAlive || stored.ExitCode == nil || *stored.ExitCode != test.exitCode {
					t.Fatalf("task=%+v attempt=%+v", current, stored)
				}
				if checkpointCursor < 1 || doneCursor <= checkpointCursor || stored.Cursor <= doneCursor || doneCount != 1 || failedCount != 0 {
					t.Fatalf("checkpoint=%d done=%d attempt_cursor=%d done_count=%d failed_count=%d messages=%+v", checkpointCursor, doneCursor, stored.Cursor, doneCount, failedCount, messages)
				}
				switch test.mode {
				case "cursor":
					if doneCursor != checkpointCursor+2 {
						t.Fatalf("later native record cursor=%d checkpoint=%d messages=%+v", doneCursor, checkpointCursor, messages)
					}
				case "question":
					if firstCheckpointCursor+1 != questionCursor || questionCursor+1 != checkpointCursor || checkpointCursor+1 != doneCursor || questionCount != 1 || len(notifications) != 2 || notifications[0].Kind != "question" || notifications[0].SourceCursor != questionCursor || notifications[1].Kind != "done" || notifications[1].SourceCursor != doneCursor {
						t.Fatalf("first_checkpoint=%d question=%d checkpoint=%d done=%d question_count=%d notifications=%+v messages=%+v", firstCheckpointCursor, questionCursor, checkpointCursor, doneCursor, questionCount, notifications, messages)
					}
				default:
					if questionCount != 0 || len(notifications) != 1 || notifications[0].Kind != "done" || notifications[0].SourceCursor != doneCursor {
						t.Fatalf("question_count=%d notifications=%+v", questionCount, notifications)
					}
				}
				doneMessage, err := state.AcceptedDoneMessage(task.ID, attempt.ID)
				if err != nil {
					t.Fatal(err)
				}
				artifact, err := state.VerifiedArtifactForDoneMessage(doneMessage.ID)
				if err != nil || artifact == nil || artifact.SizeBytes != int64(len(reportBody)) || artifact.OriginalRef != current.ArtifactRef {
					t.Fatalf("artifact=%+v err=%v task=%+v", artifact, err, current)
				}
				snapshot, err := os.ReadFile(artifact.SnapshotPath)
				if err != nil || !bytes.Equal(snapshot, reportBody) {
					t.Fatalf("snapshot bytes=%d err=%v", len(snapshot), err)
				}
			})
		}
	}
}

func runHeadlessEventFixture(t *testing.T, harness, mode string, delay time.Duration, exitCode int) (*store.Store, model.Task, model.Attempt, []byte, error) {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	streamPath := filepath.Join(root, "stream.jsonl")
	script := "#!/bin/sh\nif [ -n \"$SHEPHRD_TEST_REPORT_SOURCE\" ]; then mkdir -p \"$(dirname \"$SHEPHRD_TEST_REPORT\")\"; cp \"$SHEPHRD_TEST_REPORT_SOURCE\" \"$SHEPHRD_TEST_REPORT\"; fi\ncat \"$SHEPHRD_TEST_STREAM\"\n"
	if delay > 0 {
		script += fmt.Sprintf("sleep %.3f\n", delay.Seconds())
	}
	script += fmt.Sprintf("exit %d\n", exitCode)
	if err := os.WriteFile(filepath.Join(binDir, harness), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: harness + "-" + mode, Objective: "compound events", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, harness, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetRuntimeBackend(attempt.ID, "headless"); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, harness+"-session", root, "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	reportBody := bytes.Repeat([]byte("R"), 46818)
	reportPath := filepath.Join(dataDir, task.ID, "report.md")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if mode == "checkpoint-invalid" {
		source := filepath.Join(root, "report-source.md")
		if err := os.WriteFile(source, reportBody, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SHEPHRD_TEST_REPORT_SOURCE", source)
		t.Setenv("SHEPHRD_TEST_REPORT", reportPath)
	} else if err := os.WriteFile(reportPath, reportBody, 0o600); err != nil {
		t.Fatal(err)
	}
	checkpoint := headlessEventEnvelope(t, model.Event{Type: "checkpoint", Payload: "compound checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "compound checkpoint", Completed: []string{"report written"}, NextSteps: []string{"emit done"}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
	freshCheckpoint := headlessEventEnvelope(t, model.Event{Type: "checkpoint", Payload: "fresh checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "fresh checkpoint", Completed: []string{"question answered"}, NextSteps: []string{"emit done"}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
	question := headlessEventEnvelope(t, model.Event{Type: "question", Payload: "review needed"})
	done := headlessEventEnvelope(t, model.Event{Type: "done", Payload: "compound complete", Artifact: "report:" + reportPath})
	progress := headlessEventEnvelope(t, model.Event{Type: "progress", Payload: "compound progress"})
	failed := headlessEventEnvelope(t, model.Event{Type: "failed", Payload: "duplicate terminal"})
	var texts []string
	switch mode {
	case "compound":
		texts = []string{checkpoint + "\n" + done}
	case "split":
		texts = []string{checkpoint, done}
	case "cursor":
		texts = []string{checkpoint + "\n" + progress, done}
	case "question":
		texts = []string{checkpoint, question, freshCheckpoint, done}
	case "duplicate":
		texts = []string{checkpoint + "\n" + done + "\n" + failed}
	case "invalid":
		texts = []string{checkpoint + `\n<shephrd-event>{"type":"done","payload":}</shephrd-event>`}
	case "checkpoint-invalid":
		texts = []string{checkpoint, `<shephrd-event>{"type":"done","payload":}</shephrd-event>`}
	case "plain":
		texts = []string{"ordinary assistant progress"}
	default:
		t.Fatalf("unknown mode %q", mode)
	}
	if err := os.WriteFile(streamPath, headlessNativeStream(t, harness, texts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHEPHRD_TEST_STREAM", streamPath)
	service := New(config.Config{DataDir: dataDir}, state)
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "prompt")
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	runErr := service.RunAttempt(attempt.ID, inputPath, false, nil, generation)
	return state, task, attempt, reportBody, runErr
}

func headlessEventEnvelope(t *testing.T, event model.Event) string {
	t.Helper()
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return "<shephrd-event>" + string(encoded) + "</shephrd-event>"
}

func headlessNativeStream(t *testing.T, harness string, texts []string) []byte {
	t.Helper()
	var records []any
	switch harness {
	case "pi":
		records = append(records, map[string]any{"type": "session", "id": "native-pi-session"})
		for _, text := range texts {
			records = append(records, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}, "stopReason": "stop"}})
		}
		records = append(records, map[string]any{"type": "agent_end"})
	case "codex":
		records = append(records, map[string]any{"type": "thread.started", "thread_id": "native-codex-session"})
		for _, text := range texts {
			records = append(records, map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": text}})
		}
		records = append(records, map[string]any{"type": "turn.completed"})
	default:
		t.Fatalf("unknown harness %q", harness)
	}
	var stream bytes.Buffer
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		stream.Write(encoded)
		stream.WriteByte('\n')
	}
	return stream.Bytes()
}

func TestInteractivePiBridgeFailuresBlockAndPreserveWorktree(t *testing.T) {
	for _, mode := range []string{"missing", "stale", "malformed", "oversized", "settled", "early"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			service, state, task, attempt := interactivePiFixture(t, root, dataDir, mode, filepath.Join(root, "args"), "")
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

func TestInteractivePiRepairsExactMalformedQuestionInProcess(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	service, state, task, attempt := interactivePiFixture(t, root, dataDir, "repair-question", filepath.Join(root, "args"), "")
	defer state.Close()
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "prompt")
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
		t.Fatal(err)
	}
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := state.Messages(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	repairs, questions, blocked := 0, 0, 0
	for _, message := range messages {
		switch {
		case message.Type == "protocol-repair":
			repairs++
			if message.Wake || message.Stale || message.SourceCursor != 3 || !strings.Contains(message.Payload, adapter.DiagnosticSchemaUnknownField) {
				t.Fatalf("repair audit=%+v", message)
			}
		case message.Type == "question" && !message.Stale:
			questions++
			if message.SourceCursor != 5 || strings.Contains(message.Payload, `"question"`) {
				t.Fatalf("question=%+v", message)
			}
		case message.Type == "blocked" && !message.Stale:
			blocked++
		}
	}
	if current.Status != model.TaskStatusWaiting || current.ProcessAlive || stored.Status != model.AttemptStatusWaiting || stored.Cursor != 5 || stored.RunGeneration != attempt.RunGeneration {
		t.Fatalf("task=%+v attempt=%+v", current, stored)
	}
	if repairs != 1 || questions != 1 || blocked != 0 || len(notifications) != 1 || notifications[0].Kind != "question" || notifications[0].SourceCursor != 5 {
		t.Fatalf("repairs=%d questions=%d blocked=%d notifications=%+v messages=%+v", repairs, questions, blocked, notifications, messages)
	}
}

func TestInteractivePiRepairTimeoutBlocksAndPreservesWorktree(t *testing.T) {
	original := interactivePiRepairTimeout
	interactivePiRepairTimeout = 30 * time.Millisecond
	t.Cleanup(func() { interactivePiRepairTimeout = original })
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	service, state, task, attempt := interactivePiFixture(t, root, dataDir, "repair-timeout", filepath.Join(root, "args"), "")
	defer state.Close()
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "prompt")
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error=%v", err)
	}
	current, _ := state.Task(task.ID)
	stored, _ := state.Attempt(attempt.ID)
	messages, _ := state.Messages(task.ID)
	repairs, blocked := 0, 0
	for _, message := range messages {
		repairs += boolInt(message.Type == "protocol-repair")
		blocked += boolInt(message.Type == "blocked" && !message.Stale)
	}
	if current.Status != model.TaskStatusBlocked || current.ProcessAlive || stored.Status != model.AttemptStatusBlocked || repairs != 1 || blocked != 1 {
		t.Fatalf("task=%+v attempt=%+v repairs=%d blocked=%d messages=%+v", current, stored, repairs, blocked, messages)
	}
	if _, err := os.Stat(attempt.WorktreePath); err != nil {
		t.Fatalf("worktree was not preserved: %v", err)
	}
}

func TestInteractivePiAcceptedDoneSurvivesPostTerminalFailures(t *testing.T) {
	originalGrace := interactivePiShutdownGrace
	interactivePiShutdownGrace = 50 * time.Millisecond
	t.Cleanup(func() { interactivePiShutdownGrace = originalGrace })
	for _, mode := range []string{"done-hold", "done-invalid", "done-oversized", "done-duplicate"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			service, state, task, attempt := interactivePiFixture(t, root, dataDir, mode, filepath.Join(root, "args"), "")
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
				t.Fatalf("task = %+v", current)
			}
			if done.ArtifactRef != expectedArtifact || stored.RunnerPID != 0 || stored.FailureReason != "" || stored.ReleaseState != "held" {
				t.Fatalf("done=%+v attempt=%+v", done, stored)
			}
			messages, err := state.Messages(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range messages {
				if message.Type == "blocked" && !message.Stale {
					t.Fatalf("accepted done regressed to blocked: %+v", messages)
				}
			}
			calls := herdrFixtureRunner(t, service).joinedCalls()
			if !strings.Contains(calls, "--state idle") || !strings.Contains(calls, "release-agent") || strings.Contains(calls, "--state blocked") {
				t.Fatalf("terminal lifecycle calls:\n%s", calls)
			}
		})
	}
}

func TestRealInteractivePiHerdrWorkerIsOptIn(t *testing.T) {
	if os.Getenv("SHEPHRD_REAL_HERDR_PI_E2E") != "1" {
		t.Skip("set SHEPHRD_REAL_HERDR_PI_E2E=1 to run a real interactive Pi worker in Herdr")
	}
	client := terminal.New(os.Getenv("HERDR_SOCKET_PATH"))
	parent, err := client.ValidateParent()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	databasePath := filepath.Join(root, "state.db")
	configPath := filepath.Join(root, "config.toml")
	binary := filepath.Join(root, "shephrd")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build E2E Shephrd: %s: %v", output, err)
	}
	configBody := fmt.Sprintf(`default_harness = "pi"
worker_runtime = "herdr"
database_path = %q
data_dir = %q

[wake]
enabled = true
default_batch = 10
max_batch = 20
claim_ttl = "5m"
claim_ttl_min = "30s"
claim_ttl_max = "30m"
driver_id = "driver:e2e"

[notifications]
enabled = false
details = false
task_per_minute = 2
global_per_minute = 10

[pi_watcher]
enabled = false
poll_min = "1s"
poll_max = "15s"
`, databasePath, dataDir)
	configBody += liveHerdrExtensionConfig(t, filepath.Clean(filepath.Join(workingDirectory, "..", "..")), root)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "herdr-e2e", Path: root, DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:e2e", RepoID: repo.ID, FeatureKey: "interactive-pi", Objective: "HERDR_INTERACTIVE_PI_E2E prove the real TUI", Deliverable: "report"})
	modelName := os.Getenv("SHEPHRD_REAL_HERDR_PI_MODEL")
	if modelName == "" {
		modelName = "openai-codex/gpt-5.6-luna"
	}
	attempt, _ := state.BeginAttempt(task.ID, "pi", modelName)
	if err := state.SetRuntimeBackend(attempt.ID, "herdr"); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, adapter.NewSessionID("pi"), root, "e2e-lease", "e2e-branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"complete the E2E"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dataDir, task.ID, "report.md")
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", fmt.Sprintf("HERDR_INTERACTIVE_PI_E2E. Use the bash tool to run sleep 3 first so the active TUI can be observed. Then write a short report to %s. Emit this exact checkpoint shape with truthful content: <shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"E2E checkpoint\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"E2E checkpoint\",\"completed\":[\"Real Pi TUI ran\"],\"next_steps\":[\"Finish with done\"],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>. Then finish with exactly <shephrd-event>{\"type\":\"done\",\"payload\":\"E2E complete\",\"artifact\":\"report:%s\"}</shephrd-event>.", reportPath, reportPath))
	endpoint, err := client.CreateTab(terminal.TabSpec{WorkspaceID: parent.WorkspaceID, CWD: root, Label: "shephrd-real-pi-e2e", Harness: "pi", Source: "shephrd:e2e:display", Environment: []string{"SHEPHRD_CONFIG=" + configPath, "SHEPHRD_PI_WATCHER_ENABLED=0", "SHEPHRD_HERDR_LIFECYCLE_SEQ=1", "PI_SKIP_VERSION_CHECK=1"}})
	if err != nil {
		t.Fatal(err)
	}
	paneOwned := true
	defer func() {
		if paneOwned {
			_ = client.CloseExactPane(endpoint)
		}
	}()
	if _, err := state.SetTerminalEndpointForRun(attempt.ID, generation, model.TerminalEndpoint{Backend: "herdr", SocketPath: endpoint.SocketPath, WorkspaceID: endpoint.WorkspaceID, TabID: endpoint.TabID, PaneID: endpoint.PaneID}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportAgent(endpoint, terminal.AgentReport{Source: terminalLifecycleSource(attempt.ID, generation), Agent: "pi", State: "working", Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	command := "exec " + shellQuote(binary) + " _run " + shellQuote(attempt.ID) + " --input " + shellQuote(inputPath) + " --run-generation " + fmt.Sprint(generation)
	if err := client.Run(endpoint, command); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "queued-input-reached-shell")
	paneWorking, tabWorking, visible, queued, focused := false, false, false, false, false
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		pane, paneErr := client.GetPane(endpoint)
		if terminal.IsNotFound(paneErr) {
			paneOwned = false
			break
		}
		if paneErr != nil {
			t.Fatal(paneErr)
		}
		paneWorking = paneWorking || pane.AgentStatus == "working"
		focused = focused || pane.Focused
		tab, tabErr := client.GetTab(endpoint)
		if tabErr == nil {
			tabWorking = tabWorking || tab.AgentStatus == "working"
			focused = focused || tab.Focused
		}
		output, readErr := client.ReadPane(endpoint, 200)
		if readErr == nil && strings.Contains(output, "HERDR_INTERACTIVE_PI_E2E") && !strings.Contains(output, `{"type":"session"`) {
			visible = true
		}
		if !queued && visible && paneWorking && tabWorking {
			if _, stderr, sendErr := (terminal.ExecRunner{}).Run(endpoint.SocketPath, "pane", "send-text", endpoint.PaneID, "touch "+marker); sendErr != nil {
				t.Fatalf("queue input: %s: %v", stderr, sendErr)
			}
			queued = true
		}
		time.Sleep(100 * time.Millisecond)
	}
	if paneOwned {
		t.Fatal("real interactive Pi pane did not disappear")
	}
	if !paneWorking || !tabWorking || !visible || !queued || focused {
		t.Fatalf("paneWorking=%t tabWorking=%t visible=%t queued=%t focused=%t", paneWorking, tabWorking, visible, queued, focused)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("queued input reached a shell: %v", err)
	}
	current, _ := state.Task(task.ID)
	messages, _ := state.Messages(task.ID)
	checkpointSeen, doneSeen := false, false
	for _, message := range messages {
		checkpointSeen = checkpointSeen || message.Type == "checkpoint" && message.Direction == "worker-to-driver"
		doneSeen = doneSeen || message.Type == "done" && !message.Stale
	}
	if current.Status != "done" || !current.ClaimedDone || !current.Landed || !checkpointSeen || !doneSeen {
		t.Fatalf("task=%+v checkpoint=%t done=%t messages=%+v", current, checkpointSeen, doneSeen, messages)
	}
}

func TestRealInteractivePiRepairHerdrIsOptIn(t *testing.T) {
	if os.Getenv("SHEPHRD_REAL_HERDR_PI_E2E") != "1" {
		t.Skip("set SHEPHRD_REAL_HERDR_PI_E2E=1 to run a real interactive Pi repair in Herdr")
	}
	client := terminal.New(os.Getenv("HERDR_SOCKET_PATH"))
	parent, err := client.ValidateParent()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	databasePath := filepath.Join(root, "state.db")
	configPath := filepath.Join(root, "config.toml")
	binary := filepath.Join(root, "shephrd")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build E2E Shephrd: %s: %v", output, err)
	}
	configBody := fmt.Sprintf(`default_harness = "pi"
worker_runtime = "herdr"
database_path = %q
data_dir = %q

[wake]
enabled = true
default_batch = 10
max_batch = 20
claim_ttl = "5m"
claim_ttl_min = "30s"
claim_ttl_max = "30m"
driver_id = "driver:e2e"

[notifications]
enabled = false
details = false
task_per_minute = 2
global_per_minute = 10

[pi_watcher]
enabled = false
poll_min = "1s"
poll_max = "15s"
`, databasePath, dataDir)
	configBody += liveHerdrExtensionConfig(t, filepath.Clean(filepath.Join(workingDirectory, "..", "..")), root)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "herdr-repair-e2e", Path: root, DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:e2e", RepoID: repo.ID, FeatureKey: "interactive-pi-repair", Objective: "repair one malformed question", Deliverable: "code"})
	modelName := os.Getenv("SHEPHRD_REAL_HERDR_PI_MODEL")
	if modelName == "" {
		modelName = "openai-codex/gpt-5.6-luna"
	}
	attempt, _ := state.BeginAttempt(task.ID, "pi", modelName)
	if err := state.SetRuntimeBackend(attempt.ID, "herdr"); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, adapter.NewSessionID("pi"), root, "e2e-repair-lease", "e2e-repair-branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"emit the repair fixture"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	checkpoint := `<shephrd-event>{"type":"checkpoint","payload":"Repair E2E checkpoint","checkpoint":{"schema_version":1,"summary":"Repair E2E checkpoint","completed":["No project work required"],"next_steps":["Ask the fixture question"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
	malformed := `<shephrd-event>{"type":"question","payload":"Choose A or B","question":{"prompt":"Choose A or B","options":["A","B"]}}</shephrd-event>`
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "HERDR_INTERACTIVE_PI_REPAIR_E2E. Do not use tools or modify files. In the first response emit these two exact envelopes without correcting or changing them: "+checkpoint+" then "+malformed+". If Shephrd sends SHEPHRD_PROTOCOL_REPAIR, follow it and emit a current checkpoint followed by a corrected payload-only question envelope with options A and B and a recommendation in one assistant message.")
	endpoint, err := client.CreateTab(terminal.TabSpec{WorkspaceID: parent.WorkspaceID, CWD: root, Label: "shephrd-real-pi-repair-e2e", Harness: "pi", Source: "shephrd:e2e:repair", Environment: []string{"SHEPHRD_CONFIG=" + configPath, "SHEPHRD_PI_WATCHER_ENABLED=0", "SHEPHRD_HERDR_LIFECYCLE_SEQ=1", "PI_SKIP_VERSION_CHECK=1"}})
	if err != nil {
		t.Fatal(err)
	}
	paneOwned := true
	defer func() {
		if paneOwned {
			_ = client.CloseExactPane(endpoint)
		}
	}()
	if _, err := state.SetTerminalEndpointForRun(attempt.ID, generation, model.TerminalEndpoint{Backend: "herdr", SocketPath: endpoint.SocketPath, WorkspaceID: endpoint.WorkspaceID, TabID: endpoint.TabID, PaneID: endpoint.PaneID}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportAgent(endpoint, terminal.AgentReport{Source: terminalLifecycleSource(attempt.ID, generation), Agent: "pi", State: "working", Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	command := "exec " + shellQuote(binary) + " _run " + shellQuote(attempt.ID) + " --input " + shellQuote(inputPath) + " --run-generation " + fmt.Sprint(generation)
	if err := client.Run(endpoint, command); err != nil {
		t.Fatal(err)
	}
	focused := false
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		pane, paneErr := client.GetPane(endpoint)
		if terminal.IsNotFound(paneErr) {
			paneOwned = false
			break
		}
		if paneErr != nil {
			t.Fatal(paneErr)
		}
		focused = focused || pane.Focused
		time.Sleep(100 * time.Millisecond)
	}
	if paneOwned || focused {
		t.Fatalf("paneOwned=%t focused=%t", paneOwned, focused)
	}
	current, _ := state.Task(task.ID)
	stored, _ := state.Attempt(attempt.ID)
	messages, _ := state.Messages(task.ID)
	notifications, _ := state.Notifications(task.ID)
	repairs, questions, blocked := 0, 0, 0
	for _, message := range messages {
		repairs += boolInt(message.Type == "protocol-repair" && message.SourceCursor == 3 && !message.Wake)
		questions += boolInt(message.Type == "question" && message.SourceCursor == 4 && !message.Stale)
		blocked += boolInt(message.Type == "blocked" && !message.Stale)
	}
	if current.Status != model.TaskStatusWaiting || stored.Status != model.AttemptStatusWaiting || stored.RunGeneration != generation || stored.Cursor != 4 || repairs != 1 || questions != 1 || blocked != 0 || len(notifications) != 1 || notifications[0].Kind != "question" {
		t.Fatalf("task=%+v attempt=%+v repairs=%d questions=%d blocked=%d notifications=%+v messages=%+v", current, stored, repairs, questions, blocked, notifications, messages)
	}
}

func TestPiBridgeHarnessProcess(t *testing.T) {
	if os.Getenv("SHEPHRD_TEST_BRIDGE_HELPER") != "1" {
		return
	}
	if os.Getenv("SHEPHRD_PI_WATCHER_ENABLED") != "0" {
		os.Exit(4)
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
	mode := os.Getenv("SHEPHRD_TEST_BRIDGE_MODE")
	if mode == "missing" {
		os.Exit(0)
	}
	conn, err := net.Dial("unix", os.Getenv("SHEPHRD_BRIDGE_SOCKET"))
	if err != nil {
		os.Exit(2)
	}
	defer conn.Close()
	generation := os.Getenv("SHEPHRD_BRIDGE_RUN_GENERATION")
	if mode == "stale" {
		generation = "999"
	}
	sessionID := argumentValue(args, "--session-id")
	if sessionID == "" {
		sessionID = argumentValue(args, "--session")
	}
	writeHelperFrame(conn, map[string]any{"schema_version": pibridge.SchemaVersion, "token": os.Getenv("SHEPHRD_BRIDGE_TOKEN"), "attempt_id": os.Getenv("SHEPHRD_BRIDGE_ATTEMPT_ID"), "run_generation": json.Number(generation), "seq": 1, "kind": "session", "session_id": sessionID})
	if mode == "stale" || mode == "early" {
		os.Exit(0)
	}
	readHelperResponse(conn)
	if mode == "settled" {
		writeHelperFrame(conn, map[string]any{"schema_version": pibridge.SchemaVersion, "token": os.Getenv("SHEPHRD_BRIDGE_TOKEN"), "attempt_id": os.Getenv("SHEPHRD_BRIDGE_ATTEMPT_ID"), "run_generation": json.Number(generation), "seq": 2, "kind": "settled"})
		readHelperResponse(conn)
		os.Exit(0)
	}
	if mode == "oversized" {
		conn.Write([]byte(strings.Repeat("x", pibridge.MaxFrameBytes) + "\n"))
		os.Exit(0)
	}
	envelope := checkpointEnvelope()
	if mode == "malformed" {
		envelope = `<shephrd-event>{"type":"done","payload":"complete","artifact":"report:x","extra":true}</shephrd-event>`
		writeHelperFrame(conn, helperEventFrame(generation, 2, envelope))
		readHelperResponse(conn)
		os.Exit(0)
	}
	writeHelperFrame(conn, helperEventFrame(generation, 2, envelope))
	readHelperResponse(conn)
	if mode == "report-handoff-failure" {
		report := os.Getenv("SHEPHRD_TEST_REPORT")
		os.MkdirAll(filepath.Dir(report), 0o700)
		os.WriteFile(report, []byte("complete report before failed handoff\n"), 0o600)
		malformed := `<shephrd-event>{"type":"done","payload":"complete","artifact":"report:` + report + `","extra":true}</shephrd-event>`
		writeHelperFrame(conn, helperEventFrame(generation, 3, malformed))
		response := readHelperResponse(conn)
		if response["result"] != pibridge.ResultRepair {
			os.Exit(9)
		}
		os.Exit(0)
	}
	if strings.HasPrefix(mode, "repair-") {
		malformed := `<shephrd-event>{"type":"question","payload":"Which option should I use?","question":{"prompt":"Choose A or B","options":["A","B"]}}</shephrd-event>`
		code := adapter.DiagnosticSchemaUnknownField
		if mode != "repair-question" && mode != "repair-timeout" {
			field := "decisions"
			if mode == "repair-checks" {
				field = "checks"
			}
			malformed = strings.Replace(checkpointEnvelope(), `"`+field+`":[]`, `"`+field+`":["wrong string"]`, 1)
			code = adapter.DiagnosticSchemaTypeMismatch
		}
		writeHelperFrame(conn, helperEventFrame(generation, 3, malformed))
		response := readHelperResponse(conn)
		diagnostic, _ := response["diagnostic"].(map[string]any)
		if response["result"] != pibridge.ResultRepair || diagnostic["code"] != code {
			os.Exit(7)
		}
		if mode == "repair-timeout" {
			for {
				time.Sleep(time.Second)
			}
		}
		corrected := `<shephrd-event>{"type":"question","payload":"Choose A or B. Options: A or B. Recommendation: A."}</shephrd-event>`
		correction := checkpointEnvelope() + corrected
		if mode == "repair-exhausted" {
			correction = checkpointEnvelope() + malformed
		}
		if mode == "repair-duplicate" {
			correction += corrected
		}
		writeHelperFrame(conn, helperEventFrame(generation, 4, correction))
		response = readHelperResponse(conn)
		if mode == "repair-exhausted" || mode == "repair-duplicate" {
			os.Exit(0)
		}
		if response["result"] != pibridge.ResultAcceptedTerminal {
			os.Exit(8)
		}
		os.Exit(0)
	}
	if mode == "question-hold" {
		for {
			if _, err := os.Stat(os.Getenv("SHEPHRD_TEST_RELEASE")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		writeHelperFrame(conn, helperEventFrame(generation, 3, `<shephrd-event>{"type":"question","payload":"choose"}</shephrd-event>`))
		readHelperResponse(conn)
		os.Exit(0)
	}
	if mode == "done" || strings.HasPrefix(mode, "done-") {
		report := os.Getenv("SHEPHRD_TEST_REPORT")
		os.MkdirAll(filepath.Dir(report), 0o700)
		os.WriteFile(report, []byte("report\n"), 0o600)
		writeHelperFrame(conn, helperEventFrame(generation, 3, `<shephrd-event>{"type":"done","payload":"complete","artifact":"report:`+report+`"}</shephrd-event>`))
		readHelperResponse(conn)
		switch mode {
		case "done-hold":
			for {
				time.Sleep(time.Second)
			}
		case "done-invalid":
			writeHelperFrame(conn, map[string]any{"schema_version": pibridge.SchemaVersion, "token": os.Getenv("SHEPHRD_BRIDGE_TOKEN"), "attempt_id": os.Getenv("SHEPHRD_BRIDGE_ATTEMPT_ID"), "run_generation": json.Number(generation), "seq": 4, "kind": "invalid"})
		case "done-oversized":
			conn.Write([]byte(strings.Repeat("x", pibridge.MaxFrameBytes) + "\n"))
		case "done-duplicate":
			writeHelperFrame(conn, helperEventFrame(generation, 4, `<shephrd-event>{"type":"done","payload":"replacement","artifact":"report:/tmp/replacement"}</shephrd-event>`))
		}
		os.Exit(0)
	}
	os.Exit(3)
}

func interactivePiFixture(t *testing.T, root, dataDir, mode, argsPath, releasePath string) (Service, *store.Store, model.Task, model.Attempt) {
	t.Helper()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(binDir, "pi")
	body := "#!/bin/sh\nexec \"$SHEPHRD_TEST_BINARY\" -test.run '^TestPiBridgeHarnessProcess$' -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "interactive", Objective: "show the real TUI", Deliverable: "report"})
	attempt, _ := state.BeginAttempt(task.ID, "pi", "model")
	if err := state.SetRuntimeBackend(attempt.ID, "herdr"); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "native-session", root, "lease", "branch"); err != nil {
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
	t.Setenv("SHEPHRD_TEST_BRIDGE_HELPER", "1")
	t.Setenv("SHEPHRD_TEST_BRIDGE_MODE", mode)
	t.Setenv("SHEPHRD_TEST_ARGS_PATH", argsPath)
	t.Setenv("SHEPHRD_TEST_RELEASE", releasePath)
	t.Setenv("SHEPHRD_PI_WATCHER_ENABLED", "1")
	t.Setenv("SHEPHRD_HERDR_LIFECYCLE_SEQ", "")
	return service, state, task, attempt
}

func helperEventFrame(generation string, sequence int, envelope string) map[string]any {
	return map[string]any{"schema_version": pibridge.SchemaVersion, "token": os.Getenv("SHEPHRD_BRIDGE_TOKEN"), "attempt_id": os.Getenv("SHEPHRD_BRIDGE_ATTEMPT_ID"), "run_generation": json.Number(generation), "seq": sequence, "kind": "event_candidate", "envelope": envelope}
}

func writeHelperFrame(conn net.Conn, value map[string]any) {
	encoded, _ := json.Marshal(value)
	conn.Write(append(encoded, '\n'))
}

func readHelperResponse(conn net.Conn) map[string]any {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		os.Exit(5)
	}
	var response map[string]any
	if json.Unmarshal(line, &response) != nil {
		os.Exit(6)
	}
	return response
}

func checkpointEnvelope() string {
	return `<shephrd-event>{"type":"checkpoint","payload":"bridge checkpoint","checkpoint":{"schema_version":1,"summary":"bridge checkpoint","completed":["connected"],"next_steps":["finish"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
}

func argumentValue(args []string, name string) string {
	for index, value := range args {
		if value == name && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func writeInput(t *testing.T, dataDir, taskID, name, text string) string {
	t.Helper()
	path := filepath.Join(dataDir, taskID, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readArgv(t *testing.T, path string) [][]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var result [][]string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var args []string
		if err := json.Unmarshal(scanner.Bytes(), &args); err != nil {
			t.Fatal(err)
		}
		result = append(result, args)
	}
	return result
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied")
}
