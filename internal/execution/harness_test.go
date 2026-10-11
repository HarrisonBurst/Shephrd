package execution

import (
	"slices"
	"testing"
)

func TestCodexInteractiveCommandUsesForegroundCLI(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request HarnessRequest
		want    []string
	}{
		{
			name: "new",
			request: HarnessRequest{
				Mode: "start", Interactive: true, Workspace: "/workspace", Prompt: "brief",
			},
			want: []string{
				"codex", "--dangerously-bypass-approvals-and-sandbox", "-C", "/workspace", "brief",
			},
		},
		{
			name: "resume",
			request: HarnessRequest{
				Mode: "resume", Interactive: true, Session: "session", Prompt: "brief",
			},
			want: []string{
				"codex", "resume", "--dangerously-bypass-approvals-and-sandbox", "session", "brief",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := codex(tc.request).Command
			if slices.Contains(got, "--no-daemon") {
				t.Fatalf("unsupported Codex flag in command: %v", got)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("command = %v, want %v", got, tc.want)
			}
		})
	}
}
