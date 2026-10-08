package terminal

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	extensionhost "shephrd/internal/extension"
)

const terminalCapabilityVersion = 1

var terminalCapabilityOperations = []string{
	"detect",
	"diagnostics",
	"create",
	"start",
	"inspect",
	"process_info",
	"read",
	"focus",
	"close",
	"report_agent",
	"release_agent",
	"report_metadata",
}

type terminalExtensionSpec struct {
	backend            string
	extensionID        string
	extensionVersion   string
	capability         string
	contextKeys        []string
	allowedEnvironment []string
	ownedCleanup       bool
	validateEndpoint   func(Endpoint) error
	validatePrepared   func(Endpoint, WorkspaceSpec) error
}

func terminalExtensionCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) <= 128 {
			return setting.Value
		}
	}
	return ""
}

func terminalExtensionCapability(name string) extensionhost.Capability {
	return extensionhost.Capability{Name: name, Version: terminalCapabilityVersion, Operations: append([]string(nil), terminalCapabilityOperations...)}
}

func terminalExtensionManifest(spec terminalExtensionSpec) extensionhost.Manifest {
	return extensionhost.Manifest{
		Wire:         extensionhost.WireRange{Major: extensionhost.WireMajor, MinorMin: extensionhost.WireMinor, MinorMax: extensionhost.WireMinor},
		Extension:    extensionhost.Identity{ID: spec.extensionID, Version: spec.extensionVersion, BuildCommit: terminalExtensionCommit()},
		Capabilities: []extensionhost.Capability{terminalExtensionCapability(spec.capability)},
	}
}

type terminalExtensionProvider struct {
	host       *extensionhost.Host
	spec       terminalExtensionSpec
	socketPath string
}

func newTerminalExtensionProvider(spec terminalExtensionSpec, command []string, sha256 string, environment []string) *terminalExtensionProvider {
	host := extensionhost.NewHost(extensionhost.HostConfig{
		Command:              command,
		SHA256:               sha256,
		ParentEnvironment:    environment,
		AllowedEnvironment:   append([]string(nil), spec.allowedEnvironment...),
		ExpectedID:           spec.extensionID,
		ExpectedCapabilities: []extensionhost.Capability{terminalExtensionCapability(spec.capability)},
	})
	return &terminalExtensionProvider{host: host, spec: spec}
}

func (p *terminalExtensionProvider) Backend() string {
	return p.spec.backend
}

func (p *terminalExtensionProvider) WithSocket(socketPath string) RuntimeClient {
	copy := *p
	copy.socketPath = socketPath
	return &copy
}

func (p *terminalExtensionProvider) ContextKeys() []string {
	return append([]string(nil), p.spec.contextKeys...)
}

func (p *terminalExtensionProvider) Detect(parentContext ParentContext) Detection {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result extensionDetectResult
	if err := p.host.Invoke(ctx, p.spec.capability, terminalCapabilityVersion, "detect", extensionDetectRequest{Context: parentContext}, &result); err != nil {
		return Detection{Provider: p.Backend(), State: DetectionInvalid, Err: extensionRuntimeError("detect "+p.Backend()+" parent", err)}
	}
	return Detection{Provider: p.Backend(), State: result.State, Parent: result.Parent, Diagnostics: result.Diagnostics}
}

func (p *terminalExtensionProvider) OwnedCleanup() bool {
	return p.spec.ownedCleanup
}

func (p *terminalExtensionProvider) ValidateEndpoint(endpoint Endpoint) error {
	return p.spec.validateEndpoint(endpoint)
}

func (p *terminalExtensionProvider) Validate() error {
	_, err := p.Diagnostics()
	return err
}

func (p *terminalExtensionProvider) Diagnostics() (Diagnostics, error) {
	var result extensionDiagnosticsResult
	if err := p.invoke(5*time.Second, "diagnostics", extensionSocketRequest{SocketPath: p.socketPath}, &result); err != nil {
		return Diagnostics{}, err
	}
	if err := validateDiagnostics(result.Diagnostics); err != nil {
		return Diagnostics{}, &RuntimeError{Kind: ErrorProtocolViolation, Operation: "diagnostics", Cause: err}
	}
	return result.Diagnostics, nil
}

func (p *terminalExtensionProvider) PrepareWorkspace(spec WorkspaceSpec) (PreparedWorkspace, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	invocation, err := p.host.Begin(ctx, p.spec.capability, terminalCapabilityVersion, "create", extensionCreateRequest{SocketPath: p.socketPath, Spec: spec})
	if err != nil {
		cancel()
		return nil, extensionRuntimeError("create "+p.Backend()+" workspace", err)
	}
	var result extensionEndpointResult
	if _, err := invocation.Receive(&result, "prepared"); err != nil {
		var remote *extensionhost.RemoteError
		if errors.As(err, &remote) {
			cancelErr := invocation.Cancel()
			cancel()
			return nil, extensionRuntimeError("create "+p.Backend()+" workspace", errors.Join(err, cancelErr))
		}
		abortErr := invocation.Abort()
		cancel()
		createErr := extensionRuntimeError("create "+p.Backend()+" workspace", errors.Join(err, abortErr))
		if abortErr != nil {
			return nil, &CreateSideEffect{Effect: CreateEffectUnknown, Cause: createErr}
		}
		return nil, createErr
	}
	validationErr := p.ValidateEndpoint(result.Endpoint)
	if validationErr == nil && result.Endpoint.SocketPath != p.socketPath {
		validationErr = fmt.Errorf("extension returned a cross-parent %s endpoint", p.Backend())
	}
	if validationErr == nil {
		validationErr = p.spec.validatePrepared(result.Endpoint, spec)
	}
	prepared := &terminalPreparedWorkspace{invocation: invocation, endpoint: result.Endpoint, cancel: cancel, backend: p.Backend()}
	if validationErr != nil {
		if abortErr := prepared.Abort(); abortErr != nil {
			return prepared, errors.Join(validationErr, fmt.Errorf("exact %s preparation cleanup is uncertain: %w", p.Backend(), abortErr))
		}
		return nil, validationErr
	}
	return prepared, nil
}

func (p *terminalExtensionProvider) CreateWorkspace(spec WorkspaceSpec) (Endpoint, error) {
	prepared, err := p.PrepareWorkspace(spec)
	if err != nil {
		return Endpoint{}, err
	}
	if err := prepared.Commit(); err != nil {
		return prepared.Endpoint(), err
	}
	return prepared.Endpoint(), nil
}

func (p *terminalExtensionProvider) Start(endpoint Endpoint, command string) error {
	return p.invoke(15*time.Second, "start", extensionStartRequest{Endpoint: endpoint, Command: command}, &extensionUnit{})
}

func (p *terminalExtensionProvider) Inspect(endpoint Endpoint) (EndpointState, error) {
	var result extensionStateResult
	if err := p.invoke(5*time.Second, "inspect", extensionEndpointRequest{Endpoint: endpoint}, &result); err != nil {
		return EndpointState{}, err
	}
	if p.ValidateEndpoint(result.State.Endpoint) != nil || !sameEndpointIdentity(endpoint, result.State.Endpoint) || result.State.PaneCount != 1 || result.State.SurfaceCount != 1 || len(result.State.Label) > 4096 || strings.IndexFunc(result.State.Label, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
		return EndpointState{}, &RuntimeError{Kind: ErrorProtocolViolation, Operation: "inspect", Cause: fmt.Errorf("extension returned invalid endpoint state")}
	}
	return result.State, nil
}

func (p *terminalExtensionProvider) ProcessInfo(endpoint Endpoint) (ProcessInfo, error) {
	var result extensionProcessResult
	if err := p.invoke(5*time.Second, "process_info", extensionEndpointRequest{Endpoint: endpoint}, &result); err != nil {
		return ProcessInfo{}, err
	}
	info := result.ProcessInfo
	if info.PaneID != endpoint.PaneID || info.ShellPID < 0 || info.ForegroundProcessGroup < 0 || len(info.ForegroundProcesses) > 256 {
		return ProcessInfo{}, &RuntimeError{Kind: ErrorProtocolViolation, Operation: "process_info", Cause: fmt.Errorf("extension returned invalid process attribution")}
	}
	seen := make(map[int]bool, len(info.ForegroundProcesses))
	for _, process := range info.ForegroundProcesses {
		if process.PID <= 0 || process.PGID < 0 || seen[process.PID] || len(process.Name) > 512 || len(process.Path) > 4096 || len(process.Cmdline) > 16384 || len(process.Argv) > 256 {
			return ProcessInfo{}, &RuntimeError{Kind: ErrorProtocolViolation, Operation: "process_info", Cause: fmt.Errorf("extension returned invalid process attribution")}
		}
		seen[process.PID] = true
	}
	return info, nil
}

func (p *terminalExtensionProvider) Read(endpoint Endpoint, lines int) (string, error) {
	var result extensionReadResult
	if err := p.invoke(10*time.Second, "read", extensionReadRequest{Endpoint: endpoint, Lines: lines}, &result); err != nil {
		return "", err
	}
	if len(result.Text) > extensionhost.MaxFrameBytes {
		return "", &RuntimeError{Kind: ErrorProtocolViolation, Operation: "read", Cause: fmt.Errorf("extension returned oversized terminal output")}
	}
	return result.Text, nil
}

func (p *terminalExtensionProvider) Focus(endpoint Endpoint) error {
	return p.invoke(5*time.Second, "focus", extensionEndpointRequest{Endpoint: endpoint}, &extensionUnit{})
}

func (p *terminalExtensionProvider) Close(endpoint Endpoint) error {
	return p.invoke(15*time.Second, "close", extensionEndpointRequest{Endpoint: endpoint}, &extensionUnit{})
}

func (p *terminalExtensionProvider) ReportAgent(endpoint Endpoint, report AgentReport) error {
	return p.invoke(5*time.Second, "report_agent", extensionAgentRequest{Endpoint: endpoint, Report: report}, &extensionUnit{})
}

func (p *terminalExtensionProvider) ReleaseAgent(endpoint Endpoint, source, agent string, sequence uint64) error {
	return p.invoke(5*time.Second, "release_agent", extensionReleaseAgentRequest{Endpoint: endpoint, Source: source, Agent: agent, Sequence: sequence}, &extensionUnit{})
}

func (p *terminalExtensionProvider) ReportMetadata(endpoint Endpoint, label, harness, source, state string) error {
	return p.invoke(5*time.Second, "report_metadata", extensionMetadataRequest{Endpoint: endpoint, Label: label, Harness: harness, Source: source, State: state}, &extensionUnit{})
}

func (p *terminalExtensionProvider) invoke(timeout time.Duration, operation string, payload, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return extensionRuntimeError(operation, p.host.Invoke(ctx, p.spec.capability, terminalCapabilityVersion, operation, payload, result))
}

func sameEndpointIdentity(first, second Endpoint) bool {
	return first.Backend == second.Backend && first.SocketPath == second.SocketPath && first.WindowID == second.WindowID && first.WorkspaceID == second.WorkspaceID && first.TabID == second.TabID && first.PaneID == second.PaneID && first.SurfaceID == second.SurfaceID
}

type terminalPreparedWorkspace struct {
	invocation *extensionhost.Invocation
	endpoint   Endpoint
	cancel     context.CancelFunc
	backend    string
	finished   bool
}

func (p *terminalPreparedWorkspace) Endpoint() Endpoint {
	return p.endpoint
}

func (p *terminalPreparedWorkspace) Commit() error {
	if p.finished {
		return fmt.Errorf("%s workspace preparation is already finished", p.backend)
	}
	p.finished = true
	defer p.cancel()
	if err := p.invocation.SendControl("commit"); err != nil {
		return extensionRuntimeError("commit "+p.backend+" workspace", errors.Join(err, p.invocation.Cancel()))
	}
	var result extensionUnit
	if _, err := p.invocation.Receive(&result, "ok"); err != nil {
		return extensionRuntimeError("commit "+p.backend+" workspace", errors.Join(err, p.invocation.Cancel()))
	}
	return extensionRuntimeError("commit "+p.backend+" workspace", p.invocation.Finish())
}

func (p *terminalPreparedWorkspace) Abort() error {
	if p.finished {
		return nil
	}
	p.finished = true
	defer p.cancel()
	if err := p.invocation.Abort(); err != nil {
		return &CreateSideEffect{Effect: CreateEffectUnknown, Cause: extensionRuntimeError("abort "+p.backend+" workspace", err)}
	}
	return nil
}

func extensionRuntimeError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var remote *extensionhost.RemoteError
	if errors.As(err, &remote) {
		switch remote.Failure.Class {
		case ErrorEndpointAbsent, ErrorUnavailable, ErrorUnauthorized, ErrorIncompatible, ErrorMalformed, ErrorAmbiguous:
			return &RuntimeError{Kind: remote.Failure.Class, Operation: operation, Cause: err}
		default:
			return &RuntimeError{Kind: ErrorProtocolViolation, Operation: operation, Cause: fmt.Errorf("extension returned an unknown error class")}
		}
	}
	var host *extensionhost.HostError
	if errors.As(err, &host) {
		kind := map[string]string{
			extensionhost.ErrorDeadlineExceeded:  ErrorDeadlineExceeded,
			extensionhost.ErrorCancelled:         ErrorCancelled,
			extensionhost.ErrorExtensionExit:     ErrorExtensionExit,
			extensionhost.ErrorProtocolViolation: ErrorProtocolViolation,
			extensionhost.ErrorAbortUncertain:    ErrorAbortUncertain,
		}[host.Kind]
		if kind == "" {
			kind = ErrorProtocolViolation
		}
		return &RuntimeError{Kind: kind, Operation: operation, Cause: err}
	}
	return err
}

type extensionDetectRequest struct {
	Context ParentContext `json:"context"`
}

type extensionDetectResult struct {
	State       string      `json:"state"`
	Parent      Parent      `json:"parent"`
	Diagnostics Diagnostics `json:"diagnostics"`
}

type extensionSocketRequest struct {
	SocketPath string `json:"socket_path"`
}

type extensionDiagnosticsResult struct {
	Diagnostics Diagnostics `json:"diagnostics"`
}

type extensionCreateRequest struct {
	SocketPath string        `json:"socket_path"`
	Spec       WorkspaceSpec `json:"spec"`
}

type extensionEndpointRequest struct {
	Endpoint Endpoint `json:"endpoint"`
}

type extensionEndpointResult struct {
	Endpoint Endpoint `json:"endpoint"`
}

type extensionStartRequest struct {
	Endpoint Endpoint `json:"endpoint"`
	Command  string   `json:"command"`
}

type extensionStateResult struct {
	State EndpointState `json:"state"`
}

type extensionProcessResult struct {
	ProcessInfo ProcessInfo `json:"process_info"`
}

type extensionReadRequest struct {
	Endpoint Endpoint `json:"endpoint"`
	Lines    int      `json:"lines"`
}

type extensionReadResult struct {
	Text string `json:"text"`
}

type extensionAgentRequest struct {
	Endpoint Endpoint    `json:"endpoint"`
	Report   AgentReport `json:"report"`
}

type extensionReleaseAgentRequest struct {
	Endpoint Endpoint `json:"endpoint"`
	Source   string   `json:"source"`
	Agent    string   `json:"agent"`
	Sequence uint64   `json:"sequence"`
}

type extensionMetadataRequest struct {
	Endpoint Endpoint `json:"endpoint"`
	Label    string   `json:"label"`
	Harness  string   `json:"harness"`
	Source   string   `json:"source"`
	State    string   `json:"state"`
}

type extensionUnit struct{}
