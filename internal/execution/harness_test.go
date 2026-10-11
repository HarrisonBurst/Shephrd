package execution

import (
	"slices"
	"testing"
)

func TestCodexInteractiveCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request HarnessRequest
		want    []string
	}{
		{
			name: "new",
			request: HarnessRequest{
				Mode: "start", Interactive: true, Workspace: "/workspace", Model: "gpt-5", Prompt: "brief",
			},
			want: []string{
				"codex", "--model", "gpt-5", "--dangerously-bypass-approvals-and-sandbox", "-c", `sandbox_mode="danger-full-access"`, "-C", "/workspace", "brief",
			},
		},
		{
			name: "resume",
			request: HarnessRequest{
				Mode: "resume", Interactive: true, Session: "session", Prompt: "brief",
			},
			want: []string{
				"codex", "resume", "--dangerously-bypass-approvals-and-sandbox", "-c", `sandbox_mode="danger-full-access"`, "session", "brief",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codex(tc.request).Command; !slices.Equal(got, tc.want) {
				t.Fatalf("command = %v, want %v", got, tc.want)
			}
		})
	}
}
