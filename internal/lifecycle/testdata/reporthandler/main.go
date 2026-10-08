package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"shephrd/internal/extension"
	"shephrd/internal/lifecycle"
)

const extensionID = "fixture.report-memory"

func main() {
	if len(os.Args) != 4 {
		os.Exit(2)
	}
	switch os.Args[3] {
	case "describe":
		write(lifecycleManifest())
	case "invoke":
		invoke(os.Args[1], os.Args[2])
	default:
		os.Exit(2)
	}
}

func lifecycleManifest() extension.Manifest {
	return extension.Manifest{
		Wire:         extension.WireRange{Major: extension.WireMajor, MinorMin: extension.WireMinor, MinorMax: extension.WireMinor},
		Extension:    extension.Identity{ID: extensionID, Version: "1.0.0"},
		Capabilities: []extension.Capability{lifecycle.ReportAcceptedCapabilityManifest()},
	}
}

func invoke(memoryDir, mode string) {
	frame, err := bufio.NewReaderSize(os.Stdin, 64*1024).ReadBytes('\n')
	if err != nil || len(frame) > extension.MaxFrameBytes {
		os.Exit(3)
	}
	var request extension.Request
	if extension.StrictDecode(bytes.TrimSpace(frame), &request) != nil || request.Wire != (extension.WireVersion{Major: extension.WireMajor, Minor: extension.WireMinor}) || request.Capability != lifecycle.ReportAcceptedCapability || request.CapabilityVersion != lifecycle.ReportAcceptedCapabilityVersion || request.Operation != lifecycle.ReportAcceptedOperation || request.DeadlineUnixMS <= time.Now().UnixMilli() {
		os.Exit(3)
	}
	var payload lifecycle.ReportAcceptedRequest
	if extension.StrictDecode(request.Payload, &payload) != nil || payload.Event.Name != lifecycle.ReportAcceptedEvent || payload.Event.Version != lifecycle.ReportAcceptedEventVersion || payload.Event.ID == "" || payload.Invocation.ID == "" || payload.Report.ArtifactID == "" || payload.Report.Kind != "report" || payload.Task.ID == "" || payload.Task.AttemptID == "" {
		respondError(request, "fixture_request_invalid", "fixture rejected report.accepted request", "none")
		return
	}
	if mode == "fail" {
		respondError(request, "fixture_memory_unavailable", "fixture memory system is unavailable", "none")
		return
	}
	body, err := os.ReadFile(payload.Report.SnapshotPath)
	if err != nil || int64(len(body)) != payload.Report.SizeBytes {
		respondError(request, "fixture_report_unreadable", "fixture could not read immutable report", "none")
		return
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != payload.Report.SHA256 {
		respondError(request, "fixture_report_mismatch", "fixture report digest does not match event", "none")
		return
	}
	summary := summarize(body)
	receiptID := "memory:" + payload.Event.ID
	entry := struct {
		EventID    string `json:"event_id"`
		Invocation string `json:"invocation_id"`
		ArtifactID string `json:"artifact_id"`
		SHA256     string `json:"sha256"`
		Summary    string `json:"summary"`
		ReceiptID  string `json:"receipt_id"`
	}{payload.Event.ID, payload.Invocation.ID, payload.Report.ArtifactID, payload.Report.SHA256, summary, receiptID}
	encoded, err := json.Marshal(entry)
	if err != nil || install(memoryDir, payload.Invocation.ID+".json", encoded) != nil {
		respondError(request, "fixture_memory_write_failed", "fixture could not persist memory receipt", "unknown")
		return
	}
	if mode == "unknown" {
		respondError(request, "fixture_response_interrupted", "fixture side effect completed before response interruption", "unknown")
		return
	}
	result := lifecycle.ReportAcceptedResult{Annotation: summary, Receipt: &lifecycle.Receipt{System: "fixture-memory", ID: receiptID}}
	respond(request, result)
}

func summarize(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.Join(strings.Fields(line), " ")
		line = strings.Map(func(char rune) rune {
			if char < 0x20 || char == 0x7f {
				return ' '
			}
			return char
		}, line)
		if line != "" {
			if len(line) > 160 {
				line = strings.ToValidUTF8(line[:160], "")
			}
			return "Report accepted: " + line
		}
	}
	return "Report accepted: empty report"
}

func install(dir, name string, body []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, body) {
			return nil
		}
		return fmt.Errorf("receipt conflict")
	}
	temporary, err := os.CreateTemp(dir, ".receipt-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o400); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, body) {
			return nil
		}
		return err
	}
	return nil
}

func respond(request extension.Request, result lifecycle.ReportAcceptedResult) {
	encoded, err := json.Marshal(result)
	if err != nil {
		os.Exit(4)
	}
	write(extension.Response{Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability, CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "ok", Result: encoded, Extension: lifecycleManifest().Extension})
}

func respondError(request extension.Request, code, message, effect string) {
	write(extension.Response{Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability, CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "error", Error: &extension.Failure{Class: "unavailable", Code: code, Message: message, Effect: effect}, Extension: lifecycleManifest().Extension})
}

func write(value any) {
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
}
