// Command shephrd-macos-notify shows a macOS notification for each new
// inbox item. It subscribes to inbox.added and reads the task it names
// with its own call token.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type request struct {
	Kind string `json:"kind"`
	Body struct {
		Events []struct {
			Name string `json:"name"`
			Task string `json:"task"`
			Data struct {
				Driver string `json:"driver"`
				Item   int64  `json:"item"`
			} `json:"data"`
		} `json:"events"`
	} `json:"body"`
}

func main() {
	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "decode request:", err)
		os.Exit(2)
	}
	for _, e := range req.Body.Events {
		if e.Name != "inbox.added" {
			continue
		}
		title, reason := describe(e.Task)
		message := fmt.Sprintf("%s: %s", e.Task, title)
		subtitle := fmt.Sprintf("Inbox item %d for %s", e.Data.Item, e.Data.Driver)
		if reason != "" {
			subtitle += " (" + reason + ")"
		}
		script := fmt.Sprintf("display notification %s with title %s subtitle %s", quote(message), quote("Shephrd"), quote(subtitle))
		if out, err := exec.Command("osascript", "-e", script).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "notify %s: %v: %s\n", e.Task, err, out)
		}
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{})
}

// describe reads the task's title and state with this call's token.
func describe(task string) (string, string) {
	out, err := exec.Command("shephrd", "task", "show", task, "--events", "1").Output()
	if err != nil {
		return "needs attention", ""
	}
	var shown struct {
		Task struct {
			Title  string `json:"title"`
			State  string `json:"state"`
			Reason string `json:"reason"`
		} `json:"task"`
	}
	if json.Unmarshal(out, &shown) != nil {
		return "needs attention", ""
	}
	return shown.Task.Title, strings.TrimSpace(shown.Task.State + " " + shown.Task.Reason)
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
