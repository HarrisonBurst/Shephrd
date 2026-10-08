package terminal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	cmuxWindow    = "11111111-1111-4111-8111-111111111111"
	cmuxWorkspace = "22222222-2222-4222-8222-222222222222"
	cmuxPane      = "33333333-3333-4333-8333-333333333333"
	cmuxSurface   = "44444444-4444-4444-8444-444444444444"
)

type cmuxFixtureResponse struct {
	stdout string
	stderr string
	err    error
}

type cmuxFixtureRunner struct {
	responses map[string][]cmuxFixtureResponse
	calls     [][]string
}

func (r *cmuxFixtureRunner) Run(_ string, _ string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) < 5 {
		return nil, nil, errors.New("missing global arguments")
	}
	key := strings.Join(args[5:], " ")
	responses := r.responses[key]
	if len(responses) == 0 {
		return nil, nil, fmt.Errorf("unexpected cmux fixture: %s", key)
	}
	response := responses[0]
	r.responses[key] = responses[1:]
	return []byte(response.stdout), []byte(response.stderr), response.err
}

func TestCmuxValidateParentUsesExactExecutableSocketIdentityAndMethods(t *testing.T) {
	runner := &cmuxFixtureRunner{responses: map[string][]cmuxFixtureResponse{
		"capabilities": {{stdout: cmuxCapabilities(t, requiredCmuxMethods)}},
		"version":      {{stdout: "cmux 0.64.22 (102) [ddd4a01bc]"}},
		"identify --workspace " + cmuxWorkspace + " --surface " + cmuxSurface: {{stdout: cmuxIdentify()}},
	}}
	client := cmuxTestClient(t, runner)
	setCmuxParentEnvironment(t)
	parent, err := client.ValidateParent()
	if err != nil {
		t.Fatal(err)
	}
	if parent.Backend != "cmux" || parent.SocketPath != "/socket" || parent.WindowID != cmuxWindow || parent.WorkspaceID != cmuxWorkspace || parent.PaneID != cmuxPane || parent.SurfaceID != cmuxSurface {
		t.Fatalf("parent = %+v", parent)
	}
	for _, call := range runner.calls {
		if len(call) < 5 || call[0] != "--socket" || call[1] != "/socket" || call[2] != "--json" || call[3] != "--id-format" || call[4] != "uuids" {
			t.Fatalf("call = %q", call)
		}
	}
}

func TestCmuxPreflightFailsClosedWithoutCreatingAnything(t *testing.T) {
	t.Run("non-macOS", func(t *testing.T) {
		client := cmuxTestClient(t, &cmuxFixtureRunner{})
		client.Platform = "linux"
		if _, err := client.ValidateParent(); err == nil || !strings.Contains(err.Error(), "only on macOS") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("outside cmux", func(t *testing.T) {
		client := cmuxTestClient(t, &cmuxFixtureRunner{})
		if _, err := client.ValidateParent(); err == nil || !strings.Contains(err.Error(), "CMUX_SOCKET_PATH") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing method redacts response", func(t *testing.T) {
		setCmuxParentEnvironment(t)
		runner := &cmuxFixtureRunner{responses: map[string][]cmuxFixtureResponse{
			"capabilities": {{stdout: `{"socket_path":"/socket","methods":["sensitive-capability-value"]}`}},
		}}
		client := cmuxTestClient(t, runner)
		_, err := client.ValidateParent()
		if err == nil || strings.Contains(err.Error(), "sensitive-capability-value") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("command failure redacts output", func(t *testing.T) {
		setCmuxParentEnvironment(t)
		runner := &cmuxFixtureRunner{responses: map[string][]cmuxFixtureResponse{
			"capabilities": {{stderr: "credential-value", err: errors.New("exit")}},
		}}
		client := cmuxTestClient(t, runner)
		_, err := client.ValidateParent()
		if err == nil || strings.Contains(err.Error(), "credential-value") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCmuxWorkspaceLifecycleUsesExactUUIDEndpoint(t *testing.T) {
	responses := map[string][]cmuxFixtureResponse{
		"workspace create --name demo-task --description Shephrd ephemeral worker --cwd /tree --window " + cmuxWindow + " --focus false": {{stdout: `{"window_id":"` + cmuxWindow + `","workspace_id":"` + cmuxWorkspace + `","surface_id":"` + cmuxSurface + `"}`}},
		"tree --workspace " + cmuxWorkspace + " --window " + cmuxWindow: {
			{stdout: cmuxTree(false)},
			{stdout: cmuxTree(false)},
			{stdout: cmuxTree(false)},
			{stdout: cmuxTree(false)},
			{stdout: cmuxTree(false)},
			{stdout: cmuxTree(false)},
			{stdout: cmuxTree(false)},
			{stdout: cmuxTree(false)},
			{stdout: cmuxTree(true)},
			{stdout: cmuxTree(true)},
			{stderr: "Error: not_found: Workspace not found", err: errors.New("exit")},
		},
		"workspace list --window " + cmuxWindow: {{stdout: `{"workspaces":[{"id":"` + cmuxWorkspace + `","current_directory":"/tree"}]}`}},
		"send --workspace " + cmuxWorkspace + " --surface " + cmuxSurface + " --window " + cmuxWindow + " -- exec '/launcher'": {{stdout: `{"ok":true}`}},
		"send-key --workspace " + cmuxWorkspace + " --surface " + cmuxSurface + " --window " + cmuxWindow + " enter":           {{stdout: `{"ok":true}`}},
		"top --workspace " + cmuxWorkspace + " --window " + cmuxWindow + " --processes":                                        {{stdout: cmuxTop()}},
		"read-screen --workspace " + cmuxWorkspace + " --surface " + cmuxSurface + " --window " + cmuxWindow + " --lines 25":   {{stdout: "native terminal output\n"}},
		"set-status shephrd-agent working:pi --workspace " + cmuxWorkspace + " --window " + cmuxWindow + " --priority 100":     {{stdout: `{"ok":true}`}},
		"set-status shephrd-worker working --workspace " + cmuxWorkspace + " --window " + cmuxWindow + " --priority 90":        {{stdout: `{"ok":true}`}},
		"clear-status shephrd-agent --workspace " + cmuxWorkspace + " --window " + cmuxWindow:                                  {{stdout: `{"ok":true}`}},
		"workspace select " + cmuxWorkspace + " --window " + cmuxWindow:                                                        {{stdout: `{"ok":true}`}},
		"focus-pane --pane " + cmuxPane + " --workspace " + cmuxWorkspace + " --window " + cmuxWindow:                          {{stdout: `{"ok":true}`}},
		"workspace close " + cmuxWorkspace + " --window " + cmuxWindow:                                                         {{stdout: `{"ok":true}`}},
	}
	runner := &cmuxFixtureRunner{responses: responses}
	client := cmuxTestClient(t, runner)
	endpoint, err := client.CreateWorkspace(WorkspaceSpec{WindowID: cmuxWindow, CWD: "/tree", Label: "demo-task", Description: "Shephrd ephemeral worker", Environment: []string{"SECRET=not-persisted"}})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Backend != "cmux" || endpoint.WindowID != cmuxWindow || endpoint.WorkspaceID != cmuxWorkspace || endpoint.PaneID != cmuxPane || endpoint.SurfaceID != cmuxSurface {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if err := client.Start(endpoint, "exec '/launcher'"); err != nil {
		t.Fatal(err)
	}
	info, err := client.ProcessInfo(endpoint)
	if err != nil || len(info.ForegroundProcesses) != 2 || info.ForegroundProcesses[1].PID != 901 || info.ForegroundProcesses[1].PGID != 900 || info.ForegroundProcesses[1].Path != "/absolute/shephrd" {
		t.Fatalf("process info = %+v, err = %v", info, err)
	}
	output, err := client.Read(endpoint, 25)
	if err != nil || output != "native terminal output\n" {
		t.Fatalf("output = %q, err = %v", output, err)
	}
	if err := client.ReportAgent(endpoint, AgentReport{Source: "source", Agent: "pi", State: "working", Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportMetadata(endpoint, "label", "pi", "source", "working"); err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseAgent(endpoint, "source", "pi", 2); err != nil {
		t.Fatal(err)
	}
	if err := client.Focus(endpoint); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(endpoint); err != nil {
		t.Fatal(err)
	}
	joined := make([]string, 0, len(runner.calls))
	for _, call := range runner.calls {
		joined = append(joined, strings.Join(call, " "))
	}
	if strings.Contains(strings.Join(joined, "\n"), "SECRET=not-persisted") {
		t.Fatal("workspace environment was persisted through cmux")
	}
}

func TestCmuxRefusesCloseAfterUserAltersTopology(t *testing.T) {
	altered := fmt.Sprintf(`{"windows":[{"id":"%s","workspaces":[{"id":"%s","title":"demo-task","selected":false,"panes":[{"id":"%s","surfaces":[{"id":"%s","pane_id":"%s","type":"terminal"}]},{"id":"55555555-5555-4555-8555-555555555555","surfaces":[]}]}]}]}`, cmuxWindow, cmuxWorkspace, cmuxPane, cmuxSurface, cmuxPane)
	runner := &cmuxFixtureRunner{responses: map[string][]cmuxFixtureResponse{
		"tree --workspace " + cmuxWorkspace + " --window " + cmuxWindow: {{stdout: altered}},
	}}
	client := cmuxTestClient(t, runner)
	endpoint := Endpoint{Backend: "cmux", SocketPath: "/socket", WindowID: cmuxWindow, WorkspaceID: cmuxWorkspace, PaneID: cmuxPane, SurfaceID: cmuxSurface}
	if err := client.Close(endpoint); err == nil || !strings.Contains(err.Error(), "topology") {
		t.Fatalf("error = %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "workspace close") {
			t.Fatalf("altered workspace was closed: %q", runner.calls)
		}
	}
}

func TestCmuxCreationFailureClosesOnlyReturnedUUID(t *testing.T) {
	runner := &cmuxFixtureRunner{responses: map[string][]cmuxFixtureResponse{
		"workspace create --name demo --cwd /tree --window " + cmuxWindow + " --focus false": {{stdout: `{"window_id":"` + cmuxWindow + `","workspace_id":"` + cmuxWorkspace + `","surface_id":"` + cmuxSurface + `"}`}},
		"tree --workspace " + cmuxWorkspace + " --window " + cmuxWindow: {
			{stdout: strings.Replace(cmuxTree(false), `"title":"demo-task"`, `"title":"wrong"`, 1)},
			{stderr: "Error: not_found: Workspace not found", err: errors.New("exit")},
		},
		"workspace close " + cmuxWorkspace + " --window " + cmuxWindow: {{stdout: `{"ok":true}`}},
	}}
	client := cmuxTestClient(t, runner)
	if _, err := client.CreateWorkspace(WorkspaceSpec{WindowID: cmuxWindow, CWD: "/tree", Label: "demo"}); err == nil {
		t.Fatal("inconsistent workspace accepted")
	}
	closeCall := []string{"--socket", "/socket", "--json", "--id-format", "uuids", "workspace", "close", cmuxWorkspace, "--window", cmuxWindow}
	if !slices.ContainsFunc(runner.calls, func(call []string) bool { return slices.Equal(call, closeCall) }) {
		t.Fatalf("calls = %q", runner.calls)
	}
}

func TestCmuxErrorClassificationFailsClosedOnTransportAndAuthenticationLoss(t *testing.T) {
	endpoint := Endpoint{Backend: "cmux", SocketPath: "/socket", WindowID: cmuxWindow, WorkspaceID: cmuxWorkspace, PaneID: cmuxPane, SurfaceID: cmuxSurface}
	for _, test := range []struct {
		name     string
		stderr   string
		kind     string
		notFound bool
	}{
		{name: "authoritative endpoint absence", stderr: "Error: not_found: Workspace not found", kind: ErrorEndpointAbsent, notFound: true},
		{name: "socket absence", stderr: "Error: Socket not found at /socket", kind: ErrorUnavailable},
		{name: "transport loss", stderr: "Error: connection refused", kind: ErrorUnavailable},
		{name: "authentication loss", stderr: "Error: unauthorized: Login required", kind: ErrorUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &cmuxFixtureRunner{responses: map[string][]cmuxFixtureResponse{
				"tree --workspace " + cmuxWorkspace + " --window " + cmuxWindow: {{stderr: test.stderr, err: errors.New("exit")}},
			}}
			client := cmuxTestClient(t, runner)
			_, err := client.Inspect(endpoint)
			if Classify(err) != test.kind || IsNotFound(err) != test.notFound {
				t.Fatalf("kind = %q, not found = %t, err = %v", Classify(err), IsNotFound(err), err)
			}
		})
	}
}

func TestCmuxCapabilityAndOutputSkewFailClosed(t *testing.T) {
	setCmuxParentEnvironment(t)
	t.Run("protocol version", func(t *testing.T) {
		runner := &cmuxFixtureRunner{responses: map[string][]cmuxFixtureResponse{
			"capabilities": {{stdout: strings.Replace(cmuxCapabilities(t, requiredCmuxMethods), `"version":2`, `"version":3`, 1)}},
		}}
		client := cmuxTestClient(t, runner)
		if _, err := client.ValidateParent(); Classify(err) != ErrorIncompatible {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		runner := &cmuxFixtureRunner{responses: map[string][]cmuxFixtureResponse{
			"capabilities": {{stdout: `{not-json`}},
		}}
		client := cmuxTestClient(t, runner)
		if _, err := client.ValidateParent(); Classify(err) != ErrorMalformed {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCmuxProcessAmbiguityFailsClosed(t *testing.T) {
	duplicate := `{"windows":[{"id":"` + cmuxWindow + `","workspaces":[{"id":"` + cmuxWorkspace + `","panes":[{"id":"` + cmuxPane + `","surfaces":[{"id":"` + cmuxSurface + `","processes":[{"pid":900,"pgid":900,"name":"one","path":"/one","children":[]},{"pid":900,"pgid":900,"name":"two","path":"/two","children":[]}]}]}]}]}]}`
	runner := &cmuxFixtureRunner{responses: map[string][]cmuxFixtureResponse{
		"tree --workspace " + cmuxWorkspace + " --window " + cmuxWindow:                 {{stdout: cmuxTree(false)}},
		"top --workspace " + cmuxWorkspace + " --window " + cmuxWindow + " --processes": {{stdout: duplicate}},
	}}
	client := cmuxTestClient(t, runner)
	endpoint := Endpoint{Backend: "cmux", SocketPath: "/socket", WindowID: cmuxWindow, WorkspaceID: cmuxWorkspace, PaneID: cmuxPane, SurfaceID: cmuxSurface}
	if _, err := client.ProcessInfo(endpoint); Classify(err) != ErrorAmbiguous {
		t.Fatalf("error = %v", err)
	}
}

type concurrentCmuxRunner struct {
	endpoints map[string]Endpoint
	closed    map[string]bool
	calls     [][]string
}

func (r *concurrentCmuxRunner) Run(_ string, _ string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	command := args[5:]
	if len(command) >= 5 && command[0] == "tree" {
		workspace := command[2]
		endpoint := r.endpoints[workspace]
		if r.closed[workspace] {
			return nil, []byte("Error: not_found: Workspace not found"), errors.New("exit")
		}
		return []byte(fmt.Sprintf(`{"windows":[{"id":"%s","workspaces":[{"id":"%s","title":"worker","selected":false,"panes":[{"id":"%s","focused":false,"surfaces":[{"id":"%s","pane_id":"%s","type":"terminal"}]}]}]}]}`, endpoint.WindowID, endpoint.WorkspaceID, endpoint.PaneID, endpoint.SurfaceID, endpoint.PaneID)), nil, nil
	}
	if len(command) >= 5 && command[0] == "workspace" && command[1] == "close" {
		r.closed[command[2]] = true
		return []byte(`{"ok":true}`), nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected concurrent cmux call %q", command)
}

func TestCmuxProcessAttributionIsBounded(t *testing.T) {
	var processes []Process
	seen := make(map[int]bool)
	for pid := 1; pid <= 256; pid++ {
		if err := flattenCmuxProcess(cmuxTopProcess{PID: pid, PGID: pid, Name: "process"}, &processes, seen); err != nil {
			t.Fatal(err)
		}
	}
	if err := flattenCmuxProcess(cmuxTopProcess{PID: 257, PGID: 257, Name: "process"}, &processes, seen); Classify(err) != ErrorAmbiguous {
		t.Fatalf("error = %v", err)
	}
}

func TestCmuxConcurrentEndpointCleanupNeverCrossCloses(t *testing.T) {
	first := Endpoint{Backend: "cmux", SocketPath: "/socket", WindowID: cmuxWindow, WorkspaceID: "55555555-5555-4555-8555-555555555555", PaneID: "66666666-6666-4666-8666-666666666666", SurfaceID: "77777777-7777-4777-8777-777777777777"}
	second := Endpoint{Backend: "cmux", SocketPath: "/socket", WindowID: cmuxWindow, WorkspaceID: "88888888-8888-4888-8888-888888888888", PaneID: "99999999-9999-4999-8999-999999999999", SurfaceID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
	runner := &concurrentCmuxRunner{endpoints: map[string]Endpoint{first.WorkspaceID: first, second.WorkspaceID: second}, closed: make(map[string]bool)}
	client := cmuxTestClient(t, runner)
	if err := client.Close(first); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(second); err != nil {
		t.Fatalf("second endpoint changed after first cleanup: %v", err)
	}
	if !runner.closed[first.WorkspaceID] || runner.closed[second.WorkspaceID] {
		t.Fatalf("closed = %+v", runner.closed)
	}
}

func cmuxTestClient(t *testing.T, runner CmuxRunner) CmuxClient {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "cmux")
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := NewCmuxWithRunner(executable, "/socket", runner)
	client.Platform = "darwin"
	client.SocketCheck = func(path string) error {
		if path != "/socket" {
			return fmt.Errorf("socket = %s", path)
		}
		return nil
	}
	return client
}

func setCmuxParentEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("CMUX_SOCKET_PATH", "/socket")
	t.Setenv("CMUX_WORKSPACE_ID", cmuxWorkspace)
	t.Setenv("CMUX_SURFACE_ID", cmuxSurface)
}

func cmuxCapabilities(t *testing.T, methods []string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"socket_path": "/socket", "protocol": "cmux-socket", "version": requiredCmuxProtocolVersion, "methods": methods})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func cmuxIdentify() string {
	return `{"socket_path":"/socket","caller":{"window_id":"` + cmuxWindow + `","workspace_id":"` + cmuxWorkspace + `","pane_id":"` + cmuxPane + `","surface_id":"` + cmuxSurface + `","surface_type":"terminal","is_browser_surface":false}}`
}

func cmuxTree(selected bool) string {
	return fmt.Sprintf(`{"windows":[{"id":"%s","workspaces":[{"id":"%s","title":"demo-task","selected":%t,"panes":[{"id":"%s","focused":%t,"surfaces":[{"id":"%s","pane_id":"%s","type":"terminal","focused":%t,"selected":%t}]}]}]}]}`, cmuxWindow, cmuxWorkspace, selected, cmuxPane, selected, cmuxSurface, cmuxPane, selected, selected)
}

func cmuxTop() string {
	return `{"windows":[{"id":"` + cmuxWindow + `","workspaces":[{"id":"` + cmuxWorkspace + `","panes":[{"id":"` + cmuxPane + `","surfaces":[{"id":"` + cmuxSurface + `","processes":[{"pid":900,"pgid":900,"name":"zsh","path":"/bin/zsh","children":[{"pid":901,"pgid":900,"name":"shephrd","path":"/absolute/shephrd","children":[]}]}]}]}]}]}]}`
}
