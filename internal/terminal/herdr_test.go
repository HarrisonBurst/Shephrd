package terminal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixtureRunner struct {
	responses map[string][]fixtureResponse
	calls     [][]string
}

type fixtureResponse struct {
	stdout string
	stderr string
	err    error
}

func (r *fixtureRunner) Run(socketPath string, args ...string) ([]byte, []byte, error) {
	call := append([]string{socketPath}, args...)
	r.calls = append(r.calls, call)
	key := strings.Join(args, " ")
	responses := r.responses[key]
	if len(responses) == 0 {
		return nil, nil, errors.New("unexpected fixture: " + key)
	}
	response := responses[0]
	r.responses[key] = responses[1:]
	return []byte(response.stdout), []byte(response.stderr), response.err
}

func TestCreateTabPublishesAndVerifiesExactIdentity(t *testing.T) {
	runner := &fixtureRunner{responses: map[string][]fixtureResponse{
		"tab create --workspace w7 --cwd /tree --label label --no-focus": {{stdout: `{"result":{"root_pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7"},"tab":{"tab_id":"w7:t2","workspace_id":"w7","pane_count":1}}}`}},
		"pane get w7:p2": {{stdout: `{"result":{"pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7"}}}`}, {stdout: `{"result":{"pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7"}}}`}},
		"pane report-metadata w7:p2 --source source --agent pi --title label --display-agent label --state-label working=working": {{stdout: `{"result":{"type":"ok"}}`}},
		"pane rename w7:p2 label": {{stdout: `{"result":{"type":"ok"}}`}},
	}}
	client := NewWithRunner("/socket", runner)
	endpoint, err := client.CreateTab(TabSpec{WorkspaceID: "w7", CWD: "/tree", Label: "label", Harness: "pi", Source: "source"})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.TabID != "w7:t2" || endpoint.PaneID != "w7:p2" {
		t.Fatalf("endpoint = %+v", endpoint)
	}
}

func TestAgentLifecycleUsesExactPaneSourceSessionAndSequence(t *testing.T) {
	runner := &fixtureRunner{responses: map[string][]fixtureResponse{
		"pane get w7:p2": {{stdout: `{"result":{"pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7"}}}`}, {stdout: `{"result":{"pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7"}}}`}},
		"pane report-agent w7:p2 --source shephrd:attempt:run:3 --agent pi --state working --seq 1 --agent-session-id native": {{stdout: `{"result":{"type":"ok"}}`}},
		"pane release-agent w7:p2 --source shephrd:attempt:run:3 --agent pi --seq 2":                                          {{stdout: `{"result":{"type":"ok"}}`}},
	}}
	client := NewWithRunner("/socket", runner)
	endpoint := Endpoint{SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}
	if err := client.ReportAgent(endpoint, AgentReport{Source: "shephrd:attempt:run:3", Agent: "pi", State: "working", Sequence: 1, SessionID: "native"}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseAgent(endpoint, "shephrd:attempt:run:3", "pi", 2); err != nil {
		t.Fatal(err)
	}
}

func TestAgentLifecycleRejectsInvalidAuthority(t *testing.T) {
	client := NewWithRunner("/socket", &fixtureRunner{})
	endpoint := Endpoint{SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}
	for _, report := range []AgentReport{
		{Source: "bad source", Agent: "pi", State: "working", Sequence: 1},
		{Source: "source", Agent: "pi", State: "done", Sequence: 1},
		{Source: "source", Agent: "pi", State: "working"},
	} {
		if err := client.ReportAgent(endpoint, report); err == nil {
			t.Fatalf("report accepted: %+v", report)
		}
	}
}

func TestCreateTabRejectsCrossWorkspace(t *testing.T) {
	runner := &fixtureRunner{responses: map[string][]fixtureResponse{
		"tab create --workspace w7 --cwd /tree --label label --no-focus": {{stdout: `{"result":{"root_pane":{"pane_id":"w8:p2","tab_id":"w8:t2","workspace_id":"w8"},"tab":{"tab_id":"w8:t2","workspace_id":"w8","pane_count":1}}}`}},
	}}
	_, err := NewWithRunner("/socket", runner).CreateTab(TabSpec{WorkspaceID: "w7", CWD: "/tree", Label: "label", Source: "source"})
	if err == nil || !strings.Contains(err.Error(), "cross-workspace") {
		t.Fatalf("error = %v", err)
	}
}

func TestCloseExactPaneRequiresOnePaneAndConfirmsGone(t *testing.T) {
	runner := &fixtureRunner{responses: map[string][]fixtureResponse{
		"pane get w7:p2":   {{stdout: `{"result":{"pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7"}}}`}, {stdout: `{"error":{"code":"pane_not_found","message":"gone"}}`}},
		"tab get w7:t2":    {{stdout: `{"result":{"tab":{"tab_id":"w7:t2","workspace_id":"w7","pane_count":1}}}`}},
		"pane close w7:p2": {{stdout: `{"result":{"type":"ok"}}`}},
	}}
	if err := NewWithRunner("/socket", runner).CloseExactPane(Endpoint{SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedHerdrJSONIsRejected(t *testing.T) {
	runner := &fixtureRunner{responses: map[string][]fixtureResponse{
		"pane get w7:p2": {{stdout: "not json"}},
	}}
	_, err := NewWithRunner("/socket", runner).GetPane(Endpoint{SocketPath: "/socket", WorkspaceID: "w7", PaneID: "w7:p2"})
	if err == nil || !strings.Contains(err.Error(), "invalid Herdr JSON") {
		t.Fatalf("error = %v", err)
	}
}

func TestHerdrDiagnosticsUsesJSONStatus(t *testing.T) {
	for _, test := range []struct {
		name     string
		output   string
		protocol string
		version  string
	}{
		{"protocol floor", `{"running":true,"version":"fixture-1","protocol":17,"compatible":true,"socket":"/socket"}`, "17", "fixture-1"},
		{"herdr 0.9.1", `{"status":"running","running":true,"version":"0.9.1","protocol":22,"capabilities":{"live_handoff":true,"endpoint_protocol_generation":1},"compatible":true,"endpoint_compatible":true,"socket":"/socket","session":null,"restart_needed":false,"server_binary_stale":false}`, "22", "0.9.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fixtureRunner{responses: map[string][]fixtureResponse{
				"status server --json": {{stdout: test.output}},
			}}
			diagnostics, err := NewWithRunner("/socket", runner).Diagnostics()
			if err != nil {
				t.Fatal(err)
			}
			if diagnostics.ProtocolVersion != test.protocol || diagnostics.ProviderVersion != test.version {
				t.Fatalf("diagnostics = %+v", diagnostics)
			}
		})
	}
}

func TestHerdrDiagnosticsRejectsUnsafeStatus(t *testing.T) {
	for _, test := range []struct {
		name     string
		response fixtureResponse
		kind     string
		message  string
	}{
		{"command failure", fixtureResponse{stderr: "connection refused", err: errors.New("exit status 1")}, ErrorUnavailable, "connection refused"},
		{"human status", fixtureResponse{stdout: "status: running\nprivate_protocol: 22\nprivate_protocol_compatible: yes\n"}, ErrorMalformed, "invalid server status JSON"},
		{"invalid protocol type", fixtureResponse{stdout: `{"protocol":"22"}`}, ErrorMalformed, "invalid server status JSON"},
		{"missing protocol", fixtureResponse{stdout: `{"running":true,"compatible":true,"socket":"/socket"}`}, ErrorMalformed, "missing protocol"},
		{"null protocol", fixtureResponse{stdout: `{"protocol":null}`}, ErrorMalformed, "missing protocol"},
		{"older server", fixtureResponse{stdout: `{"running":true,"protocol":16,"compatible":true,"socket":"/socket"}`}, ErrorIncompatible, "protocol 17 or newer is required"},
		{"stopped server", fixtureResponse{stdout: `{"running":false,"protocol":22,"compatible":true,"socket":"/socket"}`}, ErrorUnavailable, "server is not running"},
		{"unknown running state", fixtureResponse{stdout: `{"protocol":22,"compatible":true,"socket":"/socket"}`}, ErrorUnavailable, "server is not running"},
		{"incompatible", fixtureResponse{stdout: `{"running":true,"protocol":22,"compatible":false,"socket":"/socket"}`}, ErrorIncompatible, "compatibility"},
		{"unknown compatibility", fixtureResponse{stdout: `{"running":true,"protocol":22,"socket":"/socket"}`}, ErrorIncompatible, "compatibility"},
		{"endpoint incompatible", fixtureResponse{stdout: `{"running":true,"protocol":22,"compatible":true,"endpoint_compatible":false,"socket":"/socket"}`}, ErrorIncompatible, "compatibility"},
		{"different socket", fixtureResponse{stdout: `{"running":true,"protocol":22,"compatible":true,"socket":"/other"}`}, ErrorMalformed, "socket identity mismatch"},
		{"missing socket", fixtureResponse{stdout: `{"running":true,"protocol":22,"compatible":true}`}, ErrorMalformed, "socket identity mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fixtureRunner{responses: map[string][]fixtureResponse{
				"status server --json": {test.response},
			}}
			err := NewWithRunner("/socket", runner).ValidateProtocol()
			if err == nil || Classify(err) != test.kind || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %s containing %q", err, test.kind, test.message)
			}
		})
	}
}

func TestExecRunnerUsesRecordedSocket(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "herdr")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$HERDR_SOCKET_PATH\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HERDR_SOCKET_PATH", "/ambient")
	output, _, err := (ExecRunner{}).Run("/recorded", "status")
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "/recorded" {
		t.Fatalf("socket = %q", output)
	}
}

func TestValidateParentRequiresExactEnvironmentAndIdentity(t *testing.T) {
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SOCKET_PATH", "/socket")
	t.Setenv("HERDR_WORKSPACE_ID", "w7")
	t.Setenv("HERDR_PANE_ID", "w7:p1")
	runner := &fixtureRunner{responses: map[string][]fixtureResponse{
		"status server --json": {{stdout: `{"running":true,"protocol":17,"compatible":true,"socket":"/socket"}`}},
		"pane get w7:p1":       {{stdout: `{"result":{"pane":{"pane_id":"w7:p1","tab_id":"w7:t1","workspace_id":"w7"}}}`}},
		"tab get w7:t1":        {{stdout: `{"result":{"tab":{"tab_id":"w7:t1","workspace_id":"w7","pane_count":1}}}`}},
	}}
	parent, err := NewWithRunner("/socket", runner).ValidateParent()
	if err != nil {
		t.Fatal(err)
	}
	if parent.TabID != "w7:t1" {
		t.Fatalf("parent = %+v", parent)
	}
}

func TestLiveHerdrTabIsOptIn(t *testing.T) {
	if os.Getenv("SHEPHRD_REAL_HERDR_E2E") != "1" {
		t.Skip("set SHEPHRD_REAL_HERDR_E2E=1 to run against the live Herdr server")
	}
	client := New(os.Getenv("HERDR_SOCKET_PATH"))
	parent, err := client.ValidateParent()
	if err != nil {
		t.Fatal(err)
	}
	runtimeClient := RuntimeClient(client)
	if os.Getenv("SHEPHRD_REAL_HERDR_EXTENSION_E2E") == "1" {
		extensionClient, extensionParent := liveHerdrExtensionClient(t)
		if extensionParent != parent {
			t.Fatalf("direct parent = %+v, extension parent = %+v", parent, extensionParent)
		}
		runtimeClient = extensionClient
	}
	before, err := liveFocus(client)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "queued-input-executed")
	endpoint, err := runtimeClient.CreateWorkspace(WorkspaceSpec{WorkspaceID: parent.WorkspaceID, CWD: dir, Label: "shephrd-live-test", Harness: "test", Source: "shephrd-live-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeClient.Close(endpoint)
	if err := runtimeClient.ReportAgent(endpoint, AgentReport{Source: "shephrd-live-test", Agent: "test", State: "working", Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	if err := runtimeClient.ReleaseAgent(endpoint, "shephrd-live-test", "test", 2); err != nil {
		t.Fatal(err)
	}
	if err := runtimeClient.Start(endpoint, "exec sh -c 'echo shephrd-live-extension; sleep 1'"); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := client.run("pane", "send-text", endpoint.PaneID, "touch "+marker); err != nil {
		t.Fatalf("queue text: %s: %v", stderr, err)
	}
	if _, stderr, err := client.run("pane", "send-keys", endpoint.PaneID, "Enter"); err != nil {
		t.Fatalf("queue Enter: %s: %v", stderr, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	gone, processObserved, outputObserved := false, false, false
	for time.Now().Before(deadline) {
		if info, infoErr := runtimeClient.ProcessInfo(endpoint); infoErr == nil && info.PaneID == endpoint.PaneID {
			processObserved = true
		}
		if _, readErr := runtimeClient.Read(endpoint, 25); readErr == nil {
			outputObserved = true
		}
		if _, err := client.GetPane(endpoint); err != nil {
			if IsNotFound(err) {
				gone = true
				break
			}
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gone {
		t.Fatal("live Herdr pane did not end")
	}
	if !processObserved || !outputObserved {
		t.Fatalf("processObserved=%t outputObserved=%t", processObserved, outputObserved)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("queued terminal input reached a shell: %v", err)
	}
	after, err := liveFocus(client)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("focus changed from %v to %v", before, after)
	}
}

func liveFocus(client Client) ([3]string, error) {
	output, stderr, err := client.run("api", "snapshot")
	if err != nil {
		return [3]string{}, fmt.Errorf("read Herdr focus: %s: %w", stderr, err)
	}
	var response struct {
		Result struct {
			Snapshot struct {
				WorkspaceID string `json:"focused_workspace_id"`
				TabID       string `json:"focused_tab_id"`
				PaneID      string `json:"focused_pane_id"`
			} `json:"snapshot"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return [3]string{}, err
	}
	return [3]string{response.Result.Snapshot.WorkspaceID, response.Result.Snapshot.TabID, response.Result.Snapshot.PaneID}, nil
}
