package cli_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"shephrd/internal/cli"
	"shephrd/internal/fault"
	"shephrd/internal/testkit"
)

// machines builds the three-machine example: workhorse is the home host,
// hermes is a client host for the main driver, laptop is a worker host.
type machines struct {
	home, client, laptop *testkit.Env
}

func threeMachines(t *testing.T) *machines {
	m := &machines{home: initialized(t), client: testkit.NewEnv(t), laptop: testkit.NewEnv(t)}
	m.home.InstallHarnesses()
	m.home.AppendConfig("[defaults]\nharness = \"codex\"\n\n[hosts.laptop]\nssh = \"me@laptop\"")
	m.client.OK("init", "--home", "me@workhorse")
	m.client.SetCLIPath()
	m.laptop.OK("init", "--host", "laptop", "--home", "me@workhorse")
	m.laptop.SetCLIPath()
	bin := m.home.Bin
	m.client.InstallSSH(map[string]testkit.SSHTarget{"me@workhorse": {Command: []string{bin, "serve", "--as", "driver:main"}, Home: m.home.Home}})
	m.laptop.InstallSSH(map[string]testkit.SSHTarget{"me@workhorse": {Command: []string{bin, "serve", "--host", "laptop"}, Home: m.home.Home}})
	m.home.InstallSSH(map[string]testkit.SSHTarget{"me@laptop": {Command: []string{bin, "agent"}, Home: m.laptop.Home}})
	return m
}

func TestClientCommandsMatchLocalOnes(t *testing.T) {
	m := threeMachines(t)
	m.home.OK("repo", "add", testkit.GitRepo(t, "api"))
	local, remote := m.home.Run("", "repo", "list"), m.client.Run("", "repo", "list")
	if remote.Code != 0 || remote.Stdout != local.Stdout {
		t.Fatalf("parity:\nlocal  %s\nremote %+v", local.Stdout, remote)
	}
	created := m.client.Run("Objective read from stdin", "task", "create", "--repo", "api", "--objective", "-")
	if created.Code != 0 || created.JSON(t)["objective"] != "Objective read from stdin" || created.JSON(t)["driver"] != "driver:main" {
		t.Fatalf("remote create: %+v", created)
	}
	if tasks := m.home.OK("task", "list")["tasks"].([]any); len(tasks) != 1 {
		t.Fatalf("home tasks: %v", tasks)
	}
	m.client.Refused("operator_only", "repo", "add", testkit.GitRepo(t, "web"))
	m.client.Refused("as_not_allowed", "task", "list", "--as", "driver:other")
	var refusal map[string]any
	for _, e := range m.home.OK("events")["events"].([]any) {
		if e.(map[string]any)["name"] == "request.refused" {
			refusal = e.(map[string]any)["data"].(map[string]any)
		}
	}
	if refusal["command"] != "repo add" || refusal["kind"] != "operator_only" || refusal["caller"] != "driver:main" {
		t.Fatalf("request.refused: %v", refusal)
	}
	m.client.SSHDown("me@workhorse", true)
	if out := m.client.OK("version"); out["protocol"] != float64(cli.Protocol) {
		t.Fatalf("local version: %v", out)
	}
}

func TestDroppedConnectionReturnsTheStoredResult(t *testing.T) {
	m := threeMachines(t)
	m.home.OK("repo", "add", testkit.GitRepo(t, "api"))
	m.client.SSHDropOnce("me@workhorse")
	r := m.client.Run("", "task", "create", "--repo", "api", "--objective", "Exactly once", "--key", "k1")
	if r.Code != 0 || r.JSON(t)["id"] != "t_1" {
		t.Fatalf("retried create: %+v", r)
	}
	if tasks := m.home.OK("task", "list")["tasks"].([]any); len(tasks) != 1 {
		t.Fatalf("tasks after a dropped connection: %v", tasks)
	}
}

func TestUnreachableHomeIsAnUnknownOutcome(t *testing.T) {
	m := threeMachines(t)
	m.client.SSHDown("me@workhorse", true)
	r := m.client.Run("", "task", "list")
	if r.Code != 3 || !strings.Contains(r.Stderr, "transport_failed") {
		t.Fatalf("unreachable home: %+v", r)
	}
}

func remoteInboxStream(t *testing.T, env *testkit.Env, after string) (func() string, func()) {
	t.Helper()
	cmd := exec.Command(env.Bin, "inbox", "wait", "--after", after)
	cmd.Env = []string{"HOME=" + env.Home, "PATH=" + strings.Join(env.Path, ":") + ":" + os.Getenv("PATH"), "USER=" + os.Getenv("USER")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := filepath.Join(t.TempDir(), "stream")
	file, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = file, file
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			cmd.Wait()
			file.Close()
		})
	}
	t.Cleanup(stop)
	return func() string { body, _ := os.ReadFile(out); return string(body) }, stop
}

func TestRemoteInboxWaitReplaysPendingWithoutDeliveryConfig(t *testing.T) {
	m := threeMachines(t)
	m.home.OK("repo", "add", testkit.GitRepo(t, "api"))
	m.home.Script(map[string][][]action{"t_1": {{{"report": []string{"blocker", "Need help"}}}}})
	m.home.OK("task", "create", "--repo", "api", "--deliverable", "report", "--objective", "Ready")
	m.home.OK("task", "start", "t_1")
	m.home.WaitState("t_1", "held")
	if items := m.client.OK("inbox")["items"].([]any); len(items) != 1 {
		t.Fatalf("remote inbox: %v", items)
	}
	first, interrupt := remoteInboxStream(t, m.client, "0")
	m.client.Eventually("first item over SSH", func() bool { return strings.Contains(first(), `"id":1`) })
	interrupt()
	m.client.SSHDown("me@workhorse", true)
	if r := m.client.Run("", "inbox", "wait", "--after", "1"); r.Code != 3 {
		t.Fatalf("interrupted remote stream: %+v", r)
	}
	m.client.SSHDown("me@workhorse", false)
	read, _ := remoteInboxStream(t, m.client, "1")
	m.client.Eventually("pending item replay after SSH reconnect", func() bool {
		return strings.Contains(read(), `"id":1`)
	})
	if got := strings.Count(read(), `"id":1`); got != 1 {
		t.Fatalf("duplicate ID on the same connection: %s", read())
	}
	m.client.OK("inbox", "ack", "1")
	if items := m.client.OK("inbox")["items"].([]any); len(items) != 0 {
		t.Fatalf("item remained after explicit ack: %v", items)
	}
	acked, _ := remoteInboxStream(t, m.client, "1")
	if got := acked(); got != "" {
		t.Fatalf("acknowledged item was replayed: %s", got)
	}
}

func TestRemoteInboxWaitIdleNewItemAndDriverIsolation(t *testing.T) {
	m := threeMachines(t)
	m.home.OK("repo", "add", testkit.GitRepo(t, "api"))
	read, _ := remoteInboxStream(t, m.client, "0")
	time.Sleep(600 * time.Millisecond)
	if got := read(); got != "" {
		t.Fatalf("idle watch emitted output: %s", got)
	}
	other := testkit.NewEnv(t)
	other.OK("init", "--home", "me@workhorse")
	other.InstallSSH(map[string]testkit.SSHTarget{"me@workhorse": {Command: []string{m.home.Bin, "serve", "--as", "driver:other"}, Home: m.home.Home}})
	otherRead, _ := remoteInboxStream(t, other, "0")
	m.home.Script(map[string][][]action{"t_1": {{{"report": []string{"blocker", "Need help"}}}}})
	m.home.OK("task", "create", "--repo", "api", "--objective", "Ready")
	m.home.OK("task", "start", "t_1")
	m.home.WaitState("t_1", "held")
	m.client.Eventually("new remote item", func() bool { return strings.Contains(read(), `"id":1`) })
	if got := strings.Count(read(), `"id":1`); got != 1 {
		t.Fatalf("duplicate item on live watch: %s", read())
	}
	if got := otherRead(); got != "" {
		t.Fatalf("wrong driver received item: %s", got)
	}
	other.Refused("not_found", "inbox", "ack", "1")
	if items := m.client.OK("inbox")["items"].([]any); len(items) != 1 {
		t.Fatalf("wrong driver's ack settled the item: %v", items)
	}
}

func TestWorkerHostRunsSessionsAndLandsCode(t *testing.T) {
	m := threeMachines(t)
	repo := testkit.GitRepo(t, "api")
	m.home.AppendConfig("[repos.api.landing]\nmode = \"direct\"")
	added := m.home.OK("repo", "add", repo, "--host", "laptop")
	if added["host"] != "laptop" {
		t.Fatalf("repo add: %v", added)
	}
	hosts := m.home.OK("host", "list")["hosts"].([]any)
	if laptop := hosts[1].(map[string]any); laptop["name"] != "laptop" || laptop["reachable"] != true || len(laptop["harnesses"].([]any)) != 3 {
		t.Fatalf("hosts: %v", hosts)
	}
	m.home.OK("task", "create", "--repo", "api", "--objective", "Fix it on the laptop")
	started := m.home.OK("task", "start", "t_1")
	workspace := started["attempt"].(map[string]any)["workspace"].(string)
	if !strings.HasPrefix(workspace, m.laptop.Home) {
		t.Fatalf("workspace is not on the laptop: %s", workspace)
	}
	m.home.WaitState("t_1", "done")
	if calls := m.laptop.Calls(); len(calls) == 0 || calls[0]["cwd"] != workspace {
		t.Fatalf("laptop harness calls: %v", calls)
	}
	if log := m.home.OK("task", "log", "t_1"); !strings.Contains(log["log"].(string), "thread.started") {
		t.Fatalf("remote log: %v", log)
	}
	commit := m.home.OK("task", "show", "t_1")["artifact"].(map[string]any)["commit"]
	m.home.OK("task", "deliver", "t_1")
	if head := testkit.Git(t, repo, "rev-parse", "refs/heads/main"); head != commit {
		t.Fatalf("main is %s, want %s", head, commit)
	}
}

func TestRunTokensAreBoundToTheirHost(t *testing.T) {
	m := threeMachines(t)
	m.home.OK("repo", "add", testkit.GitRepo(t, "api"), "--host", "laptop")
	m.laptop.Script(map[string][][]action{"t_1": {{{"sleep": "60s"}}}})
	m.home.OK("task", "create", "--repo", "api", "--objective", "Work")
	m.home.OK("task", "start", "t_1")
	m.home.Eventually("the laptop session", func() bool { return len(m.laptop.Calls()) > 0 })
	token := envOf(m.laptop.Calls()[0])["SHEPHRD_RUN_TOKEN"]
	getenv := func(key string) string {
		if key == "HOME" {
			return m.home.Home
		}
		return ""
	}
	report := func(host string) error {
		req := cli.Request{V: cli.Protocol, Argv: []string{"report", "progress", "from " + host}, Key: host, RunToken: token}
		_, err := cli.Dispatch(context.Background(), cli.Origin{Host: host}, req, getenv, io.Discard)
		return err
	}
	if err := report("elsewhere"); fault.As(err).Kind != "invalid_token" {
		t.Fatalf("token from another host: %v", err)
	}
	if err := report("laptop"); err != nil {
		t.Fatalf("token from its own host: %v", err)
	}
}

func TestSleepingWorkerHostLeavesRunsUnknown(t *testing.T) {
	m := threeMachines(t)
	m.home.OK("repo", "add", testkit.GitRepo(t, "api"), "--host", "laptop")
	m.laptop.Script(map[string][][]action{"t_1": {{{"sleep": "60s"}}}})
	m.home.OK("task", "create", "--repo", "api", "--objective", "Work")
	m.home.OK("task", "start", "t_1")
	m.home.Eventually("the laptop session", func() bool { return len(m.laptop.Calls()) > 0 })
	m.home.SSHDown("me@laptop", true)
	runs := m.home.OK("workspace", "reconcile")["runs"].([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["liveness"] != "unknown" {
		t.Fatalf("reconcile with the laptop asleep: %v", runs)
	}
	m.home.Refused("stop_unconfirmed", "task", "stop", "t_1")
	m.home.Refused("liveness_unknown", "task", "retry", "t_1")
	m.home.SSHDown("me@laptop", false)
	m.home.OK("task", "stop", "t_1")
	m.home.OK("task", "retry", "t_1")
}
