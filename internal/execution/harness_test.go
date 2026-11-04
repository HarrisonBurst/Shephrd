package execution

import (
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestCodexCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request HarnessRequest
		want    []string
	}{
		{
			name: "interactive start",
			request: HarnessRequest{
				Mode: "start", Interactive: true, Workspace: `/Users/a/.local/work trees/"quoted"/t_1-1`, Model: "gpt-5", Prompt: "brief",
			},
			want: []string{
				"codex", "--model", "gpt-5", "--dangerously-bypass-approvals-and-sandbox", "-c", `sandbox_mode="danger-full-access"`, "-c", `projects={ "/Users/a/.local/work trees/\"quoted\"/t_1-1" = { trust_level = "trusted" } }`, "-C", `/Users/a/.local/work trees/"quoted"/t_1-1`, "brief",
			},
		},
		{
			name: "interactive resume",
			request: HarnessRequest{
				Mode: "resume", Interactive: true, Workspace: "/workspace", Session: "session", Prompt: "brief",
			},
			want: []string{
				"codex", "resume", "--dangerously-bypass-approvals-and-sandbox", "-c", `sandbox_mode="danger-full-access"`, "-c", `projects={ "/workspace" = { trust_level = "trusted" } }`, "session", "brief",
			},
		},
		{
			name:    "headless start",
			request: HarnessRequest{Mode: "start", Workspace: "/workspace", Prompt: "brief"},
			want:    []string{"codex", "exec", "--dangerously-bypass-approvals-and-sandbox", "-c", `projects={ "/workspace" = { trust_level = "trusted" } }`, "--json", "-C", "/workspace", "brief"},
		},
		{
			name:    "headless resume",
			request: HarnessRequest{Mode: "resume", Workspace: "/workspace", Session: "session", Prompt: "brief"},
			want:    []string{"codex", "exec", "resume", "--dangerously-bypass-approvals-and-sandbox", "-c", `projects={ "/workspace" = { trust_level = "trusted" } }`, "--json", "session", "brief"},
		},
		{
			name:    "escaped path",
			request: HarnessRequest{Mode: "start", Workspace: "/work/back\\slash\nline", Prompt: "brief"},
			want:    []string{"codex", "exec", "--dangerously-bypass-approvals-and-sandbox", "-c", `projects={ "/work/back\\slash\nline" = { trust_level = "trusted" } }`, "--json", "-C", "/work/back\\slash\nline", "brief"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := codex(tc.request).Command
			if !slices.Equal(got, tc.want) {
				t.Fatalf("command = %v, want %v", got, tc.want)
			}
			var config struct {
				Projects map[string]struct {
					TrustLevel string `toml:"trust_level"`
				} `toml:"projects"`
			}
			for i, arg := range got {
				if !strings.HasPrefix(arg, "projects=") {
					continue
				}
				if i == 0 || got[i-1] != "-c" {
					t.Fatalf("override is not a -c argument: %v", got)
				}
				if _, err := toml.Decode(arg, &config); err != nil {
					t.Fatalf("invalid TOML override %q: %v", arg, err)
				}
			}
			if len(config.Projects) != 1 || config.Projects[tc.request.Workspace].TrustLevel != "trusted" {
				t.Fatalf("trust scope = %v, want only %q", config.Projects, tc.request.Workspace)
			}
		})
	}
}

func TestOtherHarnessesUnchanged(t *testing.T) {
	r := HarnessRequest{Mode: "resume", Interactive: true, Workspace: "/workspace", Session: "session", Prompt: "brief"}
	for _, name := range []string{"pi", "claude-code"} {
		if command := Builtin[name](r).Command; slices.Contains(command, "-c") || slices.ContainsFunc(command, func(arg string) bool { return strings.HasPrefix(arg, "projects=") }) {
			t.Fatalf("%s command unexpectedly has Codex trust override: %v", name, command)
		}
	}
}
