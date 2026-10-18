package cli_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/proc"
	"shephrd/internal/testkit"

	_ "modernc.org/sqlite"
)

func upgradeBinaries(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	legacy := t.TempDir()
	for _, name := range []string{"go.mod", "go.sum"} {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacy, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.MkdirAll(filepath.Join(legacy, rel), 0o755)
			}
			if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "0002_landing_delta.sql") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(legacy, rel), body, 0o644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	migrations := filepath.Join(legacy, "internal", "store", "migrations")
	base := filepath.Join(migrations, "0001_init.sql")
	first := mustRead(t, base)
	current := mustRead(t, filepath.Join(root, "internal", "store", "migrations", "0002_landing_delta.sql"))
	if err := os.WriteFile(base, []byte(first+"\n"+current), 0o644); err != nil {
		t.Fatal(err)
	}
	bins := t.TempDir()
	build := func(src, release string) string {
		path := filepath.Join(bins, release, "shephrd")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("go", "build", "-ldflags=-X shephrd/internal/version.Release="+release, "-o", path, "./cmd/shephrd")
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", release, err, out)
		}
		return path
	}
	v1 := build(legacy, "fixture-v1")
	if err := os.WriteFile(filepath.Join(migrations, "0002_upgrade_probe.sql"), []byte("ALTER TABLE landings ADD COLUMN upgrade_probe TEXT NOT NULL DEFAULT '';\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return v1, build(legacy, "fixture-v2")
}

func mustReadIfExists(path string) string {
	body, _ := os.ReadFile(path)
	return string(body)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func installRelease(t *testing.T, env *testkit.Env, binary string) {
	t.Helper()
	link := env.Installed + ".next"
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(link, env.Installed); err != nil {
		t.Fatal(err)
	}
}

func migrationVersion(t *testing.T, env *testkit.Env) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(env.Home, ".local", "state", "shephrd", "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestActiveRunSurvivesStoreMigration(t *testing.T) {
	v1, v2 := upgradeBinaries(t)
	for _, host := range []string{"m4", "workhorse"} {
		t.Run(host, func(t *testing.T) {
			env := testkit.NewEnv(t)
			env.Bin = v1
			profile := filepath.Join(env.Home, "etc", "profiles", "per-user", "harrison", "bin")
			if host == "workhorse" {
				profile = filepath.Join(env.Home, ".nix-profile", "bin")
			}
			if err := os.MkdirAll(profile, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(profile, "shephrd")
			if err := os.Symlink(v1, path); err != nil {
				t.Fatal(err)
			}
			env.Installed = path
			env.Path = append([]string{profile}, env.Path...)
			env.OK("init", "--host", host)
			env.SetCLIPath()
			env.InstallHarnesses()
			env.AppendConfig("[defaults]\nharness = \"codex\"")
			env.OK("repo", "add", testkit.GitRepo(t, "api"))
			pluginDir := filepath.Join(env.Home, "plugin-package", "plugins", "echoer")
			testkit.WritePlugin(t, pluginDir, "echoer", "[commands]\nechoer = \"Callback\"\n")
			ready := filepath.Join(env.Home, "plugin-calls")
			unblock := filepath.Join(env.Home, "plugin-unblock")
			env.AppendConfig("[packages.tools]\npath = \"" + filepath.Join(env.Home, "plugin-package") + "\"\n[plugins.echoer]\npackage = \"tools\"\n[plugins.echoer.options]\nshephrd = \"shephrd\"\nwait = \"" + unblock + "\"\nlog = \"" + ready + "\"")
			env.Script(map[string][][]action{"t_1": {{{"report": []string{"progress", "v1 active"}}, {"wait": "upgrade"}, {"shephrd": []string{"report", "progress", "v2 active"}}, {"work": "Upgrade validated"}, {"report": []string{"result", "finished"}}}}})
			env.OK("task", "create", "--repo", "api", "--objective", "Upgrade")
			stop := env.StartDaemon()
			started := env.OK("task", "start", "t_1")
			run := started["run"].(map[string]any)
			runDir := run["dir"].(string)
			env.Eventually("v1 progress from active harness", func() bool {
				return len(env.Calls()) >= 2 && env.Task("t_1")["state"] == "running"
			})
			if migrationVersion(t, env) != 1 {
				t.Fatal("v1 migrated beyond its schema")
			}
			var spec struct {
				Shephrd string `json:"shephrd"`
			}
			raw, _ := os.ReadFile(filepath.Join(runDir, "run.json"))
			if err := json.Unmarshal(raw, &spec); err != nil || spec.Shephrd != path {
				t.Fatalf("run.json installed CLI: %s, %v", raw, err)
			}
			if !strings.Contains(callsFor(env, "t_1", 0)[0]["brief"].(string), "Run Shephrd as `"+path+"`") {
				t.Fatal("brief did not advertise installed CLI")
			}
			pid := int(run["pid"].(float64))
			identity, err := proc.Of(pid)
			if err != nil || identity.Probe() != "live" {
				t.Fatalf("v1 supervisor %d: %v", pid, err)
			}
			pluginCmd := exec.Command(v1, "echoer", "callback", "repo", "list")
			pluginCmd.Env = []string{"HOME=" + env.Home, "PATH=" + strings.Join(env.Path, ":") + ":" + os.Getenv("PATH"), "USER=" + os.Getenv("USER")}
			var pluginOutput, pluginError bytes.Buffer
			pluginCmd.Stdout, pluginCmd.Stderr = &pluginOutput, &pluginError
			if err := pluginCmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { pluginCmd.Process.Kill() })
			env.Eventually("the old caller's plugin to be held", func() bool { return strings.Contains(mustReadIfExists(ready), `"token":true`) })
			stop()
			installRelease(t, env, v2)
			env.Bin = v2
			env.StartDaemon()
			env.OK("task", "show", "t_1")
			if migrationVersion(t, env) != 2 {
				t.Fatal("v2 did not migrate the store")
			}
			old := env.Bin
			env.Bin = v1
			env.Refused("store_too_new", "task", "show", "t_1")
			env.Bin = old
			if identity.Probe() != "live" {
				t.Fatal("migration lost original supervisor")
			}
			if err := os.WriteFile(unblock, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := pluginCmd.Wait(); err != nil || !strings.Contains(pluginOutput.String(), `"repos"`) {
				t.Fatalf("held plugin callback after migration: %v, %s, %s", err, pluginOutput.String(), pluginError.String())
			}
			if err := os.WriteFile(filepath.Join(env.Home, ".fakeharness", "upgrade"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			env.WaitState("t_1", "done")
			if _, ok := os.Stat(filepath.Join(runDir, "exit.json")); ok != nil {
				t.Fatalf("original supervisor left no exit record: %v", ok)
			}
			if log := env.OK("task", "log", "t_1")["run"].(map[string]any); log["liveness"] != "exited" || log["turn_reported"] != true || log["pid"] != float64(pid) {
				t.Fatalf("exit identity and report: %v; harness calls: %v; supervisor: %s", log, env.Calls(), mustRead(t, filepath.Join(runDir, "supervisor.log")))
			}
			if errs := commandErrors(env, "t_1"); len(errs) != 0 {
				t.Fatalf("existing harness command failures: %v", errs)
			}
		})
	}
}
