package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"

	"shephrd/internal/extension"
	"shephrd/internal/notification"
)

func main() {
	if len(os.Args) != 4 {
		os.Exit(2)
	}
	logPath, behavior, command := os.Args[1], os.Args[2], os.Args[3]
	if command == "describe" {
		write(notification.Manifest())
		return
	}
	if command != "invoke" {
		os.Exit(2)
	}
	var request extension.Request
	if err := json.NewDecoder(bufio.NewReader(os.Stdin)).Decode(&request); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(logPath, request.Payload, 0o600); err != nil {
		os.Exit(4)
	}
	if behavior == "hang" {
		child := exec.Command("/bin/sleep", "60")
		if err := child.Start(); err != nil {
			os.Exit(5)
		}
		if err := os.WriteFile(logPath, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(6)
		}
		_ = child.Wait()
		return
	}
	payload, _ := json.Marshal(notification.PresentationResult{Presented: behavior != "unconfirmed"})
	write(extension.Response{
		Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability,
		CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "ok",
		Result: payload, Extension: notification.Manifest().Extension,
	})
}

func write(value any) {
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		os.Exit(7)
	}
}
