package adapter

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func installHarnessFixtures(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"pi", "claude", "codex"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRecordedContractFixtures(t *testing.T) {
	for _, harness := range []string{"claude-code", "pi", "codex"} {
		t.Run(harness, func(t *testing.T) {
			file, err := os.Open(filepath.Join("testdata", harness+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			acknowledged, terminal := 0, 0
			events := make([]string, 0)
			sessionID := ""
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				parsed := ParseLine(harness, scanner.Bytes())
				if parsed.Acknowledged {
					acknowledged++
				}
				if parsed.Terminal {
					terminal++
				}
				if parsed.SessionID != "" {
					if sessionID != "" && parsed.SessionID != sessionID {
						t.Fatalf("session changed from %s to %s", sessionID, parsed.SessionID)
					}
					sessionID = parsed.SessionID
				}
				if parsed.Text != "" {
					event, found, err := ParseEvent(parsed.Text)
					if err != nil {
						t.Fatal(err)
					}
					if found {
						events = append(events, event.Type)
					}
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if acknowledged < 2 || terminal < 2 || sessionID == "" || strings.Join(events, ",") != "question,done" {
				t.Fatalf("ack=%d terminal=%d session=%q events=%v", acknowledged, terminal, sessionID, events)
			}
		})
	}
}

func TestRecordedCompoundContractFixtures(t *testing.T) {
	for _, harness := range []string{"claude-code", "pi", "codex"} {
		t.Run(harness, func(t *testing.T) {
			file, err := os.Open(filepath.Join("testdata", harness+"-compound.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			scanner := bufio.NewScanner(file)
			var events []model.Event
			for scanner.Scan() {
				parsed := ParseLine(harness, scanner.Bytes())
				if parsed.Text == "" {
					continue
				}
				parsedEvents, found, err := ParseEvents(parsed.Text)
				if err != nil || !found {
					t.Fatalf("events=%+v found=%t err=%v", parsedEvents, found, err)
				}
				events = append(events, parsedEvents...)
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[0].Type != "checkpoint" || events[1].Type != "done" {
				t.Fatalf("events=%+v", events)
			}
		})
	}
}

func TestRecordedHarnessEvents(t *testing.T) {
	tests := []struct {
		name      string
		harness   string
		line      string
		ack       bool
		sessionID string
		text      string
		terminal  bool
	}{
		{"claude init", "claude-code", `{"type":"system","subtype":"init","session_id":"claude-session"}`, true, "claude-session", "", false},
		{"claude assistant", "claude-code", `{"type":"assistant","session_id":"claude-session","parent_tool_use_id":null,"message":{"content":[{"type":"text","text":"result"}]}}`, false, "claude-session", "result", false},
		{"claude result", "claude-code", `{"type":"result","subtype":"success","session_id":"claude-session","is_error":false}`, false, "claude-session", "", true},
		{"pi session", "pi", `{"type":"session","version":3,"id":"pi-session"}`, true, "pi-session", "", false},
		{"pi message", "pi", `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"result"}],"stopReason":"stop"}}`, false, "", "result", false},
		{"pi end", "pi", `{"type":"agent_end","willRetry":false}`, false, "", "", false},
		{"pi retrying", "pi", `{"type":"agent_end","willRetry":true}`, false, "", "", false},
		{"pi settled", "pi", `{"type":"agent_settled"}`, false, "", "", true},
		{"codex start", "codex", `{"type":"thread.started","thread_id":"codex-session"}`, true, "codex-session", "", false},
		{"codex message", "codex", `{"type":"item.completed","item":{"type":"agent_message","text":"result"}}`, false, "", "result", false},
		{"codex end", "codex", `{"type":"turn.completed"}`, false, "", "", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed := ParseLine(test.harness, []byte(test.line))
			if parsed.Acknowledged != test.ack || parsed.SessionID != test.sessionID || parsed.Text != test.text || parsed.Terminal != test.terminal {
				t.Fatalf("parsed = %+v", parsed)
			}
		})
	}
}

func TestPiAssistantCompletionAuthority(t *testing.T) {
	for _, reason := range []string{"stop", "length", "toolUse", "deferred", "error", "aborted", "pending", "", "unknown"} {
		for _, text := range []string{"", `<shephrd-event>{"type":"checkpoint"`, `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:exact"}</shephrd-event>`} {
			line, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": reason, "content": []any{map[string]any{"type": "thinking", "thinking": "private"}, map[string]any{"type": "text", "text": text}, map[string]any{"type": "toolCall", "id": "once", "name": "counter", "arguments": map[string]any{}}}}})
			if err != nil {
				t.Fatal(err)
			}
			parsed := ParseLine("pi", line)
			success := slices.Contains([]string{"stop", "length", "toolUse", "deferred"}, reason)
			if success && (parsed.EventAuthority != EventAuthorityAssignedWorker || parsed.Text != text || parsed.Failure != "") {
				t.Fatalf("successful %q: %+v", reason, parsed)
			}
			if !success && (parsed.EventAuthority != EventAuthorityNone || parsed.Text != "" || parsed.Failure == "") {
				t.Fatalf("unsuccessful %q exposed text: %+v", reason, parsed)
			}
		}
	}
}

func TestNamedSessionsUseDisplayNameOnlyOnInitialLaunch(t *testing.T) {
	installHarnessFixtures(t)
	attempt := model.Attempt{SessionID: "session", WorktreePath: "/tree"}
	initial, err := BuildNamed("pi", attempt, "prompt", false, "Shephrd: demo/feature [short]")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(initial.Args, " ")
	if !strings.Contains(joined, "--name Shephrd: demo/feature [short]") {
		t.Fatalf("initial args = %v", initial.Args)
	}
	resumed, err := BuildNamed("pi", attempt, "prompt", true, "Shephrd: demo/feature [short]")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(resumed.Args, " "), "--name") {
		t.Fatalf("resume args = %v", resumed.Args)
	}
}

func TestPiInvocationDisablesDriverWatcher(t *testing.T) {
	installHarnessFixtures(t)
	invocation, err := BuildNamed("pi", model.Attempt{SessionID: "session", WorktreePath: "/tree"}, "prompt", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Environment["SHEPHRD_PI_WATCHER_ENABLED"] != "0" {
		t.Fatalf("environment = %+v", invocation.Environment)
	}
}

func TestPiTerminalRuntimesUseInteractiveTUIWithExplicitBridge(t *testing.T) {
	installHarnessFixtures(t)
	attempt := model.Attempt{Model: "openai-codex/gpt-5.6-luna", SessionID: "session", WorktreePath: "/tree"}
	for _, workerRuntime := range []string{"herdr", "cmux"} {
		for _, resume := range []bool{false, true} {
			invocation, err := BuildNamedForRuntime("pi", attempt, "prompt", resume, "Shephrd: demo/feature [short]", workerRuntime, "/private/bridge.ts")
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(invocation.Args, "\x00")
			for _, forbidden := range []string{"--mode", "json", "--print"} {
				if slices.Contains(invocation.Args, forbidden) {
					t.Fatalf("resume=%t interactive args contain %q: %q", resume, forbidden, invocation.Args)
				}
			}
			if !strings.Contains(joined, "--approve\x00--extension\x00/private/bridge.ts") || !strings.Contains(joined, "--model\x00"+attempt.Model) {
				t.Fatalf("resume=%t args = %q", resume, invocation.Args)
			}
			if resume && !strings.Contains(joined, "--session\x00session") {
				t.Fatalf("resume args = %q", invocation.Args)
			}
			if !resume && !strings.Contains(joined, "--session-id\x00session") {
				t.Fatalf("initial args = %q", invocation.Args)
			}
		}
		if _, err := BuildNamedForRuntime("pi", attempt, "prompt", false, "name", workerRuntime, ""); err == nil {
			t.Fatal("interactive Pi accepted a missing bridge extension")
		}
	}
}

func TestHeadlessAndCodexHerdrInvocationsRemainMachineReadable(t *testing.T) {
	installHarnessFixtures(t)
	attempt := model.Attempt{SessionID: "session", WorktreePath: "/tree"}
	for _, harness := range []string{"pi", "claude-code"} {
		headless, err := BuildNamedForRuntime(harness, attempt, "prompt", false, "name", "headless", "")
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(headless.Args, "\x00")
		if harness == "pi" && (!slices.Contains(headless.Args, "--mode") || !slices.Contains(headless.Args, "--print")) {
			t.Fatalf("headless Pi args = %q", headless.Args)
		}
		if harness == "claude-code" && !strings.Contains(joined, "-p\x00--output-format\x00stream-json") {
			t.Fatalf("headless Claude Code args = %q", headless.Args)
		}
	}
	fromHeadless, err := BuildNamedForRuntime("codex", attempt, "prompt", false, "name", "headless", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, workerRuntime := range []string{"herdr", "cmux"} {
		fromTerminal, err := BuildNamedForRuntime("codex", attempt, "prompt", false, "name", workerRuntime, "/unused")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(fromHeadless.Args, "\x00") != strings.Join(fromTerminal.Args, "\x00") {
			t.Fatalf("Codex %s changed machine invocation: %q != %q", workerRuntime, fromHeadless.Args, fromTerminal.Args)
		}
	}
}

func TestClaudeTerminalRuntimesUseInteractiveTUIWithBridgeSettings(t *testing.T) {
	installHarnessFixtures(t)
	attempt := model.Attempt{Model: "claude-fable-5", SessionID: "session", WorktreePath: "/tree"}
	for _, workerRuntime := range []string{"herdr", "cmux"} {
		for _, resume := range []bool{false, true} {
			invocation, err := BuildNamedForRuntime("claude-code", attempt, "prompt", resume, "name", workerRuntime, "/private/settings.json")
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"-p", "--print", "--output-format", "stream-json", "--verbose"} {
				if slices.Contains(invocation.Args, forbidden) {
					t.Fatalf("resume=%t interactive args contain %q: %q", resume, forbidden, invocation.Args)
				}
			}
			joined := strings.Join(invocation.Args, "\x00")
			if !strings.Contains(joined, "--settings\x00/private/settings.json") || !strings.Contains(joined, "--model\x00"+attempt.Model) {
				t.Fatalf("resume=%t args = %q", resume, invocation.Args)
			}
			if resume && !strings.Contains(joined, "--resume\x00session") {
				t.Fatalf("resume args = %q", invocation.Args)
			}
			if !resume && (!strings.Contains(joined, "--session-id\x00session") || !strings.Contains(joined, "--name\x00name")) {
				t.Fatalf("initial args = %q", invocation.Args)
			}
		}
		if _, err := BuildNamedForRuntime("claude-code", attempt, "prompt", false, "name", workerRuntime, ""); err == nil {
			t.Fatal("interactive Claude Code accepted missing bridge settings")
		}
	}
}

func TestEmptyModelPreservesCodexInvocation(t *testing.T) {
	installHarnessFixtures(t)
	attempt := model.Attempt{SessionID: "session", WorktreePath: "/tree"}
	initial, err := BuildNamed("codex", attempt, "prompt", false, "")
	if err != nil {
		t.Fatal(err)
	}
	wantInitial := []string{"exec", "--json", "--dangerously-bypass-approvals-and-sandbox", "-C", "/tree", "prompt"}
	if strings.Join(initial.Args, "\x00") != strings.Join(wantInitial, "\x00") {
		t.Fatalf("initial args = %q, want %q", initial.Args, wantInitial)
	}
	resumed, err := BuildNamed("codex", attempt, "prompt", true, "")
	if err != nil {
		t.Fatal(err)
	}
	wantResumed := []string{"exec", "resume", "session", "prompt", "--json", "--dangerously-bypass-approvals-and-sandbox"}
	if strings.Join(resumed.Args, "\x00") != strings.Join(wantResumed, "\x00") {
		t.Fatalf("resume args = %q, want %q", resumed.Args, wantResumed)
	}
}

func TestModelUsesHarnessArgvFlagForInitialAndResume(t *testing.T) {
	installHarnessFixtures(t)
	for _, harness := range []string{"claude-code", "pi", "codex"} {
		t.Run(harness, func(t *testing.T) {
			attempt := model.Attempt{Model: "Luna/Sol $opaque", SessionID: "session", WorktreePath: "/tree"}
			for _, resume := range []bool{false, true} {
				invocation, err := BuildNamed(harness, attempt, "prompt", resume, "name")
				if err != nil {
					t.Fatal(err)
				}
				modelIndex := -1
				for i, arg := range invocation.Args {
					if arg == "--model" {
						modelIndex = i
						break
					}
				}
				if modelIndex < 0 || modelIndex+1 >= len(invocation.Args) || invocation.Args[modelIndex+1] != attempt.Model {
					t.Fatalf("resume=%t args = %q", resume, invocation.Args)
				}
			}
			attempt.Model = ""
			invocation, err := BuildNamed(harness, attempt, "prompt", false, "name")
			if err != nil {
				t.Fatal(err)
			}
			for _, arg := range invocation.Args {
				if arg == "--model" {
					t.Fatalf("empty model added to args = %q", invocation.Args)
				}
			}
		})
	}
}

func TestPiErrorReason(t *testing.T) {
	line := `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"overloaded"}}`
	if parsed := ParseLine("pi", []byte(line)); parsed.Failure != "overloaded" {
		t.Fatalf("failure = %q", parsed.Failure)
	}
}

func TestStableRejectedEventDiagnostics(t *testing.T) {
	tests := []struct {
		name, envelope, code, field string
	}{
		{"syntax", `<shephrd-event>{"type":"question","payload":}</shephrd-event>`, DiagnosticJSONSyntax, ""},
		{"trailing", `<shephrd-event>{"type":"question","payload":"x"} {}</shephrd-event>`, DiagnosticJSONTrailing, ""},
		{"unknown event field", `<shephrd-event>{"type":"question","payload":"x","question":{}}</shephrd-event>`, DiagnosticSchemaUnknownField, "question"},
		{"unknown checkpoint field", `<shephrd-event>{"type":"checkpoint","payload":"x","checkpoint":{"schema_version":1,"summary":"x","completed":[],"next_steps":["x"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[],"extra":true}}</shephrd-event>`, DiagnosticSchemaUnknownField, "extra"},
		{"payload type", `<shephrd-event>{"type":"question","payload":3}</shephrd-event>`, DiagnosticSchemaTypeMismatch, "payload"},
		{"semantic", `<shephrd-event>{"type":"done","payload":"x"}</shephrd-event>`, DiagnosticSemanticInvalid, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, found, diagnostic := ParseEventCandidate(test.envelope)
			if !found || diagnostic == nil || diagnostic.Code != test.code || diagnostic.Field != test.field || !diagnostic.Repairable() {
				t.Fatalf("event=%+v found=%t diagnostic=%+v", event, found, diagnostic)
			}
			if test.code == DiagnosticJSONSyntax && (diagnostic.Offset < 1 || !diagnostic.RequiresCheckpoint) {
				t.Fatalf("syntax diagnostic lacks bounded checkpoint repair metadata: %+v", diagnostic)
			}
			if test.name == "unknown checkpoint field" && !diagnostic.RequiresCheckpoint {
				t.Fatalf("checkpoint diagnostic does not require a corrected checkpoint: %+v", diagnostic)
			}
			if !reflect.DeepEqual(event, model.Event{}) {
				t.Fatalf("rejected candidate returned event %+v", event)
			}
		})
	}
}

func TestParseEventsPreservesOrderAndSingleEnvelopeCompatibility(t *testing.T) {
	checkpoint := `<shephrd-event>{"type":"checkpoint","payload":"progress","checkpoint":{"schema_version":1,"summary":"progress","completed":[],"next_steps":["finish"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
	done := `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:work"}</shephrd-event>`
	events, found, err := ParseEvents("before\n" + checkpoint + "\nbetween\n" + done + "\nafter")
	if err != nil || !found || len(events) != 2 || events[0].Type != "checkpoint" || events[1].Type != "done" {
		t.Fatalf("events=%+v found=%t err=%v", events, found, err)
	}
	if _, found, err := ParseEvent(checkpoint + done); err == nil || !found || !strings.Contains(err.Error(), "2 event envelopes") {
		t.Fatalf("plural singular-wrapper result found=%t err=%v", found, err)
	}
	event, found, err := ParseEvent(done)
	if err != nil || !found || event.Type != "done" || event.Artifact != "branch:work" {
		t.Fatalf("event=%+v found=%t err=%v", event, found, err)
	}
}

func TestParseEventsReturnsDuplicateTerminalsInOrder(t *testing.T) {
	checkpoint := `<shephrd-event>{"type":"checkpoint","payload":"progress","checkpoint":{"schema_version":1,"summary":"progress","completed":[],"next_steps":["finish"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
	done := `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:work"}</shephrd-event>`
	failed := `<shephrd-event>{"type":"failed","payload":"duplicate"}</shephrd-event>`
	events, found, err := ParseEvents(checkpoint + done + failed)
	if err != nil || !found || len(events) != 3 || events[0].Type != "checkpoint" || events[1].Type != "done" || events[2].Type != "failed" {
		t.Fatalf("events=%+v found=%t err=%v", events, found, err)
	}
}

func TestParseEventsRejectsCompleteInvalidRecord(t *testing.T) {
	checkpoint := `<shephrd-event>{"type":"checkpoint","payload":"progress","checkpoint":{"schema_version":1,"summary":"progress","completed":[],"next_steps":["finish"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
	tests := []struct {
		name, suffix, want string
	}{
		{"malformed JSON", `<shephrd-event>{"type":"done","payload":}</shephrd-event>`, "invalid worker event JSON"},
		{"unknown field", `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:work","extra":true}</shephrd-event>`, "unknown field"},
		{"missing close", `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:work"}`, "missing"},
		{"nested open", `<shephrd-event>{"type":"done","payload":"<shephrd-event>{}","artifact":"branch:work"}</shephrd-event>`, "nested"},
		{"oversized", eventOpenTag + "{" + strings.Repeat("x", maxEventEnvelopeBytes) + eventCloseTag, "exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events, found, err := ParseEvents(checkpoint + "\n" + test.suffix)
			if err == nil || !found || len(events) != 0 || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("events=%+v found=%t err=%v", events, found, err)
			}
		})
	}
}

func TestClaudeAssistantEventAuthorityIsExplicit(t *testing.T) {
	tests := []struct {
		name, line string
		authority  EventAuthority
	}{
		{"assigned worker", `{"type":"assistant","session_id":"session","parent_tool_use_id":null,"message":{"content":[{"type":"text","text":"top"}]}}`, EventAuthorityAssignedWorker},
		{"nested worker", `{"type":"assistant","session_id":"session","parent_tool_use_id":"toolu_parent","subagent_type":"general-purpose","message":{"content":[{"type":"text","text":"nested"}]}}`, EventAuthorityNestedWorker},
		{"nested equivalent metadata", `{"type":"assistant","session_id":"session","agent_id":"agent_child","message":{"content":[{"type":"text","text":"nested"}]}}`, EventAuthorityNestedWorker},
		{"unknown legacy shape", `{"type":"assistant","session_id":"session","message":{"content":[{"type":"text","text":"unknown"}]}}`, EventAuthorityNone},
		{"malformed parent", `{"type":"assistant","session_id":"session","parent_tool_use_id":3,"message":{"content":[{"type":"text","text":"unknown"}]}}`, EventAuthorityNone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed := ParseLine("claude-code", []byte(test.line))
			if parsed.EventAuthority != test.authority || parsed.Text == "" {
				t.Fatalf("parsed=%+v", parsed)
			}
		})
	}
}

func TestCandidateRecognitionIgnoresBareTagProseButKeepsPlausibleEnvelopeStrict(t *testing.T) {
	for _, text := range []string{
		"The marker is `<shephrd-event>` and this is ordinary prose.",
		"Use <shephrd-event> as the opening tag and </shephrd-event> as the close.",
	} {
		events, found, diagnostic := ParseEventCandidateSet(text)
		if found || diagnostic != nil || len(events) != 0 {
			t.Fatalf("text=%q events=%+v found=%t diagnostic=%+v", text, events, found, diagnostic)
		}
	}
	_, found, diagnostic := ParseEventCandidateSet("prefix <shephrd-event>\n  {\"type\":\"done\",\"payload\":\"x\",\"artifact\":\"branch:work\"}")
	if !found || diagnostic == nil || diagnostic.Code != DiagnosticFramingMissingClose || !diagnostic.Repairable() {
		t.Fatalf("found=%t diagnostic=%+v", found, diagnostic)
	}
}

func TestStrictCandidateSetRejectsDuplicateAndPostTerminalEnvelopes(t *testing.T) {
	checkpoint := `<shephrd-event>{"type":"checkpoint","payload":"ready","checkpoint":{"schema_version":1,"summary":"ready","completed":[],"next_steps":["finish"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
	done := `<shephrd-event>{"type":"done","payload":"done","artifact":"branch:work"}</shephrd-event>`
	for _, text := range []string{done + done, done + checkpoint, checkpoint + done + checkpoint} {
		events, found, diagnostic := ParseEventCandidateSet(text)
		if !found || diagnostic == nil || diagnostic.Code != DiagnosticSequenceInvalid || len(events) != 0 || diagnostic.Repairable() {
			t.Fatalf("events=%+v found=%t diagnostic=%+v", events, found, diagnostic)
		}
	}
}

func TestStrictEnvelope(t *testing.T) {
	text := `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:work"}</shephrd-event>`
	event, found, err := ParseEvent(text)
	if err != nil || !found || event.Type != "done" || event.Artifact != "branch:work" {
		t.Fatalf("event = %+v, found = %v, err = %v", event, found, err)
	}
	_, _, err = ParseEvent(`<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:work","memory":["note"]}</shephrd-event>`)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("removed field error = %v", err)
	}
	_, found, err = ParseEvent(`plain text`)
	if err != nil || found {
		t.Fatalf("found = %v, err = %v", found, err)
	}
	_, _, err = ParseEvent(`<shephrd-event>{"type":"done","payload":"x","extra":true}</shephrd-event>`)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
	_, _, err = ParseEvent(`<shephrd-event>{"type":"done","payload":"x"} {}</shephrd-event>`)
	if err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("error = %v", err)
	}
}
