package control

import (
	"fmt"

	"shephrd/internal/model"
	"shephrd/internal/terminal"
)

func (s Service) reconcileEndpoint(attempt model.Attempt, task model.Task) (bool, string, error) {
	if !isTerminalRuntime(attempt.RuntimeBackend) {
		return true, "", nil
	}
	endpoint := s.endpoint(attempt)
	client, err := s.terminalClientFor(attempt)
	if err != nil {
		return false, "terminal client cannot be initialized: " + err.Error(), nil
	}
	state, err := client.Inspect(endpoint)
	if terminal.IsNotFound(err) {
		if s.processAlive(attempt.RunnerPID) {
			return false, "active terminal runner has no recorded endpoint", nil
		}
		return true, "", nil
	}
	if err != nil {
		return false, "terminal endpoint identity cannot be verified: " + err.Error(), nil
	}
	if state.Endpoint.WindowID != endpoint.WindowID || state.Endpoint.WorkspaceID != endpoint.WorkspaceID || state.Endpoint.TabID != endpoint.TabID || state.Endpoint.PaneID != endpoint.PaneID || state.Endpoint.SurfaceID != endpoint.SurfaceID {
		return false, "terminal endpoint identity changed", nil
	}
	if s.processAlive(attempt.RunnerPID) {
		return true, "", nil
	}
	info, err := client.ProcessInfo(endpoint)
	if err != nil {
		return false, "terminal endpoint process state cannot be verified: " + err.Error(), nil
	}
	if foregroundWorker(info, attempt) {
		return false, fmt.Sprintf("terminal endpoint %s still has a foreground worker process", endpoint.PaneID), nil
	}
	if err := client.Close(endpoint); err != nil {
		return false, "close exact terminal endpoint: " + err.Error(), nil
	}
	return true, "", nil
}
