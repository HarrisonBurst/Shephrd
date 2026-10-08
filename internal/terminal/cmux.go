package terminal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

const requiredCmuxProtocolVersion = 2
const maxCmuxJSONBytes = 1024 * 1024

var requiredCmuxMethods = []string{
	"system.capabilities",
	"system.identify",
	"system.top",
	"window.current",
	"window.list",
	"workspace.action",
	"workspace.create",
	"workspace.list",
	"workspace.close",
	"workspace.select",
	"surface.send_text",
	"surface.send_key",
	"surface.read_text",
}

type CmuxRunner interface {
	Run(string, string, ...string) ([]byte, []byte, error)
}

type CmuxExecRunner struct{}

func (CmuxExecRunner) Run(executable, socketPath string, args ...string) ([]byte, []byte, error) {
	cmd := exec.Command(executable, args...)
	environment := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "CMUX_SOCKET" && key != "CMUX_SOCKET_PATH" && key != "CMUX_QUIET" {
			environment = append(environment, entry)
		}
	}
	cmd.Env = append(environment, "CMUX_SOCKET_PATH="+socketPath, "CMUX_QUIET=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type CmuxClient struct {
	Executable  string
	SocketPath  string
	Runner      CmuxRunner
	Platform    string
	SocketCheck func(string) error
}

func NewCmux(socketPath string) CmuxClient {
	return CmuxClient{SocketPath: socketPath, Runner: CmuxExecRunner{}, Platform: runtime.GOOS}
}

func NewCmuxWithRunner(executable, socketPath string, runner CmuxRunner) CmuxClient {
	return CmuxClient{Executable: executable, SocketPath: socketPath, Runner: runner, Platform: runtime.GOOS}
}

func (c CmuxClient) Backend() string {
	return "cmux"
}

func (c CmuxClient) WithSocket(socketPath string) RuntimeClient {
	c.SocketPath = socketPath
	return c
}

var cmuxContextKeys = []string{"CMUX_SOCKET_PATH", "CMUX_WORKSPACE_ID", "CMUX_SURFACE_ID"}

func (c CmuxClient) ContextKeys() []string {
	return append([]string(nil), cmuxContextKeys...)
}

func (c CmuxClient) DetectParent(context ParentContext) Detection {
	keys := c.ContextKeys()
	present := false
	for _, key := range keys {
		present = present || context.Value(key) != "" || context.InvalidKeys[key]
		if context.InvalidKeys[key] {
			return Detection{Provider: c.Backend(), State: DetectionInvalid, Err: fmt.Errorf("cmux parent context is oversized")}
		}
	}
	if !present {
		return Detection{Provider: c.Backend(), State: DetectionAbsent}
	}
	client := c
	if client.Platform == "" {
		client.Platform = context.Platform
	}
	parent, diagnostics, err := client.validateParent(func(key string) string { return context.Value(key) })
	if err != nil {
		return Detection{Provider: c.Backend(), State: DetectionInvalid, Err: err}
	}
	return Detection{Provider: c.Backend(), State: DetectionMatched, Parent: parent, Diagnostics: diagnostics}
}

func (c CmuxClient) ValidateEndpoint(endpoint Endpoint) error {
	if endpoint.Backend != c.Backend() || endpoint.SocketPath == "" || !cmuxUUID(endpoint.WindowID) || !cmuxUUID(endpoint.WorkspaceID) || !cmuxUUID(endpoint.PaneID) || !cmuxUUID(endpoint.SurfaceID) || endpoint.TabID != "" {
		return fmt.Errorf("cmux endpoint identity is incomplete or malformed")
	}
	return nil
}

func (c CmuxClient) ValidateParent() (Parent, error) {
	parent, _, err := c.validateParent(os.Getenv)
	return parent, err
}

func (c CmuxClient) validateParent(get func(string) string) (Parent, Diagnostics, error) {
	if c.Platform == "" {
		c.Platform = runtime.GOOS
	}
	if c.Platform != "darwin" {
		return Parent{}, Diagnostics{}, fmt.Errorf("cmux runtime is supported only on macOS")
	}
	if err := c.validateExecutable(); err != nil {
		return Parent{}, Diagnostics{}, err
	}
	socketPath := get("CMUX_SOCKET_PATH")
	workspaceID := get("CMUX_WORKSPACE_ID")
	surfaceID := get("CMUX_SURFACE_ID")
	if socketPath == "" || workspaceID == "" || surfaceID == "" {
		return Parent{}, Diagnostics{}, fmt.Errorf("cmux runtime requires CMUX_SOCKET_PATH, CMUX_WORKSPACE_ID, and CMUX_SURFACE_ID")
	}
	if c.SocketPath == "" {
		c.SocketPath = socketPath
	}
	if c.SocketPath != socketPath {
		return Parent{}, Diagnostics{}, fmt.Errorf("cmux socket path does not match CMUX_SOCKET_PATH")
	}
	diagnostics, err := c.Diagnostics()
	if err != nil {
		return Parent{}, Diagnostics{}, err
	}
	var response cmuxIdentifyResponse
	if err := c.runJSON(&response, "identify", "--workspace", workspaceID, "--surface", surfaceID); err != nil {
		return Parent{}, Diagnostics{}, fmt.Errorf("validate cmux parent identity: %w", err)
	}
	if response.SocketPath != c.SocketPath || response.Caller.WindowID == "" || response.Caller.PaneID == "" || response.Caller.WorkspaceID != workspaceID || response.Caller.SurfaceID != surfaceID || response.Caller.SurfaceType != "terminal" || response.Caller.IsBrowserSurface {
		return Parent{}, Diagnostics{}, &RuntimeError{Kind: ErrorMalformed, Operation: "validate cmux parent identity", Cause: fmt.Errorf("identity is inconsistent")}
	}
	for _, value := range []string{response.Caller.WindowID, response.Caller.WorkspaceID, response.Caller.PaneID, response.Caller.SurfaceID} {
		if !cmuxUUID(value) {
			return Parent{}, Diagnostics{}, &RuntimeError{Kind: ErrorMalformed, Operation: "validate cmux parent identity", Cause: fmt.Errorf("identity does not use exact UUIDs")}
		}
	}
	return Parent{Backend: "cmux", SocketPath: c.SocketPath, WindowID: response.Caller.WindowID, WorkspaceID: response.Caller.WorkspaceID, PaneID: response.Caller.PaneID, SurfaceID: response.Caller.SurfaceID}, diagnostics, nil
}

func (c CmuxClient) Validate() error {
	_, err := c.Diagnostics()
	return err
}

func (c CmuxClient) Diagnostics() (Diagnostics, error) {
	if c.Platform == "" {
		c.Platform = runtime.GOOS
	}
	if c.Platform != "darwin" {
		return Diagnostics{}, fmt.Errorf("cmux runtime is supported only on macOS")
	}
	if err := c.validateExecutable(); err != nil {
		return Diagnostics{}, err
	}
	if c.SocketPath == "" {
		return Diagnostics{}, fmt.Errorf("cmux socket path is empty")
	}
	check := c.SocketCheck
	if check == nil {
		check = validateCmuxSocket
	}
	if err := check(c.SocketPath); err != nil {
		return Diagnostics{}, &RuntimeError{Kind: ErrorUnavailable, Operation: "validate cmux socket", Cause: err}
	}
	var response struct {
		SocketPath string   `json:"socket_path"`
		Protocol   string   `json:"protocol"`
		Version    int      `json:"version"`
		Methods    []string `json:"methods"`
	}
	if err := c.runJSON(&response, "capabilities"); err != nil {
		return Diagnostics{}, fmt.Errorf("validate cmux API support: %w", err)
	}
	if response.SocketPath != c.SocketPath {
		return Diagnostics{}, &RuntimeError{Kind: ErrorMalformed, Operation: "validate cmux API support", Cause: fmt.Errorf("capability response came from a different socket")}
	}
	if response.Protocol != "cmux-socket" || response.Version != requiredCmuxProtocolVersion {
		return Diagnostics{}, &RuntimeError{Kind: ErrorIncompatible, Operation: "validate cmux API support", Cause: fmt.Errorf("socket protocol version is unsupported")}
	}
	if len(response.Methods) == 0 || len(response.Methods) > 1024 {
		return Diagnostics{}, &RuntimeError{Kind: ErrorMalformed, Operation: "validate cmux API support", Cause: fmt.Errorf("method list is invalid")}
	}
	available := make(map[string]bool, len(response.Methods))
	for _, method := range response.Methods {
		if method == "" || len(method) > 128 {
			return Diagnostics{}, &RuntimeError{Kind: ErrorMalformed, Operation: "validate cmux API support", Cause: fmt.Errorf("method identity is invalid")}
		}
		available[method] = true
	}
	for _, method := range requiredCmuxMethods {
		if !available[method] {
			return Diagnostics{}, &RuntimeError{Kind: ErrorIncompatible, Operation: "validate cmux API support", Cause: fmt.Errorf("required API support is unavailable")}
		}
	}
	versionOutput, _, err := c.run("version")
	if err != nil {
		return Diagnostics{}, fmt.Errorf("read cmux version: %w", err)
	}
	version := strings.TrimSpace(string(versionOutput))
	if len(version) > 128 || !cmuxVersionPattern.MatchString(version) {
		return Diagnostics{}, &RuntimeError{Kind: ErrorMalformed, Operation: "read cmux version", Cause: fmt.Errorf("version output is invalid")}
	}
	return Diagnostics{ProviderVersion: strings.TrimPrefix(version, "cmux "), ProtocolVersion: fmt.Sprint(response.Version), Capabilities: append([]string(nil), requiredCmuxMethods...)}, nil
}

func (c CmuxClient) CreateWorkspace(spec WorkspaceSpec) (Endpoint, error) {
	if spec.WindowID == "" || spec.CWD == "" || spec.Label == "" {
		return Endpoint{}, fmt.Errorf("cmux workspace creation requires window, cwd, and label")
	}
	if !cmuxUUID(spec.WindowID) {
		return Endpoint{}, fmt.Errorf("cmux workspace creation requires an exact window UUID")
	}
	if strings.IndexFunc(spec.Label, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 || strings.IndexFunc(spec.Description, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
		return Endpoint{}, fmt.Errorf("cmux workspace metadata contains control characters")
	}
	args := []string{"workspace", "create", "--name", spec.Label}
	if spec.Description != "" {
		args = append(args, "--description", spec.Description)
	}
	args = append(args, "--cwd", spec.CWD, "--window", spec.WindowID, "--focus", "false")
	var response struct {
		WindowID    string `json:"window_id"`
		WorkspaceID string `json:"workspace_id"`
		SurfaceID   string `json:"surface_id"`
	}
	if err := c.runJSON(&response, args...); err != nil {
		return Endpoint{}, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: fmt.Errorf("create cmux workspace: %w", err)}
	}
	endpoint := Endpoint{Backend: "cmux", SocketPath: c.SocketPath, WindowID: response.WindowID, WorkspaceID: response.WorkspaceID, SurfaceID: response.SurfaceID}
	if response.WindowID != spec.WindowID || !cmuxUUID(response.WorkspaceID) || !cmuxUUID(response.SurfaceID) {
		return c.creationFailure(endpoint, fmt.Errorf("cmux workspace create returned an incomplete or cross-window endpoint"))
	}
	state, err := c.inspect(endpoint)
	if err != nil {
		return c.creationFailure(endpoint, err)
	}
	if state.Label != spec.Label || state.Focused || state.PaneCount != 1 || state.SurfaceCount != 1 {
		return c.creationFailure(state.Endpoint, fmt.Errorf("cmux workspace creation returned unexpected focus, label, or topology"))
	}
	currentDirectory, err := c.workspaceDirectory(state.Endpoint)
	if err != nil {
		return c.creationFailure(state.Endpoint, err)
	}
	if !samePath(currentDirectory, spec.CWD) {
		return c.creationFailure(state.Endpoint, fmt.Errorf("cmux workspace creation returned an unexpected cwd"))
	}
	return state.Endpoint, nil
}

func (c CmuxClient) Start(endpoint Endpoint, command string) error {
	if command == "" || strings.ContainsAny(command, "\r\n") {
		return fmt.Errorf("cmux terminal command must be a single line")
	}
	if err := validateCmuxEndpoint(endpoint); err != nil {
		return err
	}
	if _, err := c.Inspect(endpoint); err != nil {
		return err
	}
	if _, _, err := c.run("send", "--workspace", endpoint.WorkspaceID, "--surface", endpoint.SurfaceID, "--window", endpoint.WindowID, "--", command); err != nil {
		return fmt.Errorf("send command to cmux surface: %w", err)
	}
	if _, _, err := c.run("send-key", "--workspace", endpoint.WorkspaceID, "--surface", endpoint.SurfaceID, "--window", endpoint.WindowID, "enter"); err != nil {
		return fmt.Errorf("start command in cmux surface: %w", err)
	}
	return nil
}

func (c CmuxClient) Inspect(endpoint Endpoint) (EndpointState, error) {
	return c.inspect(endpoint)
}

func (c CmuxClient) inspect(endpoint Endpoint) (EndpointState, error) {
	if endpoint.PaneID == "" {
		if endpoint.Backend != "cmux" || endpoint.SocketPath == "" || !cmuxUUID(endpoint.WindowID) || !cmuxUUID(endpoint.WorkspaceID) || !cmuxUUID(endpoint.SurfaceID) || endpoint.TabID != "" {
			return EndpointState{}, fmt.Errorf("cmux endpoint identity is incomplete or malformed")
		}
	} else if err := validateCmuxEndpoint(endpoint); err != nil {
		return EndpointState{}, err
	}
	var response cmuxTreeResponse
	if err := c.runJSON(&response, "tree", "--workspace", endpoint.WorkspaceID, "--window", endpoint.WindowID); err != nil {
		return EndpointState{}, err
	}
	if len(response.Windows) != 1 || response.Windows[0].ID != endpoint.WindowID || len(response.Windows[0].Workspaces) != 1 {
		return EndpointState{}, fmt.Errorf("cmux endpoint window or workspace topology is inconsistent")
	}
	workspace := response.Windows[0].Workspaces[0]
	if workspace.ID != endpoint.WorkspaceID || len(workspace.Panes) != 1 || len(workspace.Panes[0].Surfaces) != 1 {
		return EndpointState{}, fmt.Errorf("cmux endpoint workspace topology is inconsistent")
	}
	pane := workspace.Panes[0]
	surface := pane.Surfaces[0]
	if surface.ID != endpoint.SurfaceID || surface.PaneID != pane.ID || surface.Type != "terminal" || endpoint.PaneID != "" && endpoint.PaneID != pane.ID || !cmuxUUID(pane.ID) {
		return EndpointState{}, fmt.Errorf("cmux endpoint pane or surface topology is inconsistent")
	}
	endpoint.PaneID = pane.ID
	focused := workspace.Selected
	fullyFocused := workspace.Selected && pane.Focused && surface.Focused && surface.Selected
	return EndpointState{Endpoint: endpoint, Label: workspace.Title, Focused: focused, FullyFocused: fullyFocused, PaneCount: 1, SurfaceCount: 1}, nil
}

func (c CmuxClient) workspaceDirectory(endpoint Endpoint) (string, error) {
	var response struct {
		Workspaces []struct {
			ID               string `json:"id"`
			CurrentDirectory string `json:"current_directory"`
		} `json:"workspaces"`
	}
	if err := c.runJSON(&response, "workspace", "list", "--window", endpoint.WindowID); err != nil {
		return "", err
	}
	for _, workspace := range response.Workspaces {
		if workspace.ID == endpoint.WorkspaceID {
			if workspace.CurrentDirectory == "" {
				return "", fmt.Errorf("cmux workspace directory is unavailable")
			}
			return workspace.CurrentDirectory, nil
		}
	}
	return "", ErrNotFound
}

func (c CmuxClient) ProcessInfo(endpoint Endpoint) (ProcessInfo, error) {
	state, err := c.Inspect(endpoint)
	if err != nil {
		return ProcessInfo{}, err
	}
	endpoint = state.Endpoint
	var response cmuxTopResponse
	if err := c.runJSON(&response, "top", "--workspace", endpoint.WorkspaceID, "--window", endpoint.WindowID, "--processes"); err != nil {
		return ProcessInfo{}, fmt.Errorf("read cmux process attribution: %w", err)
	}
	if len(response.Windows) != 1 || response.Windows[0].ID != endpoint.WindowID || len(response.Windows[0].Workspaces) != 1 || response.Windows[0].Workspaces[0].ID != endpoint.WorkspaceID || len(response.Windows[0].Workspaces[0].Panes) != 1 || response.Windows[0].Workspaces[0].Panes[0].ID != endpoint.PaneID || len(response.Windows[0].Workspaces[0].Panes[0].Surfaces) != 1 {
		return ProcessInfo{}, fmt.Errorf("cmux process topology does not match the exact endpoint")
	}
	surface := response.Windows[0].Workspaces[0].Panes[0].Surfaces[0]
	if surface.ID != endpoint.SurfaceID {
		return ProcessInfo{}, fmt.Errorf("cmux process attribution returned a different surface")
	}
	processes := make([]Process, 0)
	seen := make(map[int]bool)
	for _, process := range surface.Processes {
		if err := flattenCmuxProcess(process, &processes, seen); err != nil {
			return ProcessInfo{}, err
		}
	}
	info := ProcessInfo{PaneID: endpoint.PaneID, ForegroundProcesses: processes}
	if len(processes) > 0 {
		info.ShellPID = processes[0].PID
	}
	for _, process := range processes {
		if process.PGID > 0 {
			info.ForegroundProcessGroup = process.PGID
			break
		}
	}
	return info, nil
}

func (c CmuxClient) Read(endpoint Endpoint, lines int) (string, error) {
	state, err := c.Inspect(endpoint)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 200
	}
	output, _, err := c.run("read-screen", "--workspace", state.Endpoint.WorkspaceID, "--surface", state.Endpoint.SurfaceID, "--window", state.Endpoint.WindowID, "--lines", fmt.Sprint(lines))
	if err != nil {
		return "", fmt.Errorf("read cmux surface: %w", err)
	}
	return string(output), nil
}

func (c CmuxClient) Focus(endpoint Endpoint) error {
	state, err := c.Inspect(endpoint)
	if err != nil {
		return err
	}
	endpoint = state.Endpoint
	if _, _, err := c.run("workspace", "select", endpoint.WorkspaceID, "--window", endpoint.WindowID); err != nil {
		return fmt.Errorf("focus cmux workspace: %w", err)
	}
	if _, _, err := c.run("focus-pane", "--pane", endpoint.PaneID, "--workspace", endpoint.WorkspaceID, "--window", endpoint.WindowID); err != nil {
		return fmt.Errorf("focus cmux pane: %w", err)
	}
	focused, err := c.Inspect(endpoint)
	if err != nil {
		return fmt.Errorf("verify focused cmux endpoint: %w", err)
	}
	if !focused.FullyFocused {
		return fmt.Errorf("cmux endpoint did not become fully focused")
	}
	return nil
}

func (c CmuxClient) Close(endpoint Endpoint) error {
	state, err := c.Inspect(endpoint)
	if err != nil {
		return err
	}
	endpoint = state.Endpoint
	if _, _, err := c.run("workspace", "close", endpoint.WorkspaceID, "--window", endpoint.WindowID); err != nil {
		return fmt.Errorf("close exact cmux workspace: %w", err)
	}
	for range 40 {
		_, err := c.Inspect(endpoint)
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("verify exact cmux workspace cleanup: %w", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("cmux workspace still exists after exact cleanup")
}

func (c CmuxClient) ReportAgent(endpoint Endpoint, report AgentReport) error {
	if err := validateAgentReport(report); err != nil {
		return err
	}
	state, err := c.Inspect(endpoint)
	if err != nil {
		return err
	}
	value := report.State + ":" + report.Agent
	if _, _, err := c.run("set-status", "shephrd-agent", value, "--workspace", state.Endpoint.WorkspaceID, "--window", state.Endpoint.WindowID, "--priority", "100"); err != nil {
		return fmt.Errorf("report cmux agent state: %w", err)
	}
	return nil
}

func (c CmuxClient) ReleaseAgent(endpoint Endpoint, source, agent string, sequence uint64) error {
	if err := validateAgentReport(AgentReport{Source: source, Agent: agent, State: "idle", Sequence: sequence}); err != nil {
		return err
	}
	state, err := c.Inspect(endpoint)
	if err != nil {
		return err
	}
	if _, _, err := c.run("clear-status", "shephrd-agent", "--workspace", state.Endpoint.WorkspaceID, "--window", state.Endpoint.WindowID); err != nil {
		return fmt.Errorf("release cmux agent state: %w", err)
	}
	return nil
}

func (c CmuxClient) ReportMetadata(endpoint Endpoint, label, harness, source, state string) error {
	if strings.IndexFunc(label+harness+source+state, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
		return fmt.Errorf("cmux state metadata contains control characters")
	}
	endpointState, err := c.Inspect(endpoint)
	if err != nil {
		return err
	}
	if _, _, err := c.run("set-status", "shephrd-worker", state, "--workspace", endpointState.Endpoint.WorkspaceID, "--window", endpointState.Endpoint.WindowID, "--priority", "90"); err != nil {
		return fmt.Errorf("report cmux worker state: %w", err)
	}
	return nil
}

func (c CmuxClient) creationFailure(endpoint Endpoint, cause error) (Endpoint, error) {
	if !cmuxUUID(endpoint.WindowID) || !cmuxUUID(endpoint.WorkspaceID) {
		return Endpoint{}, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: fmt.Errorf("%v; exact cmux cleanup is uncertain because create returned no safe workspace identity", cause)}
	}
	if err := c.closeKnownWorkspace(endpoint); err != nil {
		return endpoint, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: fmt.Errorf("%v; exact cmux cleanup is uncertain: %w", cause, err)}
	}
	return Endpoint{}, cause
}

func (c CmuxClient) closeKnownWorkspace(endpoint Endpoint) error {
	if !cmuxUUID(endpoint.WindowID) || !cmuxUUID(endpoint.WorkspaceID) {
		return fmt.Errorf("cmux cleanup requires exact window and workspace UUIDs")
	}
	if _, _, err := c.run("workspace", "close", endpoint.WorkspaceID, "--window", endpoint.WindowID); err != nil {
		return err
	}
	for range 40 {
		var response cmuxTreeResponse
		err := c.runJSON(&response, "tree", "--workspace", endpoint.WorkspaceID, "--window", endpoint.WindowID)
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("workspace remained present")
}

func (c CmuxClient) validateExecutable() error {
	if c.Executable == "" {
		path, err := exec.LookPath("cmux")
		if err != nil {
			return fmt.Errorf("required dependency %q is not installed or not on PATH", "cmux")
		}
		c.Executable, err = filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolve cmux executable: %w", err)
		}
	}
	if !filepath.IsAbs(c.Executable) {
		return fmt.Errorf("cmux executable must be an absolute path")
	}
	info, err := os.Stat(c.Executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("cmux executable is not an executable file")
	}
	return nil
}

func validateCmuxSocket(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("cmux socket path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("validate cmux socket: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0o077 != 0 || !cmuxSocketOwned(info) {
		return fmt.Errorf("cmux socket is not a private owner-controlled Unix socket")
	}
	return nil
}

func (c CmuxClient) run(args ...string) ([]byte, []byte, error) {
	if c.Runner == nil {
		return nil, nil, fmt.Errorf("cmux command runner is nil")
	}
	if c.Executable == "" {
		path, err := exec.LookPath("cmux")
		if err != nil {
			return nil, nil, fmt.Errorf("cmux executable is unavailable")
		}
		c.Executable, err = filepath.Abs(path)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve cmux executable: %w", err)
		}
	}
	commandArgs := append([]string{"--socket", c.SocketPath, "--json", "--id-format", "uuids"}, args...)
	output, stderr, err := c.Runner.Run(c.Executable, c.SocketPath, commandArgs...)
	if len(output) > maxCmuxJSONBytes || len(stderr) > maxCmuxJSONBytes {
		return nil, nil, &RuntimeError{Kind: ErrorMalformed, Operation: "cmux command", Cause: fmt.Errorf("response exceeds the size limit")}
	}
	if err != nil {
		kind := classifyCmuxFailure(output, stderr)
		return nil, nil, &RuntimeError{Kind: kind, Operation: "cmux command"}
	}
	return output, stderr, nil
}

func (c CmuxClient) runJSON(value any, args ...string) error {
	output, _, err := c.run(args...)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	if err := decoder.Decode(value); err != nil {
		return &RuntimeError{Kind: ErrorMalformed, Operation: "decode cmux response", Cause: fmt.Errorf("invalid JSON")}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return &RuntimeError{Kind: ErrorMalformed, Operation: "decode cmux response", Cause: fmt.Errorf("trailing JSON data")}
	}
	return nil
}

type cmuxIdentity struct {
	WindowID         string `json:"window_id"`
	WorkspaceID      string `json:"workspace_id"`
	PaneID           string `json:"pane_id"`
	SurfaceID        string `json:"surface_id"`
	SurfaceType      string `json:"surface_type"`
	IsBrowserSurface bool   `json:"is_browser_surface"`
}

type cmuxIdentifyResponse struct {
	SocketPath string       `json:"socket_path"`
	Caller     cmuxIdentity `json:"caller"`
}

type cmuxTreeResponse struct {
	Windows []struct {
		ID         string `json:"id"`
		Workspaces []struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Selected bool   `json:"selected"`
			Panes    []struct {
				ID       string `json:"id"`
				Focused  bool   `json:"focused"`
				Surfaces []struct {
					ID       string `json:"id"`
					PaneID   string `json:"pane_id"`
					Type     string `json:"type"`
					Focused  bool   `json:"focused"`
					Selected bool   `json:"selected"`
				} `json:"surfaces"`
			} `json:"panes"`
		} `json:"workspaces"`
	} `json:"windows"`
}

type cmuxTopProcess struct {
	PID      int              `json:"pid"`
	PGID     int              `json:"pgid"`
	Name     string           `json:"name"`
	Path     string           `json:"path"`
	Children []cmuxTopProcess `json:"children"`
}

type cmuxTopResponse struct {
	Windows []struct {
		ID         string `json:"id"`
		Workspaces []struct {
			ID    string `json:"id"`
			Panes []struct {
				ID       string `json:"id"`
				Surfaces []struct {
					ID        string           `json:"id"`
					Processes []cmuxTopProcess `json:"processes"`
				} `json:"surfaces"`
			} `json:"panes"`
		} `json:"workspaces"`
	} `json:"windows"`
}

func flattenCmuxProcess(process cmuxTopProcess, output *[]Process, seen map[int]bool) error {
	if len(*output) >= 256 || process.PID <= 0 || process.PGID < 0 || seen[process.PID] || len(process.Name) > 512 || len(process.Path) > 4096 {
		return &RuntimeError{Kind: ErrorAmbiguous, Operation: "read cmux process attribution", Cause: fmt.Errorf("process identity is invalid or duplicated")}
	}
	seen[process.PID] = true
	*output = append(*output, Process{PID: process.PID, PGID: process.PGID, Name: process.Name, Path: process.Path, Cmdline: strings.TrimSpace(process.Path + " " + process.Name)})
	for _, child := range process.Children {
		if err := flattenCmuxProcess(child, output, seen); err != nil {
			return err
		}
	}
	return nil
}

func classifyCmuxFailure(output, stderr []byte) string {
	combined := strings.TrimSpace(strings.ToLower(string(append(append([]byte(nil), output...), stderr...))))
	var response struct {
		Error *apiError `json:"error"`
	}
	for _, candidate := range [][]byte{output, stderr} {
		if json.Unmarshal(candidate, &response) == nil && response.Error != nil {
			code := strings.ToLower(strings.TrimSpace(response.Error.Code))
			switch code {
			case "not_found", "workspace_not_found", "pane_not_found", "surface_not_found", "window_not_found":
				return ErrorEndpointAbsent
			case "unauthorized", "authentication_required", "permission_denied":
				return ErrorUnauthorized
			case "unsupported", "incompatible", "method_not_found":
				return ErrorIncompatible
			}
		}
	}
	if strings.HasPrefix(combined, "error: not_found:") || strings.HasPrefix(combined, "not_found:") {
		return ErrorEndpointAbsent
	}
	if strings.HasPrefix(combined, "error: unauthorized:") || strings.HasPrefix(combined, "error: authentication_required:") || strings.HasPrefix(combined, "error: permission_denied:") {
		return ErrorUnauthorized
	}
	if strings.Contains(combined, "socket not found at") || strings.Contains(combined, "connection refused") || strings.Contains(combined, "broken pipe") || strings.Contains(combined, "connection reset") || strings.Contains(combined, "timed out") {
		return ErrorUnavailable
	}
	return ErrorUnavailable
}

func validateCmuxEndpoint(endpoint Endpoint) error {
	return CmuxClient{}.ValidateEndpoint(endpoint)
}

var cmuxVersionPattern = regexp.MustCompile(`^cmux [0-9]+\.[0-9]+\.[0-9]+(?: \([0-9]+\))?(?: \[[0-9A-Fa-f]+\])?$`)

func cmuxUUID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil && len(value) == 36
}
