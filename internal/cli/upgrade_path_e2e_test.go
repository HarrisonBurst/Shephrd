package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"shephrd/internal/testkit"
)

func TestInstalledCLIPathFailsClosedBeforeNewRuns(t *testing.T) {
	for _, tc := range []struct {
		name, target, kind string
	}{
		{"missing", "missing", "invalid_config"},
		{"dangling", "dangling", "invalid_config"},
		{"not executable", "not-executable", "invalid_config"},
		{"wrong release", "wrong-release", "version_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := withHarness(t)
			env.OK("task", "create", "--repo", "api", "--objective", "Not yet started")
			if err := os.Remove(env.Installed); err != nil {
				t.Fatal(err)
			}
			switch tc.target {
			case "dangling":
				if err := os.Symlink(filepath.Join(env.Home, "absent"), env.Installed); err != nil {
					t.Fatal(err)
				}
			case "not-executable":
				if err := os.WriteFile(env.Installed, []byte("not executable"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "wrong-release":
				_, v2 := upgradeBinaries(t)
				if err := os.Symlink(v2, env.Installed); err != nil {
					t.Fatal(err)
				}
			}
			env.Refused(tc.kind, "task", "start", "t_1")
			if task := env.Task("t_1"); task["state"] != "queued" {
				t.Fatalf("failed launch changed task: %v", task)
			}
		})
	}
}

func TestMissingInstalledPathDoesNotDeclareActiveRunExited(t *testing.T) {
	env, _ := withHarness(t)
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"progress", "running"}}, {"wait": "upgrade"}, {"work": "Recovered"}, {"report": []string{"result", "recovered"}}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Recover")
	env.OK("task", "start", "t_1")
	env.Eventually("initial progress", func() bool { return len(env.Calls()) >= 2 })
	if err := os.Remove(env.Installed); err != nil {
		t.Fatal(err)
	}
	env.OK("task", "create", "--repo", "api", "--objective", "Another run")
	env.Refused("invalid_config", "task", "start", "t_2")
	if run := env.OK("task", "log", "t_1")["run"].(map[string]any); run["liveness"] != "live" {
		t.Fatalf("missing CLI treated active run as exited: %v", run)
	}
	installRelease(t, env, env.Bin)
	if err := os.WriteFile(filepath.Join(env.Home, ".fakeharness", "upgrade"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env.WaitState("t_1", "done")
}

func TestMissingInstalledPathBlocksPluginCallback(t *testing.T) {
	env, _ := withPlugins(t)
	if err := os.Remove(env.Installed); err != nil {
		t.Fatal(err)
	}
	env.Refused("plugin_failed", "echoer", "callback", "repo", "list")
}

func TestRelativeInstalledPathIsInvalidConfiguration(t *testing.T) {
	env := testkit.NewEnv(t)
	env.OK("init", "--host", "workhorse")
	body, err := os.ReadFile(env.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.ConfigPath(), append([]byte("cli_path = \"relative/shephrd\"\n"), body...), 0o600); err != nil {
		t.Fatal(err)
	}
	env.Refused("invalid_config", "repo", "list")
}
