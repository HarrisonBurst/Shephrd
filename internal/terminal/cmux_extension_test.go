package terminal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	extensionhost "shephrd/internal/extension"
)

func TestCmuxExtensionProviderOrchestratesRealSubprocesses(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "cmux-extension-fixture")
	build := exec.Command("go", "build", "-o", binary, "./internal/terminal/testdata/cmuxextension")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture extension: %s: %v", output, err)
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	logPath := filepath.Join(t.TempDir(), "operations.log")
	provider := NewCmuxExtensionProvider([]string{binary, logPath, binary}, hex.EncodeToString(digest[:]), os.Environ())
	context := ParentContext{Platform: "darwin", Values: map[string]string{"CMUX_SOCKET_PATH": "/socket", "CMUX_WORKSPACE_ID": cmuxWorkspace, "CMUX_SURFACE_ID": cmuxSurface}, InvalidKeys: map[string]bool{}}
	detection := provider.Detect(context)
	if detection.State != DetectionMatched || detection.Parent.WindowID != cmuxWindow || detection.Diagnostics.ProtocolVersion != "2" {
		t.Fatalf("detection = %+v", detection)
	}
	client := provider.WithSocket("/socket")
	if diagnostics, err := client.Diagnostics(); err != nil || diagnostics.ProviderVersion != "fixture-1" {
		t.Fatalf("diagnostics = %+v, err = %v", diagnostics, err)
	}
	prepared, err := client.(WorkspacePreparer).PrepareWorkspace(WorkspaceSpec{WindowID: cmuxWindow, CWD: "/tree", Label: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := prepared.Endpoint()
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(endpoint, "exec '/launcher'"); err != nil {
		t.Fatal(err)
	}
	if state, err := client.Inspect(endpoint); err != nil || state.Endpoint.WorkspaceID != endpoint.WorkspaceID || state.Endpoint.SurfaceID != endpoint.SurfaceID || state.PaneCount != 1 {
		t.Fatalf("state = %+v, err = %v", state, err)
	}
	if info, err := client.ProcessInfo(endpoint); err != nil || info.PaneID != cmuxPane || len(info.ForegroundProcesses) != 1 {
		t.Fatalf("process info = %+v, err = %v", info, err)
	}
	if output, err := client.Read(endpoint, 10); err != nil || output != "fixture output" {
		t.Fatalf("output = %q, err = %v", output, err)
	}
	if err := client.ReportAgent(endpoint, AgentReport{Source: "fixture", Agent: "pi", State: "working", Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseAgent(endpoint, "fixture", "pi", 2); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportMetadata(endpoint, "fixture", "pi", "fixture", "working"); err != nil {
		t.Fatal(err)
	}
	if err := client.Focus(endpoint); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(endpoint); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"detect", "diagnostics", "create", "commit", "start", "inspect", "process_info", "read", "report_agent", "release_agent", "report_metadata", "focus", "close"} {
		if !strings.Contains(string(log), operation+"\n") {
			t.Fatalf("operation %q missing from %q", operation, log)
		}
	}
}

func TestCmuxExtensionManifestDeclaresOnlyTypedCapability(t *testing.T) {
	var output bytes.Buffer
	if err := RunCmuxExtension([]string{"describe"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	var manifest extensionhost.Manifest
	if err := extensionhost.StrictDecode(output.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := extensionhost.ValidateManifest(manifest, cmuxExtensionID, []extensionhost.Capability{CmuxExtensionCapability()}); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Capabilities) != 1 || manifest.Capabilities[0].Name != "terminal.cmux" {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestCmuxExtensionCreateRequiresCommitOrExactAbort(t *testing.T) {
	for _, action := range []string{"commit", "abort", "eof"} {
		t.Run(action, func(t *testing.T) {
			responses := map[string][]cmuxFixtureResponse{
				"workspace create --name demo-task --cwd /tree --window " + cmuxWindow + " --focus false": {{stdout: `{"window_id":"` + cmuxWindow + `","workspace_id":"` + cmuxWorkspace + `","surface_id":"` + cmuxSurface + `"}`}},
				"tree --workspace " + cmuxWorkspace + " --window " + cmuxWindow:                           {{stdout: cmuxTree(false)}},
				"workspace list --window " + cmuxWindow:                                                   {{stdout: `{"workspaces":[{"id":"` + cmuxWorkspace + `","current_directory":"/tree"}]}`}},
			}
			if action == "abort" || action == "eof" {
				responses["workspace close "+cmuxWorkspace+" --window "+cmuxWindow] = []cmuxFixtureResponse{{stdout: `{"ok":true}`}}
				responses["tree --workspace "+cmuxWorkspace+" --window "+cmuxWindow] = append(responses["tree --workspace "+cmuxWorkspace+" --window "+cmuxWindow], cmuxFixtureResponse{stdout: cmuxTree(false)}, cmuxFixtureResponse{stderr: "Error: not_found: Workspace not found", err: fmt.Errorf("exit")})
			}
			runner := &cmuxFixtureRunner{responses: responses}
			factory := func(socketPath string) CmuxClient {
				client := cmuxTestClient(t, runner)
				client.SocketPath = socketPath
				return client
			}
			request := cmuxExtensionTestRequest(t, "create", extensionCreateRequest{SocketPath: "/socket", Spec: WorkspaceSpec{WindowID: cmuxWindow, CWD: "/tree", Label: "demo-task"}})
			control := extensionhost.Control{Wire: request.Wire, RequestID: request.RequestID, Action: action}
			var input bytes.Buffer
			if err := json.NewEncoder(&input).Encode(request); err != nil {
				t.Fatal(err)
			}
			if action != "eof" {
				if err := json.NewEncoder(&input).Encode(control); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			if err := runCmuxExtensionInvocationWithClient(&input, &output, factory); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			wantedFrames := 2
			if action == "eof" {
				wantedFrames = 1
			}
			if len(lines) != wantedFrames {
				t.Fatalf("frames = %q", lines)
			}
			var prepared extensionhost.Response
			if err := extensionhost.StrictDecode([]byte(lines[0]), &prepared); err != nil {
				t.Fatal(err)
			}
			if prepared.Status != "prepared" {
				t.Fatalf("prepared = %+v", prepared)
			}
			if action != "eof" {
				var final extensionhost.Response
				if err := extensionhost.StrictDecode([]byte(lines[1]), &final); err != nil {
					t.Fatal(err)
				}
				if final.RequestID != request.RequestID || final.Status != map[string]string{"commit": "ok", "abort": "aborted"}[action] {
					t.Fatalf("final = %+v", final)
				}
			}
			closed := false
			for _, call := range runner.calls {
				closed = closed || strings.Contains(strings.Join(call, " "), "workspace close "+cmuxWorkspace)
			}
			if closed != (action == "abort" || action == "eof") {
				t.Fatalf("action = %s, calls = %q", action, runner.calls)
			}
		})
	}
}

func TestCmuxExtensionRejectsUnknownRequestFields(t *testing.T) {
	request := cmuxExtensionTestRequest(t, "inspect", extensionEndpointRequest{})
	frame, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	frame = bytes.Replace(frame, []byte(`"operation":"inspect"`), []byte(`"operation":"inspect","unknown":true`), 1)
	if err := runCmuxExtensionInvocationWithClient(bytes.NewReader(append(frame, '\n')), &bytes.Buffer{}, func(string) CmuxClient { return CmuxClient{} }); err == nil {
		t.Fatal("unknown request field was accepted")
	}
}

func cmuxExtensionTestRequest(t *testing.T, operation string, payload any) extensionhost.Request {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return extensionhost.Request{
		Wire:              extensionhost.WireVersion{Major: extensionhost.WireMajor, Minor: extensionhost.WireMinor},
		RequestID:         "extension_request_000000000000000000000000",
		Capability:        cmuxCapability,
		CapabilityVersion: cmuxCapabilityVersion,
		Operation:         operation,
		DeadlineUnixMS:    time.Now().Add(15 * time.Second).UnixMilli(),
		Payload:           encoded,
	}
}
