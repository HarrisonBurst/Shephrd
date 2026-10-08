package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	extensionhost "shephrd/internal/extension"
	"shephrd/internal/terminal"
)

const (
	workspace = "w7"
	tab       = "w7:t2"
	pane      = "w7:p2"
)

func main() {
	if len(os.Args) != 4 {
		os.Exit(2)
	}
	if os.Args[3] == "describe" {
		write(terminal.HerdrExtensionManifest())
		return
	}
	if os.Args[3] != "invoke" {
		os.Exit(2)
	}
	reader := bufio.NewReader(os.Stdin)
	var request extensionhost.Request
	if err := json.NewDecoder(reader).Decode(&request); err != nil || request.DeadlineUnixMS <= time.Now().UnixMilli() {
		os.Exit(3)
	}
	log(request.Operation)
	if _, err := os.Stat(os.Args[1] + ".panes"); err == nil {
		panes(reader, request)
		return
	}
	if request.Operation == "inspect" || request.Operation == "process_info" {
		if delay, err := os.ReadFile(os.Args[1] + ".delay"); err == nil {
			duration, err := time.ParseDuration(strings.TrimSpace(string(delay)))
			if err != nil {
				os.Exit(10)
			}
			time.Sleep(duration)
		}
		if _, err := os.Stat(os.Args[1] + ".absent"); err == nil {
			write(extensionhost.Response{Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability, CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "error", Error: &extensionhost.Failure{Class: terminal.ErrorEndpointAbsent, Code: "endpoint_absent", Message: "fixture endpoint absent", Effect: "none"}, Extension: terminal.HerdrExtensionManifest().Extension})
			return
		}
	}
	endpoint := terminal.Endpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: workspace, TabID: tab, PaneID: pane}
	var result any = struct{}{}
	status := "ok"
	switch request.Operation {
	case "detect":
		result = struct {
			State       string               `json:"state"`
			Parent      terminal.Parent      `json:"parent"`
			Diagnostics terminal.Diagnostics `json:"diagnostics"`
		}{State: terminal.DetectionMatched, Parent: terminal.Parent{Backend: "herdr", SocketPath: "/socket", WorkspaceID: workspace, TabID: "w7:t1", PaneID: "w7:p1"}, Diagnostics: diagnostics()}
	case "diagnostics":
		result = struct {
			Diagnostics terminal.Diagnostics `json:"diagnostics"`
		}{Diagnostics: diagnostics()}
	case "create":
		status = "prepared"
		result = struct {
			Endpoint terminal.Endpoint `json:"endpoint"`
		}{Endpoint: endpoint}
		respond(request, status, result)
		var control extensionhost.Control
		if err := json.NewDecoder(reader).Decode(&control); err != nil || control.RequestID != request.RequestID {
			os.Exit(4)
		}
		log(control.Action)
		if control.Action == "abort" {
			status = "aborted"
		} else if control.Action != "commit" {
			os.Exit(5)
		} else {
			status = "ok"
		}
		result = struct{}{}
	case "inspect":
		result = struct {
			State terminal.EndpointState `json:"state"`
		}{State: terminal.EndpointState{Endpoint: endpoint, Label: "fixture", PaneCount: 1, SurfaceCount: 1}}
	case "process_info":
		result = struct {
			ProcessInfo terminal.ProcessInfo `json:"process_info"`
		}{ProcessInfo: terminal.ProcessInfo{PaneID: pane, ForegroundProcesses: []terminal.Process{{PID: 8801, PGID: 8801, Name: "shephrd", Path: os.Args[2]}}}}
	case "read":
		result = struct {
			Text string `json:"text"`
		}{Text: "fixture output"}
	case "start", "focus", "close", "report_agent", "release_agent", "report_metadata":
	default:
		os.Exit(6)
	}
	respond(request, status, result)
}

// panes models distinct Herdr panes for built-CLI tests: a pane is live while
// the PID recorded in <log>.<pane>.pid runs, and closing it makes it absent.
func panes(reader *bufio.Reader, request extensionhost.Request) {
	var payload struct {
		Context  terminal.ParentContext `json:"context"`
		Spec     terminal.WorkspaceSpec `json:"spec"`
		Endpoint terminal.Endpoint      `json:"endpoint"`
		Command  string                 `json:"command"`
	}
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		os.Exit(11)
	}
	state := func(pane, suffix string) string { return os.Args[1] + "." + pane + suffix }
	exists := func(path string) bool { _, err := os.Stat(path); return err == nil }
	endpoint := payload.Endpoint
	var result any = struct{}{}
	switch request.Operation {
	case "detect":
		parent := payload.Context.Value("HERDR_PANE_ID")
		appendLine(os.Args[1]+".parents", parent)
		result = struct {
			State       string               `json:"state"`
			Parent      terminal.Parent      `json:"parent"`
			Diagnostics terminal.Diagnostics `json:"diagnostics"`
		}{State: terminal.DetectionMatched, Parent: terminal.Parent{Backend: "herdr", SocketPath: "/socket", WorkspaceID: workspace, TabID: "w7:t1", PaneID: parent}, Diagnostics: diagnostics()}
	case "diagnostics":
		result = struct {
			Diagnostics terminal.Diagnostics `json:"diagnostics"`
		}{Diagnostics: diagnostics()}
	case "create":
		body, err := os.ReadFile(os.Args[1] + ".panes")
		if err != nil {
			os.Exit(12)
		}
		index := strings.Count(string(body), "\n") + 2
		endpoint = terminal.Endpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: workspace, TabID: fmt.Sprintf("w7:t%d", index), PaneID: fmt.Sprintf("w7:p%d", index)}
		spec, err := json.Marshal(payload.Spec)
		if err != nil || os.WriteFile(state(endpoint.PaneID, ".spec"), spec, 0o600) != nil {
			os.Exit(13)
		}
		respond(request, "prepared", struct {
			Endpoint terminal.Endpoint `json:"endpoint"`
		}{Endpoint: endpoint})
		var control extensionhost.Control
		if err := json.NewDecoder(reader).Decode(&control); err != nil || control.RequestID != request.RequestID {
			os.Exit(4)
		}
		log(control.Action)
		status := "aborted"
		if control.Action == "commit" {
			status = "ok"
			appendLine(os.Args[1]+".panes", endpoint.PaneID)
		}
		respond(request, status, struct{}{})
		return
	case "start":
		if err := os.WriteFile(state(endpoint.PaneID, ".start"), []byte(payload.Command), 0o600); err != nil {
			os.Exit(14)
		}
	case "inspect", "process_info":
		if exists(state(endpoint.PaneID, ".absent")) {
			write(extensionhost.Response{Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability, CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "error", Error: &extensionhost.Failure{Class: terminal.ErrorEndpointAbsent, Code: "endpoint_absent", Message: "fixture pane closed", Effect: "none"}, Extension: terminal.HerdrExtensionManifest().Extension})
			return
		}
		if request.Operation == "inspect" {
			result = struct {
				State terminal.EndpointState `json:"state"`
			}{State: terminal.EndpointState{Endpoint: endpoint, Label: "fixture", PaneCount: 1, SurfaceCount: 1}}
			break
		}
		info := terminal.ProcessInfo{PaneID: endpoint.PaneID, ShellPID: 1}
		var pid int
		if body, err := os.ReadFile(state(endpoint.PaneID, ".pid")); err == nil {
			_, _ = fmt.Sscan(string(body), &pid)
		}
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			info.ForegroundProcesses = []terminal.Process{{PID: pid, PGID: pid, Name: "shephrd", Path: os.Args[2]}}
		}
		result = struct {
			ProcessInfo terminal.ProcessInfo `json:"process_info"`
		}{ProcessInfo: info}
	case "close":
		appendLine(os.Args[1]+".closed", endpoint.PaneID)
		if err := os.WriteFile(state(endpoint.PaneID, ".absent"), nil, 0o600); err != nil {
			os.Exit(15)
		}
	case "read":
		result = struct {
			Text string `json:"text"`
		}{Text: "fixture output"}
	case "focus", "report_agent", "release_agent", "report_metadata":
	default:
		os.Exit(6)
	}
	respond(request, "ok", result)
}

func appendLine(path, value string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(9)
	}
	_, _ = fmt.Fprintln(file, value)
	_ = file.Close()
}

func diagnostics() terminal.Diagnostics {
	return terminal.Diagnostics{ProviderVersion: "fixture-1", ProtocolVersion: "17", Capabilities: []string{"exact-pane-lifecycle", "process-info", "state-report"}}
}

func respond(request extensionhost.Request, status string, result any) {
	encoded, err := json.Marshal(result)
	if err != nil {
		os.Exit(7)
	}
	write(extensionhost.Response{Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability, CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: status, Result: encoded, Extension: terminal.HerdrExtensionManifest().Extension})
}

func write(value any) {
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(8)
	}
}

func log(value string) {
	file, err := os.OpenFile(os.Args[1], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(9)
	}
	_, _ = fmt.Fprintln(file, value)
	_ = file.Close()
}
