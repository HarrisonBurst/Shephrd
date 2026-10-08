package claudebridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type hookSettings struct {
	Hooks                             map[string][]hookGroup `json:"hooks"`
	SkipDangerousModePermissionPrompt bool                   `json:"skipDangerousModePermissionPrompt"`
}

type hookGroup struct {
	Hooks []hookCommand `json:"hooks"`
}

type hookCommand struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

func WriteSettings(dir, attemptID string, generation int, executable string) (string, error) {
	if attemptID == "" || generation < 1 || !filepath.IsAbs(executable) {
		return "", fmt.Errorf("Claude bridge settings identity is invalid")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	command := hookCommand{Type: "command", Command: executable, Args: []string{"_claude-hook"}, Timeout: 10}
	hooks := make(map[string][]hookGroup)
	for _, event := range []string{"SessionStart", "MessageDisplay", "Stop", "StopFailure", "SessionEnd"} {
		hooks[event] = []hookGroup{{Hooks: []hookCommand{command}}}
	}
	body, err := json.Marshal(hookSettings{Hooks: hooks, SkipDangerousModePermissionPrompt: true})
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("claude-herdr-bridge-%s-run-%d.json", attemptID, generation))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
