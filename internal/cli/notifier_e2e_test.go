package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/testkit"
)

func TestMacOSNotifierAnnouncesInboxItems(t *testing.T) {
	env, _ := withDaemon(t)
	bin := filepath.Join(env.Home, "osabin")
	os.MkdirAll(bin, 0o755)
	log := filepath.Join(env.Home, "notifications")
	os.WriteFile(filepath.Join(bin, "osascript"), []byte("#!/bin/sh\nprintf '%s\\n' \"$2\" >> "+log+"\n"), 0o755)
	env.Path = append([]string{bin}, env.Path...)
	pkg, _ := filepath.EvalSymlinks(t.TempDir())
	plugin := filepath.Join(pkg, "plugins", "macos-notify")
	os.MkdirAll(filepath.Join(plugin, "bin"), 0o755)
	exe, _ := os.ReadFile(testkit.Tool(t, "./plugins/macos-notify", "shephrd-macos-notify"))
	os.WriteFile(filepath.Join(plugin, "bin", "shephrd-macos-notify"), exe, 0o755)
	manifest, _ := os.ReadFile(filepath.Join("..", "..", "plugins", "macos-notify", "plugin.toml"))
	os.WriteFile(filepath.Join(plugin, "plugin.toml"), manifest, 0o644)
	env.AppendConfig(`[packages.firstparty]
path = "` + pkg + `"

[plugins.macos-notify]
package = "firstparty"`)
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"question", "Which version?"}}}}})
	env.StartDaemon()
	env.OK("task", "create", "--repo", "api", "--objective", "Pick a \"version\"")
	env.OK("task", "start", "t_1")
	env.Eventually("a notification", func() bool {
		body, _ := os.ReadFile(log)
		return strings.Contains(string(body), `t_1: Pick a \"version\"`) && strings.Contains(string(body), "waiting")
	})
}
