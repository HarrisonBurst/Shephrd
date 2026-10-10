// Command fakeplugin is a scriptable Shephrd plugin for tests. Its behavior
// comes from the plugin options declared in configuration.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type request struct {
	Kind   string `json:"kind"`
	Plugin struct {
		Name    string         `json:"name"`
		Options map[string]any `json:"options"`
	} `json:"plugin"`
	Body json.RawMessage `json:"body"`
}

func main() {
	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "decode:", err)
		os.Exit(2)
	}
	opt := func(key string) string {
		value, _ := req.Plugin.Options[key].(string)
		return value
	}
	if log := opt("log"); log != "" {
		file, err := os.OpenFile(log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			line, _ := json.Marshal(map[string]any{"kind": req.Kind, "body": req.Body, "token": os.Getenv("SHEPHRD_CALL_TOKEN") != "", "env": os.Environ()})
			file.Write(append(line, '\n'))
			file.Close()
		}
	}
	switch opt(req.Kind) {
	case "crash":
		fmt.Fprintln(os.Stderr, "crashing on purpose")
		os.Exit(1)
	case "sleep":
		time.Sleep(time.Minute)
	case "garbage":
		fmt.Print("not json")
		return
	}
	var out any
	switch req.Kind {
	case "intercept":
		decision := opt("decision")
		if decision == "" {
			decision = "allow"
		}
		out = map[string]string{"decision": decision, "reason": opt("reason")}
	case "command":
		var body struct {
			Argv []string `json:"argv"`
		}
		json.Unmarshal(req.Body, &body)
		if len(body.Argv) > 0 && body.Argv[0] == "callback" {
			cmd := exec.Command(opt("shephrd"), body.Argv[1:]...)
			cmd.Stderr = os.Stderr
			result, err := cmd.Output()
			if err != nil {
				out = map[string]any{"error": map[string]string{"kind": "callback_failed", "message": err.Error()}}
				break
			}
			out = json.RawMessage(result)
			break
		}
		if len(body.Argv) > 0 && body.Argv[0] == "fail" {
			out = map[string]any{"error": map[string]string{"kind": "bad_input", "message": "fail requested"}}
			break
		}
		out = map[string]any{"echo": req.Body}
	case "provide":
		out = req.Plugin.Options["provide"]
	default:
		out = map[string]any{}
	}
	json.NewEncoder(os.Stdout).Encode(out)
}
