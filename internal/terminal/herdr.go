package terminal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const RequiredProtocol = 17

type Runner interface {
	Run(socketPath string, args ...string) ([]byte, []byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(socketPath string, args ...string) ([]byte, []byte, error) {
	cmd := exec.Command("herdr", args...)
	env := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "HERDR_SOCKET_PATH=") {
			env = append(env, value)
		}
	}
	if socketPath != "" {
		env = append(env, "HERDR_SOCKET_PATH="+socketPath)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type Client struct {
	SocketPath  string
	Runner      Runner
	MinProtocol int
}

func New(socketPath string) Client {
	return Client{SocketPath: socketPath, Runner: ExecRunner{}, MinProtocol: RequiredProtocol}
}

func NewWithRunner(socketPath string, runner Runner) Client {
	return Client{SocketPath: socketPath, Runner: runner, MinProtocol: RequiredProtocol}
}

type TabSpec struct {
	WorkspaceID string
	CWD         string
	Label       string
	Harness     string
	Source      string
	Generation  int
	Environment []string
}

type Tab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	AgentStatus string `json:"agent_status"`
	PaneCount   int    `json:"pane_count"`
	Focused     bool   `json:"focused"`
}

type Pane struct {
	ID            string `json:"pane_id"`
	TabID         string `json:"tab_id"`
	WorkspaceID   string `json:"workspace_id"`
	CWD           string `json:"cwd"`
	ForegroundCWD string `json:"foreground_cwd"`
	AgentStatus   string `json:"agent_status"`
	Focused       bool   `json:"focused"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return "Herdr API error " + e.Code
	}
	return fmt.Sprintf("Herdr API error %s: %s", e.Code, e.Message)
}

func IsNotFound(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && strings.HasSuffix(apiErr.Code, "_not_found")
}

var herdrContextKeys = []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_WORKSPACE_ID", "HERDR_PANE_ID"}

func (c Client) ContextKeys() []string {
	return append([]string(nil), herdrContextKeys...)
}

func (c Client) DetectParent(context ParentContext) Detection {
	keys := c.ContextKeys()
	present := false
	for _, key := range keys {
		present = present || context.Value(key) != "" || context.InvalidKeys[key]
		if context.InvalidKeys[key] {
			return Detection{Provider: c.Backend(), State: DetectionInvalid, Err: fmt.Errorf("Herdr parent context is oversized")}
		}
	}
	if !present {
		return Detection{Provider: c.Backend(), State: DetectionAbsent}
	}
	parent, diagnostics, err := c.validateParent(func(key string) string { return context.Value(key) })
	if err != nil {
		return Detection{Provider: c.Backend(), State: DetectionInvalid, Err: err}
	}
	return Detection{Provider: c.Backend(), State: DetectionMatched, Parent: parent, Diagnostics: diagnostics}
}

func (c Client) ValidateEndpoint(endpoint Endpoint) error {
	if endpoint.Backend != c.Backend() || endpoint.SocketPath == "" || endpoint.WorkspaceID == "" || endpoint.TabID == "" || endpoint.PaneID == "" || endpoint.WindowID != "" || endpoint.SurfaceID != "" {
		return fmt.Errorf("Herdr endpoint identity is incomplete")
	}
	return nil
}

func (c Client) ValidateParent() (Parent, error) {
	parent, _, err := c.validateParent(os.Getenv)
	return parent, err
}

func (c Client) validateParent(get func(string) string) (Parent, Diagnostics, error) {
	if get("HERDR_ENV") != "1" {
		return Parent{}, Diagnostics{}, fmt.Errorf("Herdr runtime requires HERDR_ENV=1")
	}
	parent := Parent{
		Backend:     "herdr",
		SocketPath:  get("HERDR_SOCKET_PATH"),
		WorkspaceID: get("HERDR_WORKSPACE_ID"),
		PaneID:      get("HERDR_PANE_ID"),
	}
	if parent.SocketPath == "" || parent.WorkspaceID == "" || parent.PaneID == "" {
		return Parent{}, Diagnostics{}, fmt.Errorf("Herdr runtime requires HERDR_SOCKET_PATH, HERDR_WORKSPACE_ID, and HERDR_PANE_ID")
	}
	if c.SocketPath == "" {
		c.SocketPath = parent.SocketPath
	}
	if c.SocketPath != parent.SocketPath {
		return Parent{}, Diagnostics{}, fmt.Errorf("Herdr socket path does not match HERDR_SOCKET_PATH")
	}
	diagnostics, err := c.validateProtocol()
	if err != nil {
		return Parent{}, Diagnostics{}, err
	}
	pane, err := c.GetPane(Endpoint{SocketPath: parent.SocketPath, WorkspaceID: parent.WorkspaceID, PaneID: parent.PaneID})
	if err != nil {
		return Parent{}, Diagnostics{}, fmt.Errorf("validate Herdr parent pane: %w", err)
	}
	if pane.WorkspaceID != parent.WorkspaceID || pane.ID != parent.PaneID || pane.TabID == "" {
		return Parent{}, Diagnostics{}, fmt.Errorf("Herdr parent pane identity is inconsistent")
	}
	tab, err := c.GetTab(Endpoint{SocketPath: parent.SocketPath, WorkspaceID: parent.WorkspaceID, TabID: pane.TabID})
	if err != nil {
		return Parent{}, Diagnostics{}, fmt.Errorf("validate Herdr parent tab: %w", err)
	}
	if tab.ID != pane.TabID || tab.WorkspaceID != parent.WorkspaceID {
		return Parent{}, Diagnostics{}, fmt.Errorf("Herdr parent tab is in a different workspace")
	}
	parent.TabID = pane.TabID
	return parent, diagnostics, nil
}

func (c Client) Backend() string {
	return "herdr"
}

func (c Client) WithSocket(socketPath string) RuntimeClient {
	c.SocketPath = socketPath
	return c
}

func (c Client) Validate() error {
	_, err := c.Diagnostics()
	return err
}

func (c Client) Diagnostics() (Diagnostics, error) {
	return c.validateProtocol()
}

func (c Client) ValidateProtocol() error {
	return c.Validate()
}

func (c Client) validateProtocol() (Diagnostics, error) {
	output, stderr, err := c.run("status", "server", "--json")
	if err != nil {
		return Diagnostics{}, &RuntimeError{Kind: ErrorUnavailable, Operation: "validate Herdr protocol", Cause: fmt.Errorf("%s: %w", strings.TrimSpace(string(stderr)), err)}
	}
	var status struct {
		Running            bool   `json:"running"`
		Version            string `json:"version"`
		Protocol           *int   `json:"protocol"`
		Compatible         bool   `json:"compatible"`
		EndpointCompatible *bool  `json:"endpoint_compatible"`
		Socket             string `json:"socket"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return Diagnostics{}, &RuntimeError{Kind: ErrorMalformed, Operation: "validate Herdr protocol", Cause: fmt.Errorf("invalid server status JSON: %w", err)}
	}
	if status.Protocol == nil {
		return Diagnostics{}, &RuntimeError{Kind: ErrorMalformed, Operation: "validate Herdr protocol", Cause: fmt.Errorf("server status is missing protocol")}
	}
	if *status.Protocol < c.protocolFloor() {
		return Diagnostics{}, &RuntimeError{Kind: ErrorIncompatible, Operation: "validate Herdr protocol", Cause: fmt.Errorf("protocol %d is unsupported; protocol %d or newer is required", *status.Protocol, c.protocolFloor())}
	}
	if !status.Running {
		return Diagnostics{}, &RuntimeError{Kind: ErrorUnavailable, Operation: "validate Herdr protocol", Cause: fmt.Errorf("server is not running")}
	}
	if !status.Compatible || status.EndpointCompatible != nil && !*status.EndpointCompatible {
		return Diagnostics{}, &RuntimeError{Kind: ErrorIncompatible, Operation: "validate Herdr protocol", Cause: fmt.Errorf("server does not confirm client protocol compatibility")}
	}
	if status.Socket != c.SocketPath {
		return Diagnostics{}, &RuntimeError{Kind: ErrorMalformed, Operation: "validate Herdr protocol", Cause: fmt.Errorf("socket identity mismatch")}
	}
	return Diagnostics{ProviderVersion: status.Version, ProtocolVersion: strconv.Itoa(*status.Protocol), Capabilities: []string{"exact-pane-lifecycle", "process-info", "state-report"}}, nil
}

func (c Client) CreateTab(spec TabSpec) (Endpoint, error) {
	if spec.WorkspaceID == "" || spec.CWD == "" || spec.Label == "" {
		return Endpoint{}, fmt.Errorf("Herdr tab creation requires workspace, cwd, and label")
	}
	if strings.IndexFunc(spec.Label, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
		return Endpoint{}, fmt.Errorf("Herdr label contains control characters")
	}
	args := []string{"tab", "create", "--workspace", spec.WorkspaceID, "--cwd", spec.CWD, "--label", spec.Label, "--no-focus"}
	for _, environment := range spec.Environment {
		if strings.ContainsAny(environment, "\r\n") || !strings.Contains(environment, "=") {
			return Endpoint{}, fmt.Errorf("invalid Herdr tab environment entry")
		}
		args = append(args, "--env", environment)
	}
	var response struct {
		Result struct {
			RootPane Pane `json:"root_pane"`
			Tab      Tab  `json:"tab"`
		} `json:"result"`
	}
	if err := c.runJSON(&response, args...); err != nil {
		return Endpoint{}, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: fmt.Errorf("create Herdr tab: %w", err)}
	}
	endpoint := Endpoint{Backend: "herdr", SocketPath: c.SocketPath, WorkspaceID: spec.WorkspaceID, TabID: response.Result.Tab.ID, PaneID: response.Result.RootPane.ID}
	if response.Result.Tab.ID == "" || response.Result.RootPane.ID == "" {
		return Endpoint{}, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: fmt.Errorf("Herdr tab create returned incomplete endpoint")}
	}
	if response.Result.Tab.WorkspaceID != spec.WorkspaceID || response.Result.RootPane.WorkspaceID != spec.WorkspaceID || response.Result.RootPane.TabID != response.Result.Tab.ID {
		return endpoint, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: fmt.Errorf("Herdr tab create returned a cross-workspace or inconsistent endpoint")}
	}
	if response.Result.Tab.Focused || response.Result.RootPane.Focused {
		return endpoint, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: fmt.Errorf("Herdr tab create unexpectedly changed focus")}
	}
	if response.Result.RootPane.CWD != "" && !samePath(response.Result.RootPane.CWD, spec.CWD) {
		return endpoint, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: fmt.Errorf("Herdr tab create returned cwd %q, expected %q", response.Result.RootPane.CWD, spec.CWD)}
	}
	if err := c.ReportMetadata(endpoint, spec.Label, spec.Harness, spec.Source, "working"); err != nil {
		if closeErr := c.CloseExactPane(endpoint); closeErr != nil {
			return endpoint, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: errors.Join(err, fmt.Errorf("exact Herdr cleanup is uncertain: %w", closeErr))}
		}
		return Endpoint{}, err
	}
	pane, err := c.GetPane(endpoint)
	if err != nil {
		if !IsNotFound(err) {
			if closeErr := c.CloseExactPane(endpoint); closeErr != nil {
				return endpoint, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: errors.Join(err, closeErr)}
			}
		}
		return Endpoint{}, fmt.Errorf("verify Herdr tab pane: %w", err)
	}
	if pane.ID != endpoint.PaneID || pane.TabID != endpoint.TabID || pane.WorkspaceID != endpoint.WorkspaceID {
		if closeErr := c.CloseExactPane(endpoint); closeErr != nil {
			return endpoint, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: errors.Join(fmt.Errorf("Herdr tab pane identity changed during creation"), closeErr)}
		}
		return Endpoint{}, fmt.Errorf("Herdr tab pane identity changed during creation")
	}
	return endpoint, nil
}

func (c Client) CreateWorkspace(spec WorkspaceSpec) (Endpoint, error) {
	return c.CreateTab(TabSpec{WorkspaceID: spec.WorkspaceID, CWD: spec.CWD, Label: spec.Label, Harness: spec.Harness, Source: spec.Source, Generation: spec.Generation, Environment: spec.Environment})
}

func (c Client) Start(endpoint Endpoint, command string) error {
	return c.Run(endpoint, command)
}

func (c Client) Run(endpoint Endpoint, command string) error {
	if command == "" || strings.ContainsAny(command, "\r\n") {
		return fmt.Errorf("Herdr pane command must be a single line")
	}
	if _, err := c.checkedPane(endpoint); err != nil {
		return err
	}
	if _, stderr, err := c.run("pane", "run", endpoint.PaneID, command); err != nil {
		return fmt.Errorf("run command in Herdr pane: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	return nil
}

func (c Client) GetPane(endpoint Endpoint) (Pane, error) {
	pane, err := c.getPane(endpoint)
	if err != nil {
		return Pane{}, err
	}
	if pane.ID != endpoint.PaneID || pane.WorkspaceID != endpoint.WorkspaceID || pane.TabID != endpoint.TabID && endpoint.TabID != "" {
		return Pane{}, fmt.Errorf("Herdr pane identity mismatch: got workspace=%s tab=%s pane=%s", pane.WorkspaceID, pane.TabID, pane.ID)
	}
	return pane, nil
}

func (c Client) GetTab(endpoint Endpoint) (Tab, error) {
	if endpoint.TabID == "" {
		return Tab{}, fmt.Errorf("Herdr tab ID is empty")
	}
	var response struct {
		Result struct {
			Tab Tab `json:"tab"`
		} `json:"result"`
	}
	if err := c.runJSON(&response, "tab", "get", endpoint.TabID); err != nil {
		return Tab{}, fmt.Errorf("get Herdr tab: %w", err)
	}
	if response.Result.Tab.ID != endpoint.TabID || response.Result.Tab.WorkspaceID != endpoint.WorkspaceID {
		return Tab{}, fmt.Errorf("Herdr tab identity mismatch: got workspace=%s tab=%s", response.Result.Tab.WorkspaceID, response.Result.Tab.ID)
	}
	return response.Result.Tab, nil
}

func (c Client) Inspect(endpoint Endpoint) (EndpointState, error) {
	pane, err := c.GetPane(endpoint)
	if err != nil {
		return EndpointState{}, err
	}
	tab, err := c.GetTab(endpoint)
	if err != nil {
		return EndpointState{}, err
	}
	return EndpointState{Endpoint: endpoint, Label: tab.Label, Focused: pane.Focused || tab.Focused, FullyFocused: pane.Focused || tab.Focused, PaneCount: tab.PaneCount, SurfaceCount: 1}, nil
}

func (c Client) ProcessInfo(endpoint Endpoint) (ProcessInfo, error) {
	if _, err := c.checkedPane(endpoint); err != nil {
		return ProcessInfo{}, err
	}
	var response struct {
		Result struct {
			ProcessInfo ProcessInfo `json:"process_info"`
		} `json:"result"`
	}
	if err := c.runJSON(&response, "pane", "process-info", "--pane", endpoint.PaneID); err != nil {
		return ProcessInfo{}, fmt.Errorf("get Herdr pane process info: %w", err)
	}
	info := response.Result.ProcessInfo
	if info.PaneID != endpoint.PaneID {
		return ProcessInfo{}, fmt.Errorf("Herdr process info pane mismatch")
	}
	return info, nil
}

func (c Client) Read(endpoint Endpoint, lines int) (string, error) {
	return c.ReadPane(endpoint, lines)
}

func (c Client) ReadPane(endpoint Endpoint, lines int) (string, error) {
	if _, err := c.checkedPane(endpoint); err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 200
	}
	output, stderr, err := c.run("pane", "read", endpoint.PaneID, "--source", "recent-unwrapped", "--lines", strconv.Itoa(lines), "--format", "text")
	if apiErr := responseError(output); apiErr != nil {
		return "", apiErr
	}
	if apiErr := responseError(stderr); apiErr != nil {
		return "", apiErr
	}
	if err != nil {
		return "", fmt.Errorf("read Herdr pane: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	return string(output), nil
}

func (c Client) Focus(endpoint Endpoint) error {
	if _, err := c.checkedPane(endpoint); err != nil {
		return err
	}
	if _, err := c.GetTab(endpoint); err != nil {
		return err
	}
	if _, stderr, err := c.run("workspace", "focus", endpoint.WorkspaceID); err != nil {
		return fmt.Errorf("focus Herdr workspace: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	if _, stderr, err := c.run("tab", "focus", endpoint.TabID); err != nil {
		return fmt.Errorf("focus Herdr tab: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	return nil
}

func (c Client) Close(endpoint Endpoint) error {
	return c.CloseExactPane(endpoint)
}

func (c Client) CloseExactPane(endpoint Endpoint) error {
	pane, err := c.GetPane(endpoint)
	if err != nil {
		return err
	}
	tab, err := c.GetTab(endpoint)
	if err != nil {
		return err
	}
	if pane.TabID != tab.ID || pane.WorkspaceID != tab.WorkspaceID || tab.PaneCount != 1 {
		return fmt.Errorf("refusing to close Herdr pane with inconsistent identity or pane count")
	}
	if _, stderr, err := c.run("pane", "close", endpoint.PaneID); err != nil {
		return fmt.Errorf("close Herdr pane: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	if _, err := c.getPane(endpoint); err == nil {
		return fmt.Errorf("Herdr pane %s still exists after close", endpoint.PaneID)
	} else if !IsNotFound(err) {
		return fmt.Errorf("verify Herdr pane close: %w", err)
	}
	return nil
}

func (c Client) ReportAgent(endpoint Endpoint, report AgentReport) error {
	if err := validateAgentReport(report); err != nil {
		return err
	}
	if _, err := c.checkedPane(endpoint); err != nil {
		return err
	}
	args := []string{"pane", "report-agent", endpoint.PaneID, "--source", report.Source, "--agent", report.Agent, "--state", report.State, "--seq", strconv.FormatUint(report.Sequence, 10)}
	if report.SessionID != "" {
		args = append(args, "--agent-session-id", report.SessionID)
	}
	if _, stderr, err := c.run(args...); err != nil {
		return fmt.Errorf("publish Herdr agent lifecycle: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	return nil
}

func (c Client) ReleaseAgent(endpoint Endpoint, source, agent string, sequence uint64) error {
	if err := validateAgentReport(AgentReport{Source: source, Agent: agent, State: "idle", Sequence: sequence}); err != nil {
		return err
	}
	if _, err := c.checkedPane(endpoint); err != nil {
		return err
	}
	args := []string{"pane", "release-agent", endpoint.PaneID, "--source", source, "--agent", agent, "--seq", strconv.FormatUint(sequence, 10)}
	if _, stderr, err := c.run(args...); err != nil {
		return fmt.Errorf("release Herdr agent lifecycle: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	return nil
}

func (c Client) ReportMetadata(endpoint Endpoint, label, harness, source, state string) error {
	if strings.IndexFunc(label, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
		return fmt.Errorf("Herdr metadata label contains control characters")
	}
	if _, err := c.checkedPane(endpoint); err != nil {
		return err
	}
	stateLabel := state
	switch stateLabel {
	case "idle", "working", "blocked", "unknown":
	default:
		stateLabel = "idle"
	}
	args := []string{"pane", "report-metadata", endpoint.PaneID, "--source", source, "--agent", harness,
		"--title", label, "--display-agent", label, "--state-label", stateLabel + "=" + state}
	if _, stderr, err := c.run(args...); err != nil {
		return fmt.Errorf("publish Herdr pane metadata: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	return nil
}

func (c Client) RenamePane(endpoint Endpoint, label string) error {
	if _, err := c.checkedPane(endpoint); err != nil {
		return err
	}
	if _, stderr, err := c.run("pane", "rename", endpoint.PaneID, label); err != nil {
		return fmt.Errorf("name Herdr pane: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	return nil
}

func (c Client) getPane(endpoint Endpoint) (Pane, error) {
	if endpoint.PaneID == "" {
		return Pane{}, fmt.Errorf("Herdr pane ID is empty")
	}
	var response struct {
		Result struct {
			Pane Pane `json:"pane"`
		} `json:"result"`
	}
	if err := c.runJSON(&response, "pane", "get", endpoint.PaneID); err != nil {
		return Pane{}, fmt.Errorf("get Herdr pane: %w", err)
	}
	return response.Result.Pane, nil
}

func (c Client) checkedPane(endpoint Endpoint) (Pane, error) {
	pane, err := c.GetPane(endpoint)
	if err != nil {
		return Pane{}, err
	}
	if endpoint.TabID == "" || endpoint.WorkspaceID == "" {
		return Pane{}, fmt.Errorf("Herdr endpoint identity is incomplete")
	}
	return pane, nil
}

func (c Client) protocolFloor() int {
	if c.MinProtocol > 0 {
		return c.MinProtocol
	}
	return RequiredProtocol
}

func (c Client) run(args ...string) ([]byte, []byte, error) {
	if c.SocketPath == "" {
		return nil, nil, fmt.Errorf("Herdr socket path is empty")
	}
	if c.Runner == nil {
		return nil, nil, fmt.Errorf("Herdr command runner is nil")
	}
	return c.Runner.Run(c.SocketPath, args...)
}

func (c Client) runJSON(value any, args ...string) error {
	output, stderr, err := c.run(args...)
	if apiErr := responseError(output); apiErr != nil {
		return apiErr
	}
	if apiErr := responseError(stderr); apiErr != nil {
		return apiErr
	}
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(stderr)), err)
	}
	if err := json.Unmarshal(output, value); err != nil {
		return fmt.Errorf("invalid Herdr JSON: %w", err)
	}
	return nil
}

func responseError(output []byte) error {
	var response struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(output, &response) == nil && response.Error != nil {
		return &APIError{Code: response.Error.Code, Message: response.Error.Message}
	}
	return nil
}

var agentSourcePattern = regexp.MustCompile(`^[A-Za-z0-9:._-]+$`)

func validateAgentReport(report AgentReport) error {
	if report.Source == "" || len(report.Source) > 80 || !agentSourcePattern.MatchString(report.Source) {
		return fmt.Errorf("terminal agent source is invalid")
	}
	if report.Agent == "" || strings.IndexFunc(report.Agent, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
		return fmt.Errorf("terminal agent label is invalid")
	}
	switch report.State {
	case "idle", "working", "blocked", "unknown":
	default:
		return fmt.Errorf("terminal agent state is invalid")
	}
	if report.Sequence == 0 {
		return fmt.Errorf("terminal agent sequence must be positive")
	}
	return nil
}

func samePath(first, second string) bool {
	firstResolved, firstErr := filepath.EvalSymlinks(first)
	secondResolved, secondErr := filepath.EvalSymlinks(second)
	if firstErr == nil && secondErr == nil {
		return firstResolved == secondResolved
	}
	firstAbs, firstErr := filepath.Abs(first)
	secondAbs, secondErr := filepath.Abs(second)
	return firstErr == nil && secondErr == nil && filepath.Clean(firstAbs) == filepath.Clean(secondAbs)
}
