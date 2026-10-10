// Command shephrd-cmux is a Shephrd presentation provider: it runs each
// session's supervisor in its own cmux workspace. Shephrd still owns the
// process; cmux only shows it.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type request struct {
	Kind   string `json:"kind"`
	Plugin struct {
		Options map[string]any `json:"options"`
	} `json:"plugin"`
	Body struct {
		Operation string `json:"operation"`
		Title     string `json:"title"`
		Command   string `json:"command"`
		Workspace string `json:"workspace"`
		Endpoint  string `json:"endpoint"`
	} `json:"body"`
}

type endpoint struct {
	Window    string `json:"window_id"`
	Workspace string `json:"workspace_id"`
	Surface   string `json:"surface_id"`
}

var socket, window, password string

func main() {
	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "decode request:", err)
		os.Exit(2)
	}
	if req.Kind != "provide" {
		json.NewEncoder(os.Stdout).Encode(map[string]any{})
		return
	}
	socket, _ = req.Plugin.Options["socket"].(string)
	window, _ = req.Plugin.Options["window"].(string)
	password, _ = req.Plugin.Options["password"].(string)
	out, err := handle(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

func cmux(args ...string) ([]byte, error) {
	global := []string{"--socket", socket, "--json", "--id-format", "uuids"}
	if password != "" {
		global = append(global, "--password", password)
	}
	cmd := exec.Command("cmux", append(global, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cmux %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	return stdout.Bytes(), nil
}

func handle(req request) (map[string]string, error) {
	if socket == "" || window == "" {
		return nil, fmt.Errorf("the cmux plugin needs socket and window options")
	}
	var at endpoint
	if req.Body.Endpoint != "" {
		if err := json.Unmarshal([]byte(req.Body.Endpoint), &at); err != nil {
			return nil, fmt.Errorf("endpoint: %v", err)
		}
	}
	switch req.Body.Operation {
	case "open":
		out, err := cmux("workspace", "create", "--name", req.Body.Title, "--cwd", req.Body.Workspace, "--window", window, "--focus", "false")
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(out, &at); err != nil || at.Workspace == "" || at.Surface == "" {
			return nil, fmt.Errorf("cmux workspace create returned no surface: %s", out)
		}
		target := []string{"--workspace", at.Workspace, "--surface", at.Surface, "--window", at.Window}
		if _, err := cmux(append(append([]string{"send"}, target...), "--", req.Body.Command)...); err == nil {
			_, err = cmux(append(append([]string{"send-key"}, target...), "enter")...)
		}
		if err != nil {
			cmux("workspace", "close", at.Workspace, "--window", at.Window)
			return nil, err
		}
		encoded, _ := json.Marshal(at)
		return map[string]string{"endpoint": string(encoded)}, nil
	case "probe":
		out, err := cmux("tree", "--workspace", at.Workspace, "--window", at.Window)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "not found") {
				return map[string]string{"state": "absent"}, nil
			}
			return map[string]string{"state": "uncertain"}, nil
		}
		if strings.Contains(string(out), at.Workspace) {
			return map[string]string{"state": "present"}, nil
		}
		return map[string]string{"state": "absent"}, nil
	case "close":
		if _, err := cmux("workspace", "close", at.Workspace, "--window", at.Window); err != nil && !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, err
		}
		return map[string]string{}, nil
	case "focus":
		_, err := cmux("workspace", "select", at.Workspace, "--window", at.Window)
		return map[string]string{}, err
	}
	return nil, fmt.Errorf("unknown presentation operation %q", req.Body.Operation)
}
