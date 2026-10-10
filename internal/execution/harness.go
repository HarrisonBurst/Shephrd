package execution

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"

	"github.com/google/uuid"
)

// HarnessRequest is the harness provider contract from the execution spec.
type HarnessRequest struct {
	Mode      string `json:"mode"`
	ReadOnly  bool   `json:"read_only"`
	Brief     string `json:"brief"`
	Prompt    string `json:"prompt"`
	Workspace string `json:"workspace"`
	Model     string `json:"model"`
	Session   string `json:"session,omitempty"`
	Title     string `json:"title"`
}

type HarnessCommand struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	Session string            `json:"session,omitempty"`
}

var Builtin = map[string]func(HarnessRequest) HarnessCommand{
	"claude-code": claudeCode,
	"codex":       codex,
	"pi":          pi,
}

func claudeCode(r HarnessRequest) HarnessCommand {
	args := []string{"claude", "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	if r.ReadOnly {
		args = append(args, "--disallowedTools", "Edit", "Write", "MultiEdit", "NotebookEdit")
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	session := r.Session
	if r.Mode == "resume" {
		args = append(args, "--resume", session)
	} else {
		session = uuid.NewString()
		args = append(args, "--session-id", session, "--name", r.Title)
	}
	return HarnessCommand{Command: append(args, "--", r.Prompt), Session: session}
}

func pi(r HarnessRequest) HarnessCommand {
	args := []string{"pi", "--mode", "json", "--print", "--approve"}
	if r.ReadOnly {
		args = append(args, "--exclude-tools", "edit,write")
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	session := r.Session
	if r.Mode != "resume" {
		session = uuid.NewString()
		args = append(args, "--name", r.Title)
	}
	args = append(args, "--session-id", session)
	return HarnessCommand{Command: append(args, r.Prompt), Session: session}
}

// Codex assigns its own session ID; the supervisor reads it from the log.
func codex(r HarnessRequest) HarnessCommand {
	args := []string{"codex", "exec"}
	if r.Mode == "resume" {
		args = append(args, "resume", r.Session, r.Prompt)
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	args = append(args, "--json", "--dangerously-bypass-approvals-and-sandbox")
	if r.Mode != "resume" {
		args = append(args, "-C", r.Workspace, r.Prompt)
	}
	return HarnessCommand{Command: args, Session: r.Session}
}

func codexSession(logPath string) string {
	file, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer file.Close()
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		if !bytes.Contains(lines.Bytes(), []byte(`"thread.started"`)) {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(lines.Bytes(), &event) == nil && event.Type == "thread.started" {
			return event.ThreadID
		}
	}
	return ""
}
