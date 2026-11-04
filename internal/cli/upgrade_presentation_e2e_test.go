package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/testkit"
)

func TestPresentedSupervisorFinishesThroughInstalledCLI(t *testing.T) {
	v1, v2 := upgradeBinaries(t)
	env := testkit.NewEnv(t)
	env.Bin = v1
	installRelease(t, env, v1)
	env.OK("init", "--host", "m4")
	env.SetCLIPath()
	env.InstallHarnesses()
	env.AppendConfig("[defaults]\nharness = \"codex\"")
	installHerdr(t, env)
	env.OK("repo", "add", testkit.GitRepo(t, "api"))
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"progress", "started"}}, {"wait": "upgrade"}, {"work": "Finished"}, {"report": []string{"result", "completed"}}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Presented upgrade")
	stop := env.StartDaemon()
	started := env.OK("task", "start", "t_1")
	runDir := started["run"].(map[string]any)["dir"].(string)
	env.Eventually("presented v1 supervisor and progress", func() bool {
		return len(callsFor(env, "t_1", 0)) == 1 && env.Task("t_1")["state"] == "running" && env.OK("task", "log", "t_1")["run"].(map[string]any)["liveness"] == "live"
	})
	pane := herdrPanes(env)[0]
	if !strings.Contains(pane["command"].(string), v1) {
		t.Fatalf("presentation did not pin original _run: %v", pane)
	}
	stop()
	installRelease(t, env, v2)
	env.Bin = v2
	env.OK("task", "show", "t_1")
	env.StartDaemon()
	if migrationVersion(t, env) != 7 {
		t.Fatal("presented migration did not apply")
	}
	if err := os.WriteFile(filepath.Join(env.Home, ".fakeharness", "upgrade"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env.WaitState("t_1", "done")
	env.Eventually("original presentation closed", func() bool { return herdrPanes(env)[0]["closed"] == true })
	if _, err := os.Stat(filepath.Join(runDir, "exit.json")); err != nil {
		t.Fatalf("original presented supervisor left no exit record: %v", err)
	}
	if len(callsFor(env, "t_1", 1)) != 0 || len(commandErrors(env, "t_1")) != 0 {
		t.Fatalf("nudge or command failure: %v", env.Calls())
	}
}
