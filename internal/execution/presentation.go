package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"shephrd/internal/fault"
	"shephrd/internal/plugin"
)

type LaunchResponse struct {
	PID      int    `json:"pid,omitempty"`
	Start    string `json:"start,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// PresentationRequest is the presentation provider contract: open a
// terminal running a command, probe or close an endpoint, or focus it.
type PresentationRequest struct {
	Operation string `json:"operation"`
	Title     string `json:"title,omitempty"`
	Command   string `json:"command,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
}

type PresentationResponse struct {
	Endpoint string `json:"endpoint,omitempty"`
	State    string `json:"state,omitempty"`
}

func tokenPath(runDir string) string { return filepath.Join(runDir, "token") }

// present starts the supervisor inside a terminal from the host's
// presentation provider. The run token waits in a private file the
// supervisor removes, so it never appears in a command line or a screen.
func present(ctx context.Context, env HostEnv, req LaunchRequest) (string, error) {
	body, err := json.Marshal(req.Spec)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(req.RunDir, "run.json"), body, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(tokenPath(req.RunDir), []byte(req.Token), 0o600); err != nil {
		return "", err
	}
	command := shellQuote(env.Self) + " _run " + shellQuote(req.RunDir)
	resp, err := presentation(ctx, env, PresentationRequest{Operation: "open", Title: filepath.Base(filepath.Dir(req.RunDir)), Command: command, Workspace: req.Spec.Workspace})
	if err != nil {
		os.Remove(tokenPath(req.RunDir))
		return "", err
	}
	return resp.Endpoint, nil
}

func presentation(ctx context.Context, env HostEnv, req PresentationRequest) (PresentationResponse, error) {
	if env.Plugins == nil {
		return PresentationResponse{}, fault.New("plugin_failed", "host %s has no presentation %q", env.Host, env.Presentation)
	}
	provider := env.Plugins.Provider("presentation", env.Presentation)
	if provider == nil {
		return PresentationResponse{}, fault.New("plugin_failed", "presentation %q is not a declared plugin providing presentation on host %s", env.Presentation, env.Host)
	}
	out, err := env.Plugins.Call(ctx, provider, plugin.Call{Kind: "provide", Type: "presentation", Body: req, Getenv: env.Getenv})
	if err != nil {
		return PresentationResponse{}, fault.New("plugin_failed", "presentation %s: %v", env.Presentation, err)
	}
	var resp PresentationResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return PresentationResponse{}, fault.New("plugin_failed", "presentation %s answered invalidly: %v", env.Presentation, err)
	}
	if req.Operation == "open" && resp.Endpoint == "" {
		return PresentationResponse{}, fault.New("plugin_failed", "presentation %s opened no endpoint", env.Presentation)
	}
	return resp, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readToken takes the run token from the private file a presentation
// launch left, removing it.
func readToken(runDir string) (string, error) {
	body, err := os.ReadFile(tokenPath(runDir))
	if err != nil {
		return "", fmt.Errorf("no run token: %w", err)
	}
	os.Remove(tokenPath(runDir))
	return strings.TrimSpace(string(body)), nil
}
