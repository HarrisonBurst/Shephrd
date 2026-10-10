package cli_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/cli"
	"shephrd/internal/testkit"
)

func withPlugins(t *testing.T) (*testkit.Env, string) {
	t.Helper()
	env := initialized(t)
	pkg, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testkit.WritePlugin(t, filepath.Join(pkg, "plugins", "echoer"), "echoer", "skills = [\"skills/echoer.md\"]\n[commands]\nechoer = \"Echo the request\"\n")
	os.MkdirAll(filepath.Join(pkg, "plugins", "echoer", "skills"), 0o755)
	os.WriteFile(filepath.Join(pkg, "plugins", "echoer", "skills", "echoer.md"), []byte("# Echoer\n"), 0o644)
	testkit.WritePlugin(t, filepath.Join(pkg, "plugins", "ghost"), "ghost", "[commands]\nghost = \"Never declared\"\n")
	testkit.WritePlugin(t, filepath.Join(pkg, "plugins", "repo"), "repo", "[commands]\nrepo = \"Shadow a core command\"\n")
	env.AppendConfig(`[packages.tools]
path = "` + pkg + `"

[plugins.echoer]
package = "tools"

[plugins.echoer.options]
shephrd = "` + env.Bin + `"

[plugins.repo]
package = "tools"`)
	return env, pkg
}

func TestPluginCommandRunsWithCallerIdentity(t *testing.T) {
	env, _ := withPlugins(t)
	r := env.Run("long input", "echoer", "say", "-")
	if r.Code != 0 {
		t.Fatalf("echoer: %+v", r)
	}
	echo := r.JSON(t)["echo"].(map[string]any)
	if strings.Join(toStrings(echo["argv"]), " ") != "say -" || echo["stdin"] != "long input" {
		t.Fatalf("echo: %v", echo)
	}
	if caller := echo["caller"].(map[string]any); caller["kind"] != "driver" || caller["name"] != "main" {
		t.Fatalf("caller: %v", caller)
	}
}

func TestPluginCommandCallsBackAsItsCaller(t *testing.T) {
	env, _ := withPlugins(t)
	env.OK("repo", "add", testkit.GitRepo(t, "tool"))
	out := env.OK("echoer", "callback", "repo", "list")
	if repos := out["repos"].([]any); len(repos) != 1 {
		t.Fatalf("callback: %v", out)
	}
	added := env.OK("echoer", "callback", "repo", "add", testkit.GitRepo(t, "second"))
	if added["id"] != "r_2" {
		t.Fatalf("operator callback: %v", added)
	}
	env.Vars["SHEPHRD_CALL_TOKEN"] = "forged"
	env.Refused("invalid_token", "repo", "list")
}

func TestPluginCommandFailures(t *testing.T) {
	env, _ := withPlugins(t)
	env.Refused("bad_input", "echoer", "fail")
	env.Refused("plugin_unknown", "ghost")
	env.Refused("plugin_unknown", "nothing")
	if r := env.Run("", "repo", "list"); r.Code != 0 {
		t.Fatalf("core command shadowed: %+v", r)
	}
}

func TestPluginListStatusAndSkill(t *testing.T) {
	env, _ := withPlugins(t)
	plugins := env.OK("plugin", "list")["plugins"].([]any)
	if len(plugins) != 2 {
		t.Fatalf("plugin list: %v", plugins)
	}
	status := map[string]map[string]any{}
	for _, row := range env.OK("plugin", "status")["plugins"].([]any) {
		status[row.(map[string]any)["name"].(string)] = row.(map[string]any)
	}
	if status["echoer"]["available"] != true || status["repo"]["available"] != false ||
		!strings.Contains(status["repo"]["unavailable"].(string), "core command") {
		t.Fatalf("plugin status: %v", status)
	}
	skills := env.OK("plugin", "skill", "echoer")["skills"].([]any)
	if len(skills) != 1 || skills[0].(map[string]any)["content"] != "# Echoer\n" {
		t.Fatalf("plugin skill: %v", skills)
	}
	env.Refused("plugin_failed", "plugin", "skill", "repo")
	env.Refused("plugin_unknown", "plugin", "skill", "ghost")
}

func TestPluginSyncIsOperatorOnly(t *testing.T) {
	env, _ := withPlugins(t)
	if out := env.OK("plugin", "sync"); len(out["packages"].([]any)) != 1 {
		t.Fatalf("plugin sync: %v", out)
	}
	r := env.Run("", "echoer", "callback", "plugin", "sync")
	if r.Code != 0 {
		t.Fatalf("operator's plugin command may sync: %+v", r)
	}
}

func TestEventsReadAndFollow(t *testing.T) {
	env := initialized(t)
	env.OK("repo", "add", testkit.GitRepo(t, "tool"))
	out := env.OK("events")
	events := out["events"].([]any)
	if len(events) != 1 || events[0].(map[string]any)["name"] != "repo.added" || out["next"] != float64(1) {
		t.Fatalf("events: %v", out)
	}
	if empty := env.OK("events", "--after", "1")["events"].([]any); len(empty) != 0 {
		t.Fatalf("events after 1: %v", empty)
	}

	getenv := func(key string) string {
		if key == "HOME" {
			return env.Home
		}
		return ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	done := make(chan int)
	go func() {
		req := cli.Request{V: cli.Protocol, Argv: []string{"events", "--follow"}, Key: "k"}
		done <- cli.Run(ctx, cli.Origin{Local: true}, req, getenv, writer, io.Discard)
		writer.Close()
	}()
	lines := bufio.NewScanner(reader)
	if !lines.Scan() {
		t.Fatal("no first event")
	}
	env.OK("repo", "add", testkit.GitRepo(t, "second"))
	if !lines.Scan() {
		t.Fatal("no streamed event")
	}
	var second map[string]any
	json.Unmarshal(lines.Bytes(), &second)
	if second["seq"] != float64(2) {
		t.Fatalf("streamed: %s", lines.Text())
	}
	cancel()
	go io.Copy(io.Discard, reader)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("follow exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not stop")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, item := range v.([]any) {
		out = append(out, item.(string))
	}
	return out
}
