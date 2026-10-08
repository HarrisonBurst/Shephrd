package terminal

import (
	"errors"
	"fmt"

	extensionhost "shephrd/internal/extension"
)

var ErrNotFound = errors.New("terminal endpoint not found")

const (
	ErrorEndpointAbsent    = "endpoint_absent"
	ErrorUnavailable       = "unavailable"
	ErrorUnauthorized      = "unauthorized"
	ErrorIncompatible      = "incompatible"
	ErrorMalformed         = "malformed"
	ErrorAmbiguous         = "ambiguous"
	ErrorDeadlineExceeded  = "deadline_exceeded"
	ErrorCancelled         = "cancelled"
	ErrorExtensionExit     = "extension_exit"
	ErrorProtocolViolation = "protocol_violation"
	ErrorAbortUncertain    = "abort_uncertain"
)

const (
	CreateEffectNone    = "none"
	CreateEffectUnknown = "unknown"
)

// CreateSideEffect marks a failed create with the durable outcome of its
// provider side effect: none means the side effect is proven absent, unknown
// means a terminal endpoint may exist without a durable identity.
type CreateSideEffect struct {
	Effect string
	Cause  error
}

func (e *CreateSideEffect) Error() string {
	if e.Cause == nil {
		return "terminal create side effect is " + e.Effect
	}
	return e.Cause.Error()
}

func (e *CreateSideEffect) Unwrap() error {
	return e.Cause
}

// CreateEffectFromError reports the durable outcome of a failed create: the
// provider-side effect is proven absent (none) or may exist (unknown).
func CreateEffectFromError(endpoint Endpoint, err error) string {
	if err == nil {
		return ""
	}
	var remote *extensionhost.RemoteError
	if errors.As(err, &remote) {
		return remote.Failure.Effect
	}
	effect := CreateEffectNone
	if endpoint.WindowID != "" || endpoint.WorkspaceID != "" || endpoint.TabID != "" || endpoint.PaneID != "" || endpoint.SurfaceID != "" {
		effect = CreateEffectUnknown
	}
	var sideEffect *CreateSideEffect
	if errors.As(err, &sideEffect) {
		effect = sideEffect.Effect
	}
	return effect
}

type RuntimeError struct {
	Kind      string
	Operation string
	Cause     error
}

func (e *RuntimeError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("terminal %s: %s", e.Operation, e.Kind)
	}
	return fmt.Sprintf("terminal %s: %s: %v", e.Operation, e.Kind, e.Cause)
}

func (e *RuntimeError) Unwrap() error {
	if e.Kind == ErrorEndpointAbsent {
		return ErrNotFound
	}
	return e.Cause
}

func (e *RuntimeError) ErrorKind() string {
	return "terminal_" + e.Kind
}

func Classify(err error) string {
	var runtimeErr *RuntimeError
	if errors.As(err, &runtimeErr) {
		return runtimeErr.Kind
	}
	if errors.Is(err, ErrNotFound) {
		return ErrorEndpointAbsent
	}
	return ""
}

type Parent struct {
	Backend     string `json:"backend"`
	SocketPath  string `json:"socket_path"`
	WindowID    string `json:"window_id,omitempty"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id,omitempty"`
	PaneID      string `json:"pane_id"`
	SurfaceID   string `json:"surface_id,omitempty"`
}

type Endpoint struct {
	Backend         string   `json:"backend"`
	SocketPath      string   `json:"socket_path"`
	WindowID        string   `json:"window_id,omitempty"`
	WorkspaceID     string   `json:"workspace_id"`
	TabID           string   `json:"tab_id,omitempty"`
	PaneID          string   `json:"pane_id"`
	SurfaceID       string   `json:"surface_id,omitempty"`
	ProviderVersion string   `json:"provider_version,omitempty"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

type AgentReport struct {
	Source    string `json:"source"`
	Agent     string `json:"agent"`
	State     string `json:"state"`
	Sequence  uint64 `json:"sequence"`
	SessionID string `json:"session_id,omitempty"`
}

type Process struct {
	PID     int      `json:"pid"`
	PGID    int      `json:"pgid,omitempty"`
	Name    string   `json:"name"`
	Path    string   `json:"path,omitempty"`
	Cmdline string   `json:"cmdline"`
	Argv    []string `json:"argv"`
}

type ProcessInfo struct {
	PaneID                 string    `json:"pane_id"`
	ShellPID               int       `json:"shell_pid"`
	ForegroundProcessGroup int       `json:"foreground_process_group_id"`
	ForegroundProcesses    []Process `json:"foreground_processes"`
}

type PreparedWorkspace interface {
	Endpoint() Endpoint
	Commit() error
	Abort() error
}

type WorkspacePreparer interface {
	PrepareWorkspace(WorkspaceSpec) (PreparedWorkspace, error)
}

type RuntimeClient interface {
	Backend() string
	WithSocket(string) RuntimeClient
	Validate() error
	Diagnostics() (Diagnostics, error)
	CreateWorkspace(WorkspaceSpec) (Endpoint, error)
	Start(Endpoint, string) error
	Inspect(Endpoint) (EndpointState, error)
	ProcessInfo(Endpoint) (ProcessInfo, error)
	Read(Endpoint, int) (string, error)
	Focus(Endpoint) error
	Close(Endpoint) error
	ReportAgent(Endpoint, AgentReport) error
	ReleaseAgent(Endpoint, string, string, uint64) error
	ReportMetadata(Endpoint, string, string, string, string) error
}

type WorkspaceSpec struct {
	WindowID    string   `json:"window_id"`
	WorkspaceID string   `json:"workspace_id"`
	CWD         string   `json:"cwd"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Harness     string   `json:"harness"`
	Source      string   `json:"source"`
	Generation  int      `json:"generation"`
	Environment []string `json:"environment"`
}

type EndpointState struct {
	Endpoint     Endpoint `json:"endpoint"`
	Label        string   `json:"label"`
	Focused      bool     `json:"focused"`
	FullyFocused bool     `json:"fully_focused"`
	PaneCount    int      `json:"pane_count"`
	SurfaceCount int      `json:"surface_count"`
}
