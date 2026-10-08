package claudebridge

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const checkpointEnvelope = `<shephrd-event>{"type":"checkpoint","payload":"progress","checkpoint":{"schema_version":1,"summary":"progress","completed":[],"next_steps":["finish"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
const doneEnvelope = `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:shephrd/task"}</shephrd-event>`

func TestHookChannelAuthenticatesExactRunAndReturnsTermination(t *testing.T) {
	channel, err := Open("attempt_exact", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	info, err := os.Stat(channel.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode=%v", info.Mode().Perm())
	}
	for key, value := range channel.Environment() {
		t.Setenv(key, value)
	}
	accepted := make(chan error, 1)
	go func() {
		exchange, err := channel.Accept()
		if err == nil {
			input, parseErr := ParseHookInput(exchange.Input)
			if parseErr != nil || input.SessionID != "native-session" || input.HookEventName != "Stop" {
				err = parseErr
			}
			if respondErr := exchange.Respond(true, "accepted"); err == nil {
				err = respondErr
			}
		}
		accepted <- err
	}()
	var output bytes.Buffer
	input := strings.NewReader(`{"session_id":"native-session","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"done"}`)
	if err := RunHook(input, &output); err != nil {
		t.Fatal(err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response["continue"] != false || response["stopReason"] != "accepted" {
		t.Fatalf("hook response=%q parsed=%v err=%v", output.String(), response, err)
	}
}

func TestChannelRejectsStaleSpoofedAndMalformedRequests(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*request)
		reason string
	}{
		{name: "stale", mutate: func(value *request) { value.RunGeneration++ }, reason: "stale"},
		{name: "spoofed", mutate: func(value *request) { value.Token = "wrong" }, reason: "token"},
		{name: "missing payload", mutate: func(value *request) { value.Input = nil }, reason: "payload"},
	} {
		t.Run(test.name, func(t *testing.T) {
			channel, err := Open("attempt_exact", 2)
			if err != nil {
				t.Fatal(err)
			}
			defer channel.Close()
			value := request{SchemaVersion: 1, Token: channel.Token, AttemptID: channel.AttemptID, RunGeneration: channel.RunGeneration, Input: json.RawMessage(`{"session_id":"session","hook_event_name":"Stop"}`)}
			test.mutate(&value)
			go writeRequest(t, channel.SocketPath, value)
			if _, err := channel.Accept(); err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestTrackerExtractsOnlyOrderedBoundedEnvelopes(t *testing.T) {
	tracker := NewTracker("native-session", false)
	outcome := tracker.Handle(HookInput{SessionID: "native-session", HookEventName: "SessionStart", Source: "startup"})
	if !outcome.Acknowledged {
		t.Fatalf("session outcome=%+v", outcome)
	}
	secret := "ordinary assistant text, tool arguments, and paths"
	first := tracker.Handle(HookInput{SessionID: "native-session", HookEventName: "MessageDisplay", MessageID: "message-1", Index: 0, Delta: secret + "\n" + checkpointEnvelope[:40]})
	second := tracker.Handle(HookInput{SessionID: "native-session", HookEventName: "MessageDisplay", MessageID: "message-1", Index: 1, Final: true, Delta: checkpointEnvelope[40:] + "\n" + doneEnvelope})
	if first.Failure != "" || len(first.Envelopes) != 0 || strings.Contains(strings.Join(second.Envelopes, ""), secret) {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if strings.Join(second.Envelopes, "\n") != checkpointEnvelope+"\n"+doneEnvelope {
		t.Fatalf("envelopes=%q", second.Envelopes)
	}
	settled := tracker.Handle(HookInput{SessionID: "native-session", HookEventName: "Stop"})
	if !settled.Settled || settled.Failure != "" {
		t.Fatalf("stop=%+v", settled)
	}
}

func TestTrackerBlocksStopWhileBackgroundTasksAreActive(t *testing.T) {
	tracker := NewTracker("native-session", false)
	tracker.Handle(HookInput{SessionID: "native-session", HookEventName: "SessionStart", Source: "startup"})
	outcome := tracker.Handle(HookInput{SessionID: "native-session", HookEventName: "Stop", BackgroundTasks: []BackgroundTask{{ID: "task-1", Type: "subagent", Status: "running"}}})
	if outcome.Failure != "" || !outcome.BackgroundActive || outcome.Settled || len(outcome.BackgroundTasks) != 1 {
		t.Fatalf("active stop=%+v", outcome)
	}
	settled := tracker.Handle(HookInput{SessionID: "native-session", HookEventName: "Stop"})
	if settled.Failure != "" || !settled.Settled || settled.BackgroundActive {
		t.Fatalf("settled stop=%+v", settled)
	}
}

func TestParseHookInputValidatesBackgroundTaskSnapshot(t *testing.T) {
	raw := json.RawMessage(`{"session_id":"native-session","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"done","background_tasks":[{"id":"build","type":"shell","status":"running","description":"build"}]}`)
	input, err := ParseHookInput(raw)
	if err != nil || len(input.BackgroundTasks) != 1 || input.BackgroundTasks[0].ID != "build" {
		t.Fatalf("input=%+v err=%v", input, err)
	}
	for _, body := range []string{
		`{"session_id":"native-session","hook_event_name":"Stop","last_assistant_message":"done","background_tasks":{}}`,
		`{"session_id":"native-session","hook_event_name":"Stop","last_assistant_message":"done","background_tasks":[{"id":"build","type":"shell"}]}`,
	} {
		if _, err := ParseHookInput(json.RawMessage(body)); err == nil {
			t.Fatalf("malformed snapshot accepted: %s", body)
		}
	}
}

func TestHookRespondsWithStopBlockForBackgroundWork(t *testing.T) {
	channel, err := Open("attempt_background", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	for key, value := range channel.Environment() {
		t.Setenv(key, value)
	}
	accepted := make(chan error, 1)
	go func() {
		exchange, err := channel.Accept()
		if err == nil {
			if input, parseErr := ParseHookInput(exchange.Input); parseErr != nil || len(input.BackgroundTasks) != 1 {
				err = parseErr
			}
			if respondErr := exchange.RespondBlock("background work is still active"); err == nil {
				err = respondErr
			}
		}
		accepted <- err
	}()
	var output bytes.Buffer
	input := strings.NewReader(`{"session_id":"native-session","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"done","background_tasks":[{"id":"task-1","type":"monitor","status":"running","description":"watch"}]}`)
	if err := RunHook(input, &output); err != nil {
		t.Fatal(err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response["decision"] != "block" || response["reason"] != "background work is still active" || response["continue"] != nil {
		t.Fatalf("hook response=%q parsed=%v err=%v", output.String(), response, err)
	}
}

func TestTrackerReconcilesStopBeforeFinalMessageDisplay(t *testing.T) {
	tracker := NewTracker("native-session", false)
	if outcome := tracker.Handle(HookInput{SessionID: "native-session", HookEventName: "SessionStart", Source: "startup"}); !outcome.Acknowledged {
		t.Fatalf("session outcome=%+v", outcome)
	}
	streamed := tracker.Handle(HookInput{SessionID: "native-session", HookEventName: "MessageDisplay", MessageID: "message-1", Index: 0, Delta: checkpointEnvelope})
	if streamed.Failure != "" || strings.Join(streamed.Envelopes, "") != checkpointEnvelope {
		t.Fatalf("streamed=%+v", streamed)
	}
	raw := json.RawMessage(`{"session_id":"native-session","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":` + strconv.Quote(checkpointEnvelope+"\n"+doneEnvelope) + `}`)
	input, err := ParseHookInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if input.LastAssistantMessage != checkpointEnvelope+"\n"+doneEnvelope {
		t.Fatalf("last assistant message=%q", input.LastAssistantMessage)
	}
	stopped := tracker.Handle(input)
	if stopped.Failure != "" || !stopped.Settled || !stopped.Reconciled || strings.Join(stopped.Envelopes, "") != doneEnvelope {
		t.Fatalf("stopped=%+v", stopped)
	}
}

func TestTrackerReconcilesCheckpointOnlyStop(t *testing.T) {
	tracker := NewTracker("session", false)
	tracker.Handle(HookInput{SessionID: "session", HookEventName: "SessionStart", Source: "startup"})
	streamed := tracker.Handle(HookInput{SessionID: "session", HookEventName: "MessageDisplay", MessageID: "message", Index: 0, Delta: checkpointEnvelope})
	if streamed.Failure != "" || len(streamed.Envelopes) != 1 {
		t.Fatalf("streamed=%+v", streamed)
	}
	stopped := tracker.Handle(HookInput{SessionID: "session", HookEventName: "Stop", LastAssistantMessage: checkpointEnvelope})
	if stopped.Failure != "" || !stopped.Settled || !stopped.Reconciled || len(stopped.Envelopes) != 0 {
		t.Fatalf("stopped=%+v", stopped)
	}
}

func TestTrackerRejectsInconsistentStopWithoutClearingMessage(t *testing.T) {
	tracker := NewTracker("session", false)
	tracker.Handle(HookInput{SessionID: "session", HookEventName: "SessionStart", Source: "startup"})
	tracker.Handle(HookInput{SessionID: "session", HookEventName: "MessageDisplay", MessageID: "message", Index: 0, Delta: checkpointEnvelope})
	conflicting := strings.ReplaceAll(checkpointEnvelope, "progress", "different")
	stopped := tracker.Handle(HookInput{SessionID: "session", HookEventName: "Stop", LastAssistantMessage: conflicting + doneEnvelope})
	if !strings.Contains(stopped.Failure, "Stop reconciliation conflicts") || stopped.Settled || len(stopped.Envelopes) != 0 {
		t.Fatalf("stopped=%+v", stopped)
	}
	continued := tracker.Handle(HookInput{SessionID: "session", HookEventName: "MessageDisplay", MessageID: "message", Index: 1, Final: true, Delta: doneEnvelope})
	if continued.Failure != "" || strings.Join(continued.Envelopes, "") != doneEnvelope {
		t.Fatalf("continued=%+v", continued)
	}
}

func TestTrackerRejectsInvalidStopReconciliationFraming(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		reason  string
	}{
		{name: "malformed", message: openTag + `{}`, reason: "malformed"},
		{name: "oversized", message: openTag + strings.Repeat("x", MaxFrameBytes), reason: "oversized"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := NewTracker("session", false)
			tracker.Handle(HookInput{SessionID: "session", HookEventName: "SessionStart", Source: "startup"})
			tracker.Handle(HookInput{SessionID: "session", HookEventName: "MessageDisplay", MessageID: "message", Index: 0, Delta: "working"})
			stopped := tracker.Handle(HookInput{SessionID: "session", HookEventName: "Stop", LastAssistantMessage: test.message})
			if !strings.Contains(stopped.Failure, test.reason) || stopped.Settled || len(stopped.Envelopes) != 0 {
				t.Fatalf("stopped=%+v", stopped)
			}
		})
	}
}

func TestTrackerFailsClosedOnSessionMessageAndFramingViolations(t *testing.T) {
	for _, test := range []struct {
		name  string
		steps []HookInput
	}{
		{name: "wrong session", steps: []HookInput{{SessionID: "wrong", HookEventName: "SessionStart", Source: "startup"}}},
		{name: "wrong resume source", steps: []HookInput{{SessionID: "session", HookEventName: "SessionStart", Source: "startup"}}},
		{name: "missing message batch", steps: []HookInput{{SessionID: "session", HookEventName: "SessionStart", Source: "resume"}, {SessionID: "session", HookEventName: "MessageDisplay", MessageID: "m", Index: 1, Final: true}}},
		{name: "malformed envelope", steps: []HookInput{{SessionID: "session", HookEventName: "SessionStart", Source: "resume"}, {SessionID: "session", HookEventName: "MessageDisplay", MessageID: "m", Index: 0, Final: true, Delta: "<shephrd-event>{}"}}},
		{name: "oversized envelope", steps: []HookInput{{SessionID: "session", HookEventName: "SessionStart", Source: "resume"}, {SessionID: "session", HookEventName: "MessageDisplay", MessageID: "m", Index: 0, Final: true, Delta: openTag + strings.Repeat("x", MaxFrameBytes)}}},
		{name: "session switch", steps: []HookInput{{SessionID: "session", HookEventName: "SessionStart", Source: "resume"}, {SessionID: "session", HookEventName: "SessionStart", Source: "clear"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := NewTracker("session", true)
			var outcome Outcome
			for _, step := range test.steps {
				outcome = tracker.Handle(step)
			}
			if outcome.Failure == "" {
				t.Fatalf("outcome=%+v", outcome)
			}
		})
	}
}

func TestWriteSettingsCreatesPrivateSessionOnlyHooks(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "shephrd")
	if err := os.WriteFile(executable, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := WriteSettings(root, "attempt", 4, executable)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode=%v", info.Mode().Perm())
	}
	var settings hookSettings
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if !settings.SkipDangerousModePermissionPrompt {
		t.Fatal("private settings did not skip the bypass confirmation prompt")
	}
	for _, event := range []string{"SessionStart", "MessageDisplay", "Stop", "StopFailure", "SessionEnd"} {
		groups := settings.Hooks[event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s hooks=%+v", event, groups)
		}
		command := groups[0].Hooks[0]
		if command.Command != executable || strings.Join(command.Args, " ") != "_claude-hook" || command.Type != "command" {
			t.Fatalf("%s command=%+v", event, command)
		}
	}
}

func writeRequest(t *testing.T, path string, value request) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var conn net.Conn
	var err error
	for time.Now().Before(deadline) {
		conn, err = net.Dial("unix", path)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(value); err != nil {
		t.Error(err)
	}
}
