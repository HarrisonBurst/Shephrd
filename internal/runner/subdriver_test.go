package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/claudebridge"
	"shephrd/internal/model"
	"shephrd/internal/pibridge"
	"shephrd/internal/process"
)

func TestBoundedInteractiveTurnStopsOwnedProcessGroup(t *testing.T) {
	for _, harness := range []string{"pi", "claude-code"} {
		for _, acknowledged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/acknowledged=%t", harness, acknowledged), func(t *testing.T) {
				childFile := filepath.Join(t.TempDir(), "child")
				recorder := &bridgeRecorder{}
				pid := 0
				config := InteractiveConfig{
					Attempt:    model.Attempt{ID: "subdriver", RunGeneration: 1, SessionID: "fresh-session"},
					Invocation: adapter.Invocation{Command: "/bin/sh", Args: []string{"-c", "trap 'wait; exit 0' TERM; sleep 30 & echo $! > \"$1\"; wait", "sh", childFile}},
					Timeout:    300 * time.Millisecond, Name: harness, State: bridgeState(recorder),
					HarnessStarted: func(id int) error { pid = id; return nil },
					Lifecycle:      Lifecycle{Report: func(string, string) error { return nil }, Release: func() error { return nil }},
				}
				var run func() (Result, error)
				var socketPath string
				var frame any
				if harness == "pi" {
					channel, err := pibridge.Open(config.Attempt.ID, 1)
					if err != nil {
						t.Fatal(err)
					}
					defer channel.Close()
					socketPath = channel.SocketPath
					frame = map[string]any{"schema_version": 2, "token": channel.Token, "attempt_id": channel.AttemptID, "run_generation": 1, "seq": 1, "kind": "session", "session_id": config.Attempt.SessionID}
					run = func() (Result, error) { return RunPi(PiConfig{InteractiveConfig: config, Channel: channel}) }
				} else {
					channel, err := claudebridge.Open(config.Attempt.ID, 1)
					if err != nil {
						t.Fatal(err)
					}
					defer channel.Close()
					socketPath = channel.SocketPath
					frame = map[string]any{"schema_version": 1, "token": channel.Token, "attempt_id": channel.AttemptID, "run_generation": 1, "input": map[string]any{"session_id": config.Attempt.SessionID, "hook_event_name": "SessionStart", "source": "startup"}}
					run = func() (Result, error) {
						return RunClaude(ClaudeConfig{InteractiveConfig: config, Channel: channel, AcceptTimeout: time.Second})
					}
				}
				if acknowledged {
					connection, err := net.Dial("unix", socketPath)
					if err != nil {
						t.Fatal(err)
					}
					defer connection.Close()
					if err := json.NewEncoder(connection).Encode(frame); err != nil {
						t.Fatal(err)
					}
				}
				result, err := run()
				if err == nil || !strings.Contains(result.Failure, "timed out") || pid == 0 || process.Alive(pid) {
					t.Fatalf("result=%+v error=%v pid=%d", result, err, pid)
				}
				body, err := os.ReadFile(childFile)
				if err != nil {
					t.Fatal(err)
				}
				child, err := strconv.Atoi(strings.TrimSpace(string(body)))
				if err != nil || process.Alive(child) {
					t.Fatalf("descendant %d survived: %v", child, err)
				}
			})
		}
	}
}

func TestBoundedHeadlessTurnStopsItsOwnedHarness(t *testing.T) {
	log, err := os.Create(filepath.Join(t.TempDir(), "runner.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	pid := 0
	state := State{SetRunner: func(int) error { return nil }, SetSession: func(string) error { return nil }, UpdateCursor: func(int64) error { return nil }, Ingest: func(model.Event, int64) error { return nil }, RecordControlFailure: func(string) error { return nil }, RecordRunnerDeath: func(string) error { return nil }, Finish: func(int, string) error { return nil }}
	outcome, err := RunHeadless(HeadlessConfig{Attempt: model.Attempt{Harness: "pi", SessionID: "fixture"}, Invocation: adapter.Invocation{Command: "/bin/sh", Args: []string{"-c", "exec sleep 30"}}, Timeout: 50 * time.Millisecond, Log: log, Raw: io.Discard, State: state, HarnessStarted: func(id int) error { pid = id; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if pid == 0 || process.Alive(pid) || !strings.Contains(outcome.Failure, "timed out") {
		t.Fatalf("owned process not bounded: pid=%d outcome=%+v", pid, outcome)
	}
}
