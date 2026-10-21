package cli_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/cli"
	"shephrd/internal/fault"
	"shephrd/internal/testkit"
)

func TestUpgradedWorkerUsesItsOwnProfileAndMatchingAgent(t *testing.T) {
	v1, v2 := upgradeBinaries(t)
	m := threeMachines(t)
	m.home.Bin = v2
	installRelease(t, m.home, v2)
	m.laptop.Bin = v1
	installRelease(t, m.laptop, v1)
	m.home.InstallSSH(map[string]testkit.SSHTarget{"me@laptop": {Command: []string{v1, "agent"}, Home: m.laptop.Home}})
	m.laptop.InstallSSH(map[string]testkit.SSHTarget{"me@workhorse": {Command: []string{v2, "serve", "--host", "laptop"}, Home: m.home.Home}})
	m.client.InstallSSH(map[string]testkit.SSHTarget{"me@workhorse": {Command: []string{v2, "serve", "--as", "driver:main"}, Home: m.home.Home}})
	repo := testkit.GitRepo(t, "api")
	m.home.Refused("version_mismatch", "repo", "add", repo, "--host", "laptop")
	installRelease(t, m.laptop, v2)
	m.home.Refused("version_mismatch", "repo", "add", repo, "--host", "laptop")
	m.home.InstallSSH(map[string]testkit.SSHTarget{"me@laptop": {Command: []string{v2, "agent"}, Home: m.laptop.Home}})
	if added := m.home.OK("repo", "add", repo, "--host", "laptop"); added["host"] != "laptop" {
		t.Fatalf("remote host: %v", added)
	}
	m.laptop.Script(map[string][][]action{"t_1": {{{"report": []string{"progress", "remote"}}, {"wait": "upgrade"}, {"work": "Remote"}, {"report": []string{"result", "remote done"}}}}})
	m.home.OK("task", "create", "--repo", "api", "--objective", "Remote upgrade")
	m.home.OK("task", "start", "t_1")
	m.laptop.Eventually("remote progress", func() bool { return len(m.laptop.Calls()) >= 2 })
	call := callsFor(m.laptop, "t_1", 0)[0]
	if !strings.HasPrefix(envOf(call)["PATH"], filepath.Dir(m.laptop.Installed)+":") || !strings.Contains(call["brief"].(string), m.laptop.Installed) {
		t.Fatalf("remote run used another host's path: %v", call)
	}
	token := envOf(call)["SHEPHRD_RUN_TOKEN"]
	getenv := func(key string) string {
		if key == "HOME" {
			return m.home.Home
		}
		return ""
	}
	if _, err := cli.Dispatch(context.Background(), cli.Origin{Host: "elsewhere"}, cli.Request{V: cli.Protocol, Argv: []string{"report", "progress", "stolen"}, Key: "stolen", RunToken: token}, getenv, io.Discard); fault.As(err).Kind != "invalid_token" {
		t.Fatalf("wrong host accepted token: %v", err)
	}
	m.laptop.Bin = v2
	if err := os.WriteFile(filepath.Join(m.laptop.Home, ".fakeharness", "upgrade"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m.home.WaitState("t_1", "done")
}
