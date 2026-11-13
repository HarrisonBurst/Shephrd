package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"shephrd/internal/fault"
	"shephrd/internal/plugin"
)

type NoDispatchProof struct {
	Task       int64  `json:"task"`
	Attempt    int64  `json:"attempt"`
	Run        int64  `json:"run"`
	Generation int64  `json:"generation"`
	Host       string `json:"host"`
	Dir        string `json:"dir"`
}

type LaunchResponse struct {
	PID      int    `json:"pid,omitempty"`
	Start    string `json:"start,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// PresentationRequest is the presentation provider contract: open a
// terminal running a command, probe an endpoint, or focus it.
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

func tokenPath(runDir string) string      { return filepath.Join(runDir, "token") }
func endpointPath(runDir string) string   { return filepath.Join(runDir, "endpoint") }
func noDispatchPath(runDir string) string { return filepath.Join(runDir, "run-not-dispatched.json") }

func writeNoDispatch(runDir string, proof NoDispatchProof) error {
	body, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(runDir, ".run-not-dispatched-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), noDispatchPath(runDir)); err != nil {
		return err
	}
	dir, err := os.Open(runDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		os.Remove(noDispatchPath(runDir))
		return err
	}
	return nil
}

func readNoDispatch(runDir string) *NoDispatchProof {
	body, err := os.ReadFile(noDispatchPath(runDir))
	if err != nil {
		return nil
	}
	var proof NoDispatchProof
	if json.Unmarshal(body, &proof) != nil {
		return nil
	}
	return &proof
}

// present starts the supervisor inside a terminal from the host's
// presentation provider. The run token waits in a private file the
// supervisor removes, so it never appears in a command line or a screen.
func present(ctx context.Context, env HostEnv, req LaunchRequest) (string, error) {
	if err := os.Remove(noDispatchPath(req.RunDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
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
	command := "clear; exec " + shellQuote(env.Self) + " _run " + shellQuote(req.RunDir)
	resp, err := presentation(ctx, env, PresentationRequest{Operation: "open", Title: req.Title, Workspace: req.Spec.Workspace})
	if err != nil {
		if removeErr := os.Remove(tokenPath(req.RunDir)); removeErr != nil {
			return "", fmt.Errorf("%w: revoke run token: %v", err, removeErr)
		}
		if proofErr := writeNoDispatch(req.RunDir, req.Proof); proofErr != nil {
			return "", fmt.Errorf("%w: record no-dispatch evidence: %v", err, proofErr)
		}
		return "", err
	}
	if err := os.WriteFile(endpointPath(req.RunDir), []byte(resp.Endpoint), 0o600); err != nil {
		return "", err
	}
	_, err = presentation(ctx, env, PresentationRequest{Operation: "run", Command: command, Endpoint: resp.Endpoint})
	return resp.Endpoint, err
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
