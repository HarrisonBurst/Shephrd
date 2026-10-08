package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

	extensionhost "shephrd/internal/extension"
	"shephrd/internal/terminal"
)

const (
	window    = "11111111-1111-4111-8111-111111111111"
	workspace = "22222222-2222-4222-8222-222222222222"
	pane      = "33333333-3333-4333-8333-333333333333"
	surface   = "44444444-4444-4444-8444-444444444444"
)

func main() {
	if len(os.Args) != 4 {
		os.Exit(2)
	}
	if os.Args[3] == "describe" {
		write(terminal.CmuxExtensionManifest())
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
	endpoint := terminal.Endpoint{Backend: "cmux", SocketPath: "/socket", WindowID: window, WorkspaceID: workspace, PaneID: pane, SurfaceID: surface}
	var result any = struct{}{}
	status := "ok"
	switch request.Operation {
	case "detect":
		result = struct {
			State       string               `json:"state"`
			Parent      terminal.Parent      `json:"parent"`
			Diagnostics terminal.Diagnostics `json:"diagnostics"`
		}{State: terminal.DetectionMatched, Parent: terminal.Parent{Backend: "cmux", SocketPath: "/socket", WindowID: window, WorkspaceID: workspace, PaneID: pane, SurfaceID: surface}, Diagnostics: diagnostics()}
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

func diagnostics() terminal.Diagnostics {
	return terminal.Diagnostics{ProviderVersion: "fixture-1", ProtocolVersion: "2", Capabilities: []string{"system.identify", "workspace.close"}}
}

func respond(request extensionhost.Request, status string, result any) {
	encoded, err := json.Marshal(result)
	if err != nil {
		os.Exit(7)
	}
	write(extensionhost.Response{Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability, CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: status, Result: encoded, Extension: terminal.CmuxExtensionManifest().Extension})
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
