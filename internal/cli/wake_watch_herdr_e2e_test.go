package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"shephrd/internal/model"
	"shephrd/internal/process"
	"shephrd/internal/store"
)

const herdrWatchHarness = `#!/usr/bin/env python3
import os,sys,json,socket,subprocess,time
args=sys.argv[1:]
assert '--extension' in args and os.path.isfile(args[args.index('--extension')+1])
assert args[args.index('--model')+1]=='retained-model'
assert os.environ['SHEPHRD_PI_WATCHER_ENABLED']=='0' and os.environ.get('SHEPHRD_WORKER','')==''
root=os.environ['FIXTURE_ROOT']
exe=os.environ['SHEPHRD_EXECUTABLE']
cid=os.environ['SHEPHRD_SUBDRIVER_ID']
def cli(*a):
 p=subprocess.run([exe,*a,'--json'],capture_output=True,text=True)
 assert p.returncode==0,(a,p.stdout,p.stderr)
 return json.loads(p.stdout)
def note(name,value):
 with open(os.path.join(root,name),'a') as f:f.write(json.dumps(value)+'\n')
note('turns',{'pane':os.environ.get('HERDR_PANE_ID',''),'generation':os.environ['SHEPHRD_SUBDRIVER_GENERATION']})
for e in cli('subdriver','inspect',cid)['events']:
 if e['kind']=='reply':
  open(os.path.join(root,'ready'),'w').close()
  deadline=time.time()+30
  while not os.path.exists(os.path.join(root,'finish-turn')):
   assert time.time()<deadline,'finish gate timed out'
   time.sleep(.01)
  cli('subdriver','return',e['request_id'],'Finished with '+e['payload'],'--key','result','--kind','result')
 else:
  task=cli('subdriver','dispatch',e['request_id'],'Child worker','--key','child','--feature','herdr-child','--deliverable','code')
  cli('worker','spawn',task['id'])
  note('children',task['id'])
  cli('subdriver','return',e['request_id'],'Which option?','--key','question','--kind','question')
 cli('subdriver','handled',str(e['id']))
def event(v):return '<shephrd-event>'+json.dumps(v)+'</shephrd-event>'
cp={'schema_version':1,'summary':'Herdr watch turn','completed':[],'next_steps':[],'decisions':[],'changed_paths':[],'checks':[],'blockers':[]}
envelope=event({'type':'checkpoint','payload':'checkpoint','checkpoint':cp})+'\n'+event({'type':'done','payload':'Turn handled'})
s=socket.socket(socket.AF_UNIX);s.connect(os.environ['SHEPHRD_BRIDGE_SOCKET']);f=s.makefile('r')
seq=0
def send(kind,**kw):
 global seq
 seq+=1
 v=dict(schema_version=2,token=os.environ['SHEPHRD_BRIDGE_TOKEN'],attempt_id=os.environ['SHEPHRD_BRIDGE_ATTEMPT_ID'],run_generation=int(os.environ['SHEPHRD_BRIDGE_RUN_GENERATION']),seq=seq,kind=kind,**kw)
 s.sendall((json.dumps(v)+'\n').encode())
 return json.loads(f.readline())['result']
assert send('session',session_id=args[args.index('--session-id')+1])!='fatal'
result=send('event_candidate',envelope=envelope)
assert result=='accepted_terminal',result
`

// herdrPanes plays the Herdr server for the pinned fake extension: it starts
// each committed pane's command with the pane's own Herdr markers and records
// the foreground PID the fake reports.
type herdrPanes struct {
	t          *testing.T
	operations string
}

func (h herdrPanes) lines(suffix string) []string {
	h.t.Helper()
	body, err := os.ReadFile(h.operations + suffix)
	if err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
	return strings.Fields(string(body))
}

func (h herdrPanes) started(description, pane string) (string, []string) {
	h.t.Helper()
	var command []byte
	waitFor(h.t, description, func() bool {
		var err error
		command, err = os.ReadFile(h.operations + "." + pane + ".start")
		return err == nil
	})
	body, err := os.ReadFile(h.operations + "." + pane + ".spec")
	if err != nil {
		h.t.Fatal(err)
	}
	var spec struct {
		Environment []string `json:"environment"`
	}
	if err := json.Unmarshal(body, &spec); err != nil {
		h.t.Fatal(err)
	}
	return string(command), spec.Environment
}

func (h herdrPanes) run(pane string, command string, environment []string) *exec.Cmd {
	h.t.Helper()
	markers := map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": "/socket", "HERDR_WORKSPACE_ID": "w7", "HERDR_PANE_ID": pane, "HERDR_TAB_ID": strings.Replace(pane, ":p", ":t", 1)}
	shell := exec.Command("/bin/sh", "-c", command)
	shell.Env = compoundCLIEnvironment(environment, markers)
	if err := shell.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		if shell.ProcessState == nil {
			_ = shell.Process.Kill()
			_ = shell.Wait()
		}
	})
	if err := os.WriteFile(h.operations+"."+pane+".pid", []byte(strconv.Itoa(shell.Process.Pid)), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return shell
}

func TestWakeWatchBuiltCLIActivatesHerdrSubdriverTurnsFromItsPane(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 fixture runtime unavailable")
	}
	root := t.TempDir()
	binary, extension := buildWakeWatchBinaries(t, root)
	terminalExtension := filepath.Join(root, "herdr")
	build := exec.Command("go", "build", "-o", terminalExtension, "./internal/terminal/testdata/herdrextension")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build terminal fixture: %s %v", output, err)
	}
	if err := os.Chmod(terminalExtension, 0o755); err != nil {
		t.Fatal(err)
	}
	bin, repoRoot := filepath.Join(root, "bin"), filepath.Join(root, "repo")
	for _, path := range []string{bin, repoRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(herdrWatchHarness), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("herdr watch fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "README.md"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base"}} {
		git := exec.Command("git", args...)
		git.Dir = repoRoot
		if output, err := git.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, output, err)
		}
	}
	operations := filepath.Join(root, "operations")
	if err := os.WriteFile(operations+".panes", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	receiver := startWakeWatchReceiver(t, root)
	database := filepath.Join(root, "state.db")
	config := fmt.Sprintf("default_harness = \"pi\"\nworker_runtime = \"herdr\"\ndatabase_path = %q\ndata_dir = %q\nworktree_root = %q\n[memory]\nenabled = false\n[notifications]\nenabled = false\n[wake]\nclaim_ttl = \"30s\"\nclaim_ttl_min = \"30s\"\n[wake_watch]\npoll_min = \"100ms\"\npoll_max = \"200ms\"\nrenew_horizon = \"30m\"\n[wake_watch.delivery]\nextension_id = \"shephrd.delivery-webhook\"\ncommand = [%q, \"--url\", %q, \"--secret-file\", %q, \"--allow-http\"]\nsha256 = %q\n[terminal_extensions.herdr]\ncommand = [%q, %q, %q]\nsha256 = %q\n",
		database, filepath.Join(root, "data"), filepath.Join(root, "worktrees"), extension, receiver.url+"/hook", receiver.secret, fixtureDigest(t, extension), terminalExtension, operations, binary, fixtureDigest(t, terminalExtension))
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	const owner = "driver:hermes"
	ssh := compoundCLIEnvironment(os.Environ(), map[string]string{
		"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "FIXTURE_ROOT": root,
		"SHEPHRD_CONFIG": filepath.Join(root, "config.toml"), "SHEPHRD_EXECUTABLE": binary, "SHEPHRD_WORKER_RUNTIME": "",
		"SHEPHRD_DRIVER_HARNESS": "pi", "SHEPHRD_DRIVER_MODEL": "retained-model", "SHEPHRD_WORKER": "", "PI_SESSION_ID": "",
		"SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
		"SHEPHRD_COORDINATOR_ID": "", "SHEPHRD_COORDINATOR_GENERATION": "", "SHEPHRD_COORDINATOR_TOKEN": "",
		"SHEPHRD_HERDR_GENERATION": "", "SHEPHRD_HERDR_LIFECYCLE_SEQ": "",
		"HERDR_ENV": "", "HERDR_SOCKET_PATH": "", "HERDR_WORKSPACE_ID": "", "HERDR_PANE_ID": "", "HERDR_TAB_ID": "",
	})
	inPane := func(pane string) []string {
		return compoundCLIEnvironment(ssh, map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": "/socket", "HERDR_WORKSPACE_ID": "w7", "HERDR_PANE_ID": pane, "HERDR_TAB_ID": strings.Replace(pane, ":p", ":t", 1)})
	}
	run := func(args ...string) []byte {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = ssh
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s %v", args, output, err)
		}
		return output
	}
	state, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	if _, err := state.UpsertRepo(model.Repo{Name: "herdr-watch", Path: repoRoot, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	startWatcher := func(pane string) (*exec.Cmd, *lockedBuffer, chan error) {
		t.Helper()
		watch := exec.Command(binary, "wake", "watch", "--driver-id", owner, "--json-log")
		watch.Env = inPane(pane)
		log := &lockedBuffer{}
		watch.Stdout, watch.Stderr = log, log
		if err := watch.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- watch.Wait() }()
		t.Cleanup(func() {
			if watch.ProcessState == nil {
				_ = watch.Process.Kill()
				<-exited
			}
		})
		return watch, log, exited
	}
	stopWatcher := func(watch *exec.Cmd, log *lockedBuffer, exited chan error) {
		t.Helper()
		if err := watch.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-exited:
			if err != nil || !strings.Contains(log.String(), `"event":"stopped"`) {
				t.Fatalf("watcher exit: %v\n%s", err, log.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("watcher did not stop after SIGTERM")
		}
	}
	panes := herdrPanes{t: t, operations: operations}

	var request model.SubdriverRequest
	if err := json.Unmarshal(run("subdriver", "handoff", "Pick an option for the Herdr fixture", "--repo", "herdr-watch", "--driver-id", owner, "--key", "herdr-intake", "--queue", "--json"), &request); err != nil {
		t.Fatal(err)
	}
	if queued, err := state.Subdriver(request.SubdriverID); err != nil || queued.State != "idle" || queued.Endpoint != nil {
		t.Fatalf("queued intake started a session: %+v %v", queued, err)
	}
	watch, watchLog, exited := startWatcher("w7:p1")

	first, firstEnvironment := panes.started("watcher-activated sub-driver pane", "w7:p2")
	for _, entry := range firstEnvironment {
		key, _, _ := strings.Cut(entry, "=")
		if slices.Contains([]string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_WORKSPACE_ID", "HERDR_PANE_ID"}, key) {
			t.Fatalf("sub-driver pane inherited the watcher pane marker %s", entry)
		}
	}
	if !slices.Contains(firstEnvironment, "SHEPHRD_SUBDRIVER_ID="+request.SubdriverID) || !strings.HasPrefix(first, "exec ") || !strings.Contains(first, "'_run' '"+request.SubdriverID+"'") {
		t.Fatalf("sub-driver pane command %q environment %v", first, firstEnvironment)
	}
	turn := panes.run("w7:p2", first, firstEnvironment)
	child, _ := panes.started("child worker pane", "w7:p3")
	worker := exec.Command("sleep", "300")
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = worker.Process.Kill()
		_ = worker.Wait()
	})
	if err := os.WriteFile(operations+".w7:p3.pid", []byte(strconv.Itoa(worker.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := turn.Wait(); err != nil {
		t.Fatalf("first sub-driver turn: %v", err)
	}
	if !strings.HasPrefix(child, "exec ") {
		t.Fatalf("child worker command = %q", child)
	}
	settled, err := state.Subdriver(request.SubdriverID)
	if err != nil || settled.State != "idle" || settled.Runtime != "herdr" || settled.Generation != 1 || settled.Endpoint == nil || settled.Endpoint.PaneID != "w7:p2" || settled.RunnerPID != turn.Process.Pid {
		t.Fatalf("first turn = %+v %v", settled, err)
	}
	page, err := state.SubdriverPage(request.SubdriverID, 0)
	if err != nil || len(page.Workers) != 1 {
		t.Fatalf("sub-driver page = %+v %v", page, err)
	}
	childTask, err := state.Task(page.Workers[0].TaskID)
	if err != nil || childTask.DriverID != settled.DriverID() {
		t.Fatalf("child worker owner = %+v %v", childTask, err)
	}
	childAttempt, err := state.Attempt(childTask.CurrentAttemptID)
	if err != nil || childAttempt.RuntimeBackend != "herdr" || childAttempt.TerminalEndpoint == nil || childAttempt.TerminalEndpoint.PaneID != "w7:p3" || childAttempt.RunnerPID != worker.Process.Pid {
		t.Fatalf("child worker attempt = %+v %v", childAttempt, err)
	}
	if parents := panes.lines(".parents"); len(parents) < 2 || parents[0] != "w7:p1" || slices.ContainsFunc(parents[1:], func(parent string) bool { return parent != "w7:p2" }) {
		t.Fatalf("Herdr parents = %v, want the watcher pane for the sub-driver and the sub-driver pane for its worker", parents)
	}

	question := receiver.next("sub-driver question")
	if question.request.Notification.RequestID != request.ID || question.request.Notification.Kind != "subdriver-question" {
		t.Fatalf("question = %+v", question.request)
	}
	reply := append([]string(nil), question.request.Commands.Reply[1:]...)
	for index, arg := range reply {
		reply[index] = strings.NewReplacer("<text>", "option B", "<key>", "reply-option-b").Replace(arg)
	}
	run(reply...)
	run(question.request.Commands.Ack[1:]...)

	second, secondEnvironment := panes.started("subsequent sub-driver turn", "w7:p4")
	if closed := panes.lines(".closed"); !slices.Equal(closed, []string{"w7:p2"}) {
		t.Fatalf("closed panes before the subsequent turn = %v", closed)
	}
	readyPath, finishPath := filepath.Join(root, "ready"), filepath.Join(root, "finish-turn")
	turn = panes.run("w7:p4", second, secondEnvironment)
	waitFor(t, "subsequent turn live", func() bool { _, err := os.Stat(readyPath); return err == nil })
	live, err := state.Subdriver(request.SubdriverID)
	if err != nil || live.State != "running" || live.Generation != 2 || live.Endpoint == nil || live.Endpoint.PaneID != "w7:p4" {
		t.Fatalf("live turn = %+v %v", live, err)
	}

	stopWatcher(watch, watchLog, exited)
	watch, watchLog, exited = startWatcher("w7:p9")
	waitFor(t, "restarted watcher", func() bool { return strings.Contains(watchLog.String(), `"event":"started"`) })
	before, _ := os.ReadFile(operations)
	receiver.none("delivery while the turn is live", 2*time.Second)
	if held, err := state.Subdriver(request.SubdriverID); err != nil || held.State != "running" || held.Generation != live.Generation || held.RunnerPID != live.RunnerPID || !reflect.DeepEqual(held.Endpoint, live.Endpoint) {
		t.Fatalf("restarted watcher changed the live turn: %+v %v", held, err)
	}
	if after, _ := os.ReadFile(operations); string(after) != string(before) || len(panes.lines(".panes")) != 3 || len(panes.lines(".closed")) != 1 {
		t.Fatalf("restarted watcher touched the live endpoint: %q -> %q, panes %v, closed %v", before, after, panes.lines(".panes"), panes.lines(".closed"))
	}

	if err := os.WriteFile(finishPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := turn.Wait(); err != nil {
		t.Fatalf("subsequent sub-driver turn: %v", err)
	}
	result := receiver.next("sub-driver result after restart")
	if result.request.Notification.Kind != "subdriver-result" || result.request.Notification.Payload != "Finished with option B" || result.request.Driver.Generation == question.request.Driver.Generation {
		t.Fatalf("result = %+v", result.request)
	}
	run(result.request.Commands.Ack[1:]...)
	waitFor(t, "completed endpoint closed", func() bool { return slices.Equal(panes.lines(".closed"), []string{"w7:p2", "w7:p4"}) })
	waitFor(t, "acknowledgement observed", func() bool { return strings.Contains(watchLog.String(), `"event":"acknowledged"`) })
	stopWatcher(watch, watchLog, exited)

	final, err := state.Subdriver(request.SubdriverID)
	if err != nil || final.State != "idle" || final.Failure != "" || final.Generation != 2 || process.Alive(final.RunnerPID) || process.Alive(final.HarnessPID) {
		t.Fatalf("sub-driver after restart = %+v %v", final, err)
	}
	if created := panes.lines(".panes"); !slices.Equal(created, []string{"w7:p2", "w7:p3", "w7:p4"}) || len(panes.lines(".closed")) != 2 {
		t.Fatalf("panes created %v closed %v", created, panes.lines(".closed"))
	}
	var turns []map[string]string
	body, err := os.ReadFile(filepath.Join(root, "turns"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var entry map[string]string
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		turns = append(turns, entry)
	}
	if len(turns) != 2 || turns[0]["pane"] != "w7:p2" || turns[1]["pane"] != "w7:p4" {
		t.Fatalf("sub-driver turns ran with panes %+v", turns)
	}
	notifications, err := state.Notifications("")
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notifications {
		if notice.State != model.NotificationAcknowledged {
			t.Fatalf("unsettled notification %+v", notice)
		}
	}
}
