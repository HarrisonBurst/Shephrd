package execution

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// HarnessRequest is the harness provider contract from the execution spec.
// An interactive request runs the harness's own UI in a terminal; the
// supervisor ends the turn once it is reported.
type HarnessRequest struct {
	Mode        string `json:"mode"`
	ReadOnly    bool   `json:"read_only"`
	Interactive bool   `json:"interactive"`
	Brief       string `json:"brief"`
	Prompt      string `json:"prompt"`
	Workspace   string `json:"workspace"`
	Model       string `json:"model"`
	Session     string `json:"session,omitempty"`
	Title       string `json:"title"`
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
	args := []string{"claude", "--dangerously-skip-permissions"}
	if r.Interactive {
		args = append(args, "--settings", `{"skipDangerousModePermissionPrompt":true}`)
	} else {
		args = append(args, "-p", "--output-format", "stream-json", "--verbose")
	}
	if r.ReadOnly {
		args = append(args, "--disallowedTools", "Edit", "Write", "NotebookEdit")
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
	args := []string{"pi", "--approve"}
	if !r.Interactive {
		args = append(args, "--mode", "json", "--print")
	}
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
	return HarnessCommand{Command: append(args, "--", r.Prompt), Session: session}
}

// Codex assigns its own session ID; the supervisor finds it afterwards.
func codex(r HarnessRequest) HarnessCommand {
	args := []string{"codex"}
	if !r.Interactive {
		args = append(args, "exec")
	}
	if r.Mode == "resume" {
		args = append(args, "resume")
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	if r.Interactive {
		args = append(args, "--no-daemon")
	} else {
		args = append(args, "--json")
	}
	if r.Mode == "resume" {
		return HarnessCommand{Command: append(args, r.Session, r.Prompt), Session: r.Session}
	}
	return HarnessCommand{Command: append(args, "-C", r.Workspace, r.Prompt), Session: r.Session}
}

// codexSession reads the session ID from a headless run's JSON log.
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

// codexInteractiveSession finds the session an interactive Codex run
// recorded for its workspace since the run started.
func codexInteractiveSession(home, workspace string, since time.Time) string {
	files, _ := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*.jsonl"))
	newest, newestTime := "", since.Add(-time.Second)
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.ModTime().Before(newestTime) {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		line, _ := bufio.NewReader(file).ReadBytes('\n')
		file.Close()
		var meta struct {
			Type    string `json:"type"`
			Payload struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &meta) == nil && meta.Type == "session_meta" && strings.TrimSpace(meta.Payload.Cwd) == workspace {
			newest, newestTime = meta.Payload.ID, info.ModTime()
		}
	}
	return newest
}
