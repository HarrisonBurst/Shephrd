package terminal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	extensionhost "shephrd/internal/extension"
)

var extensionRequestIDPattern = regexp.MustCompile(`^extension_request_[0-9a-f]{24}$`)

type terminalExtensionServer struct {
	spec      terminalExtensionSpec
	newClient func(string) RuntimeClient
	detect    func(ParentContext) Detection
}

func runTerminalExtension(args []string, stdin io.Reader, stdout io.Writer, server terminalExtensionServer) error {
	if len(args) != 1 {
		return fmt.Errorf("expected exactly one command")
	}
	switch args[0] {
	case "describe":
		return writeExtensionJSON(stdout, terminalExtensionManifest(server.spec))
	case "invoke":
		return runTerminalExtensionInvocation(stdin, stdout, server)
	default:
		return fmt.Errorf("unknown command")
	}
}

func runTerminalExtensionInvocation(stdin io.Reader, stdout io.Writer, server terminalExtensionServer) error {
	reader := bufio.NewReaderSize(stdin, 64*1024)
	frame, err := readExtensionFrame(reader)
	if err != nil {
		return err
	}
	var request extensionhost.Request
	if err := extensionhost.StrictDecode(frame, &request); err != nil {
		return err
	}
	if err := validateTerminalExtensionRequest(request, server.spec); err != nil {
		return err
	}
	switch request.Operation {
	case "detect":
		var payload extensionDetectRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		detection := server.detect(payload.Context)
		if detection.State == DetectionInvalid {
			return writeTerminalExtensionError(stdout, request, server.spec, detection.Err, "none")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionDetectResult{State: detection.State, Parent: detection.Parent, Diagnostics: detection.Diagnostics})
	case "diagnostics":
		var payload extensionSocketRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		diagnostics, err := server.newClient(payload.SocketPath).Diagnostics()
		if err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionDiagnosticsResult{Diagnostics: diagnostics})
	case "create":
		return runTerminalExtensionCreate(reader, stdout, request, server)
	case "start":
		var payload extensionStartRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		if err := server.newClient(payload.Endpoint.SocketPath).Start(payload.Endpoint, payload.Command); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "unknown")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionUnit{})
	case "inspect":
		var payload extensionEndpointRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		state, err := server.newClient(payload.Endpoint.SocketPath).Inspect(payload.Endpoint)
		if err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionStateResult{State: state})
	case "process_info":
		var payload extensionEndpointRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		info, err := server.newClient(payload.Endpoint.SocketPath).ProcessInfo(payload.Endpoint)
		if err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionProcessResult{ProcessInfo: info})
	case "read":
		var payload extensionReadRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		text, err := server.newClient(payload.Endpoint.SocketPath).Read(payload.Endpoint, payload.Lines)
		if err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionReadResult{Text: text})
	case "focus":
		var payload extensionEndpointRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		if err := server.newClient(payload.Endpoint.SocketPath).Focus(payload.Endpoint); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "unknown")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionUnit{})
	case "close":
		var payload extensionEndpointRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		if err := server.newClient(payload.Endpoint.SocketPath).Close(payload.Endpoint); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "unknown")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionUnit{})
	case "report_agent":
		var payload extensionAgentRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		if err := server.newClient(payload.Endpoint.SocketPath).ReportAgent(payload.Endpoint, payload.Report); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "unknown")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionUnit{})
	case "release_agent":
		var payload extensionReleaseAgentRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		if err := server.newClient(payload.Endpoint.SocketPath).ReleaseAgent(payload.Endpoint, payload.Source, payload.Agent, payload.Sequence); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "unknown")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionUnit{})
	case "report_metadata":
		var payload extensionMetadataRequest
		if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
		}
		if err := server.newClient(payload.Endpoint.SocketPath).ReportMetadata(payload.Endpoint, payload.Label, payload.Harness, payload.Source, payload.State); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "unknown")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionUnit{})
	default:
		return fmt.Errorf("undeclared operation")
	}
}

func runTerminalExtensionCreate(reader *bufio.Reader, stdout io.Writer, request extensionhost.Request, server terminalExtensionServer) error {
	var payload extensionCreateRequest
	if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
		return writeTerminalExtensionError(stdout, request, server.spec, err, "none")
	}
	client := server.newClient(payload.SocketPath)
	endpoint, err := client.CreateWorkspace(payload.Spec)
	if err != nil {
		effect := CreateEffectNone
		if endpoint.WindowID != "" || endpoint.WorkspaceID != "" || endpoint.TabID != "" || endpoint.PaneID != "" || endpoint.SurfaceID != "" {
			effect = CreateEffectUnknown
		}
		var sideEffect *CreateSideEffect
		if errors.As(err, &sideEffect) {
			effect = sideEffect.Effect
		}
		return writeTerminalExtensionError(stdout, request, server.spec, err, effect)
	}
	if err := writeTerminalExtensionResponse(stdout, request, server.spec, "prepared", extensionEndpointResult{Endpoint: endpoint}); err != nil {
		_ = client.Close(endpoint)
		return err
	}
	controlFrame, err := readExtensionFrame(reader)
	if err != nil {
		_ = client.Close(endpoint)
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var control extensionhost.Control
	if err := extensionhost.StrictDecode(controlFrame, &control); err != nil || control.Wire != request.Wire || control.RequestID != request.RequestID {
		closeErr := client.Close(endpoint)
		if closeErr != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, closeErr, "unknown")
		}
		return writeTerminalExtensionError(stdout, request, server.spec, fmt.Errorf("create control frame is invalid"), "none")
	}
	switch control.Action {
	case "commit":
		return writeTerminalExtensionResponse(stdout, request, server.spec, "ok", extensionUnit{})
	case "abort":
		if err := client.Close(endpoint); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "unknown")
		}
		return writeTerminalExtensionResponse(stdout, request, server.spec, "aborted", extensionUnit{})
	default:
		if err := client.Close(endpoint); err != nil {
			return writeTerminalExtensionError(stdout, request, server.spec, err, "unknown")
		}
		return writeTerminalExtensionError(stdout, request, server.spec, fmt.Errorf("create control action is invalid"), "none")
	}
}

func validateTerminalExtensionRequest(request extensionhost.Request, spec terminalExtensionSpec) error {
	now := time.Now()
	if request.Wire != (extensionhost.WireVersion{Major: extensionhost.WireMajor, Minor: extensionhost.WireMinor}) || !extensionRequestIDPattern.MatchString(request.RequestID) || request.Capability != spec.capability || request.CapabilityVersion != terminalCapabilityVersion || request.DeadlineUnixMS <= now.UnixMilli() || request.DeadlineUnixMS > now.Add(20*time.Second).UnixMilli() {
		return fmt.Errorf("request envelope is invalid or expired")
	}
	for _, operation := range terminalCapabilityOperations {
		if request.Operation == operation {
			return nil
		}
	}
	return fmt.Errorf("request operation is not declared")
}

func writeTerminalExtensionResponse(writer io.Writer, request extensionhost.Request, spec terminalExtensionSpec, status string, result any) error {
	payload, err := json.Marshal(result)
	if err != nil || len(payload) > extensionhost.MaxFrameBytes {
		return fmt.Errorf("encode extension result")
	}
	response := extensionhost.Response{Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability, CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: status, Result: payload, Extension: terminalExtensionManifest(spec).Extension}
	return writeExtensionJSON(writer, response)
}

func writeTerminalExtensionError(writer io.Writer, request extensionhost.Request, spec terminalExtensionSpec, err error, effect string) error {
	class := Classify(err)
	if IsNotFound(err) {
		class = ErrorEndpointAbsent
	}
	switch class {
	case ErrorEndpointAbsent, ErrorUnavailable, ErrorUnauthorized, ErrorIncompatible, ErrorMalformed, ErrorAmbiguous:
	default:
		class = ErrorMalformed
	}
	message := "terminal capability operation failed"
	if err != nil {
		message = err.Error()
		if len(message) > 2048 {
			message = message[:2048]
		}
	}
	response := extensionhost.Response{Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability, CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "error", Error: &extensionhost.Failure{Class: class, Code: class, Message: message, Effect: effect}, Extension: terminalExtensionManifest(spec).Extension}
	return writeExtensionJSON(writer, response)
}

func writeExtensionJSON(writer io.Writer, value any) error {
	frame, err := json.Marshal(value)
	if err != nil || len(frame) > extensionhost.MaxFrameBytes {
		return fmt.Errorf("encode extension frame")
	}
	frame = append(frame, '\n')
	_, err = writer.Write(frame)
	return err
}

func readExtensionFrame(reader *bufio.Reader) ([]byte, error) {
	var frame bytes.Buffer
	for {
		fragment, err := reader.ReadSlice('\n')
		if frame.Len()+len(fragment) > extensionhost.MaxFrameBytes {
			return nil, fmt.Errorf("extension request frame exceeds the size limit")
		}
		frame.Write(fragment)
		if err == nil {
			return bytes.TrimSuffix(frame.Bytes(), []byte{'\n'}), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && frame.Len() > 0 {
			return frame.Bytes(), nil
		}
		return nil, err
	}
}
