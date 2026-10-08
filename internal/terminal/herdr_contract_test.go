package terminal

import (
	"strings"
	"testing"
)

func TestPaneProcessAndReadFixtures(t *testing.T) {
	runner := &fixtureRunner{responses: map[string][]fixtureResponse{
		"pane get w7:p2":                 {{stdout: `{"result":{"pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7"}}}`}, {stdout: `{"result":{"pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7"}}}`}},
		"pane process-info --pane w7:p2": {{stdout: `{"result":{"process_info":{"foreground_process_group_id":123,"foreground_processes":[{"pid":123,"name":"shephrd","cmdline":"shephrd _run attempt"}],"pane_id":"w7:p2","shell_pid":123}}}`}},
		"pane read w7:p2 --source recent-unwrapped --lines 200 --format text": {{stdout: "Shephrd output\n"}},
	}}
	client := NewWithRunner("/socket", runner)
	info, err := client.ProcessInfo(Endpoint{SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"})
	if err != nil || info.ForegroundProcessGroup != 123 || len(info.ForegroundProcesses) != 1 {
		t.Fatalf("process info = %+v, err = %v", info, err)
	}
	output, err := client.ReadPane(Endpoint{SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}, 200)
	if err != nil || !strings.Contains(output, "Shephrd output") {
		t.Fatalf("output = %q, err = %v", output, err)
	}
}
