// Command shephrd-herdr is a Shephrd presentation provider: it runs each
// task's sessions in one Herdr tab, reused across the task's turns.
// Shephrd still owns the process; Herdr only shows it.
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
		Reuse     string `json:"reuse"`
	} `json:"body"`
}

type endpoint struct {
	Tab  string `json:"tab"`
	Pane string `json:"pane"`
}

type herdrError struct {
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

var socket, workspace string

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
	workspace, _ = req.Plugin.Options["workspace"].(string)
	out, err := handle(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

// herdr runs one Herdr command; a not-found answer comes back as code.
func herdr(args ...string) ([]byte, string, error) {
	cmd := exec.Command("herdr", args...)
	cmd.Env = os.Environ()
	if socket != "" {
		cmd.Env = append(cmd.Env, "HERDR_SOCKET_PATH="+socket)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	for _, output := range [][]byte{stdout.Bytes(), stderr.Bytes()} {
		var answer herdrError
		if json.Unmarshal(output, &answer) == nil && answer.Error != nil {
			return nil, answer.Error.Code, fmt.Errorf("herdr %s: %s %s", args[0], answer.Error.Code, answer.Error.Message)
		}
	}
	if err != nil {
		return nil, "", fmt.Errorf("herdr %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), "", nil
}

// workspaceID finds the configured workspace by ID or label, creating a
// workspace with that label when there is none.
func workspaceID() (string, error) {
	out, _, err := herdr("workspace", "list")
	if err != nil {
		return "", err
	}
	var listed struct {
		Result struct {
			Workspaces []struct {
				ID    string `json:"workspace_id"`
				Label string `json:"label"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return "", fmt.Errorf("herdr workspace list: %v", err)
	}
	for _, w := range listed.Result.Workspaces {
		if w.ID == workspace || w.Label == workspace {
			return w.ID, nil
		}
	}
	out, _, err = herdr("workspace", "create", "--label", workspace, "--no-focus")
	if err != nil {
		return "", err
	}
	var created struct {
		Result struct {
			Workspace struct {
				ID string `json:"workspace_id"`
			} `json:"workspace"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &created); err != nil || created.Result.Workspace.ID == "" {
		return "", fmt.Errorf("herdr workspace create returned no workspace: %s", out)
	}
	return created.Result.Workspace.ID, nil
}

func handle(req request) (map[string]string, error) {
	if workspace == "" {
		workspace = "Shephrd tasks"
	}
	var at endpoint
	if req.Body.Endpoint != "" {
		if err := json.Unmarshal([]byte(req.Body.Endpoint), &at); err != nil {
			return nil, fmt.Errorf("endpoint: %v", err)
		}
	}
	switch req.Body.Operation {
	case "open":
		var previous endpoint
		if json.Unmarshal([]byte(req.Body.Reuse), &previous) == nil && previous.Pane != "" {
			if _, _, err := herdr("pane", "get", previous.Pane); err == nil {
				herdr("tab", "rename", previous.Tab, req.Body.Title)
				if _, _, err := herdr("pane", "run", previous.Pane, req.Body.Command); err == nil {
					return map[string]string{"endpoint": req.Body.Reuse}, nil
				}
			}
		}
		id, err := workspaceID()
		if err != nil {
			return nil, err
		}
		out, _, err := herdr("tab", "create", "--workspace", id, "--cwd", req.Body.Workspace, "--label", req.Body.Title, "--no-focus")
		if err != nil {
			return nil, err
		}
		var created struct {
			Result struct {
				Tab struct {
					ID string `json:"tab_id"`
				} `json:"tab"`
				RootPane struct {
					ID string `json:"pane_id"`
				} `json:"root_pane"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &created); err != nil || created.Result.RootPane.ID == "" {
			return nil, fmt.Errorf("herdr tab create returned no pane: %s", out)
		}
		at = endpoint{Tab: created.Result.Tab.ID, Pane: created.Result.RootPane.ID}
		if _, _, err := herdr("pane", "run", at.Pane, req.Body.Command); err != nil {
			herdr("pane", "close", at.Pane)
			return nil, err
		}
		encoded, _ := json.Marshal(at)
		return map[string]string{"endpoint": string(encoded)}, nil
	case "probe":
		_, code, err := herdr("pane", "get", at.Pane)
		switch {
		case err == nil:
			return map[string]string{"state": "present"}, nil
		case strings.HasSuffix(code, "_not_found"):
			return map[string]string{"state": "absent"}, nil
		}
		return map[string]string{"state": "uncertain"}, nil
	case "close":
		if _, code, err := herdr("pane", "close", at.Pane); err != nil && !strings.HasSuffix(code, "_not_found") {
			return nil, err
		}
		return map[string]string{}, nil
	case "focus":
		_, _, err := herdr("tab", "focus", at.Tab)
		return map[string]string{}, err
	}
	return nil, fmt.Errorf("unknown presentation operation %q", req.Body.Operation)
}
