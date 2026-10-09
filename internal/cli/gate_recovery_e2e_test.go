package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"shephrd/internal/control"
	"shephrd/internal/model"
	"shephrd/internal/process"
	"shephrd/internal/store"
)

const gateRecoveryHarness = `#!/usr/bin/env python3
import os,sys,json,subprocess,signal
exe=os.environ['SHEPHRD_EXECUTABLE']
root=os.environ['FIXTURE_ROOT']
args=sys.argv[1:]
with open(root+'/models','a') as f:f.write((args[args.index('--model')+1] if '--model' in args else 'native-default')+'\n')
def cli(*args):
 p=subprocess.run([exe,*args,'--json'],capture_output=True,text=True)
 assert p.returncode==0,(args,p.stdout,p.stderr)
 return json.loads(p.stdout)
def event(v):return '<shephrd-event>'+json.dumps(v)+'</shephrd-event>'
cid=os.environ['SHEPHRD_SUBDRIVER_ID']
for e in cli('subdriver','inspect',cid)['events']:
 if e['kind']=='recovery':
  cli('subdriver','handled',str(e['id']))
  continue
 text=e['payload'] if e['kind']=='reply' else cli('subdriver','request',e['request_id'])['original']
 if text.startswith('crash') and not os.path.exists(root+'/crashed'):
  open(root+'/crashed','w').close()
  os.kill(os.getppid(),signal.SIGKILL)
  sys.exit(9)
 if text.startswith('finish'):
  cli('subdriver','return',e['request_id'],'Finished '+text,'--key','result-'+str(e['id']),'--kind','result')
 else:
  cli('subdriver','return',e['request_id'],'Next? '+text,'--key','question-'+str(e['id']),'--kind','question')
 cli('subdriver','handled',str(e['id']))
cp={'schema_version':1,'summary':'Fixture turn','completed':[],'next_steps':[],'decisions':[],'changed_paths':[],'checks':[],'blockers':[]}
text=event({'type':'checkpoint','payload':'checkpoint','checkpoint':cp})+'\n'+event({'type':'done','payload':'Fixture turn complete'})
print(json.dumps({'type':'message_end','message':{'role':'assistant','content':[{'type':'text','text':text}],'stopReason':'stop'}}),flush=True)
print(json.dumps({'type':'agent_end'}),flush=True)
`

func TestGateBuiltCLIRecoversOwnedHeldSubdriverForWatcherActivation(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 fixture runtime unavailable")
	}
	root := t.TempDir()
	binary, extension := buildWakeWatchBinaries(t, root)
	bin, repo := filepath.Join(root, "bin"), filepath.Join(root, "repo")
	for _, path := range []string{bin, repo} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(gateRecoveryHarness), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "--allow-empty", "-m", "initial"}} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", output, err)
		}
	}
	receiver := startWakeWatchReceiver(t, root)
	database := filepath.Join(root, "state.db")
	config := fmt.Sprintf("default_harness = \"pi\"\nworker_runtime = \"headless\"\ndatabase_path = %q\ndata_dir = %q\nworktree_root = %q\n[memory]\nenabled = false\n[wake]\nclaim_ttl = \"30s\"\nclaim_ttl_min = \"30s\"\n[wake_watch]\npoll_min = \"100ms\"\npoll_max = \"200ms\"\nrenew_horizon = \"30m\"\n[wake_watch.delivery]\nextension_id = \"shephrd.delivery-webhook\"\ncommand = [%q, \"--url\", %q, \"--secret-file\", %q, \"--allow-http\"]\nsha256 = %q\n",
		database, filepath.Join(root, "data"), filepath.Join(root, "worktrees"), extension, receiver.url+"/hook", receiver.secret, fixtureDigest(t, extension))
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := compoundCLIEnvironment(os.Environ(), map[string]string{
		"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "FIXTURE_ROOT": root,
		"SHEPHRD_CONFIG": filepath.Join(root, "config.toml"), "SHEPHRD_EXECUTABLE": binary, "SHEPHRD_WORKER_RUNTIME": "",
		"SHEPHRD_DRIVER_HARNESS": "pi", "SHEPHRD_DRIVER_MODEL": "fixture-initial-model", "SHEPHRD_WORKER": "", "PI_SESSION_ID": "",
		"SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
		"SHEPHRD_COORDINATOR_ID": "", "SHEPHRD_COORDINATOR_GENERATION": "", "SHEPHRD_COORDINATOR_TOKEN": "",
	})
	const owner = "driver:hermes"
	local := func(args ...string) []byte {
		t.Helper()
		command := exec.Command(binary, append(args, "--json")...)
		command.Env = environment
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s %v", args, output, err)
		}
		return output
	}
	gate := func(extra map[string]string, argv ...string) (string, map[string]any, int) {
		t.Helper()
		request, err := json.Marshal(argv)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(binary, "gate", "--driver-id", owner)
		command.Env = append(compoundCLIEnvironment(environment, extra), "SSH_ORIGINAL_COMMAND="+string(request))
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		status := 0
		if err := command.Run(); err != nil {
			exit, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatal(err)
			}
			status = exit.ExitCode()
		}
		var diagnostic map[string]any
		if stderr.Len() > 0 {
			if err := json.Unmarshal(stderr.Bytes(), &diagnostic); err != nil {
				t.Fatalf("%v stderr is not one JSON error: %q", argv, stderr.String())
			}
		}
		return stdout.String(), diagnostic, status
	}
	allowed := func(argv ...string) string {
		t.Helper()
		stdout, diagnostic, status := gate(nil, argv...)
		if status != 0 || diagnostic != nil {
			t.Fatalf("%v: status=%d diagnostic=%v", argv, status, diagnostic)
		}
		return stdout
	}
	refused := func(extra map[string]string, want string, argv ...string) {
		t.Helper()
		stdout, diagnostic, status := gate(extra, argv...)
		if status != 1 || stdout != "" || diagnostic == nil || !strings.Contains(fmt.Sprint(diagnostic["error_kind"], " ", diagnostic["error"]), want) {
			t.Fatalf("%v: status=%d stdout=%q diagnostic=%v, want %q", argv, status, stdout, diagnostic, want)
		}
	}
	state, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	models := func() []string {
		body, _ := os.ReadFile(filepath.Join(root, "models"))
		return strings.Fields(string(body))
	}
	ack := func(description, kind, payload string) {
		t.Helper()
		delivered := receiver.next(description)
		if delivered.request.Notification.Kind != kind || !strings.Contains(delivered.request.Notification.Payload, payload) {
			t.Fatalf("%s = %+v", description, delivered.request.Notification)
		}
		allowed(delivered.request.Commands.Ack...)
	}

	local("repo", "add", repo, "--name", "fixture")
	watch := exec.Command(binary, "wake", "watch", "--driver-id", owner, "--json-log")
	watch.Env = environment
	var watchLog lockedBuffer
	watch.Stdout, watch.Stderr = &watchLog, &watchLog
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
	var first, second model.SubdriverRequest
	if err := json.Unmarshal([]byte(allowed("subdriver", "handoff", "--repo", "fixture", "--key", "first", "finish alpha")), &first); err != nil {
		t.Fatal(err)
	}
	ack("first result", "subdriver-result", "Finished finish alpha")
	if done, err := state.SubdriverRequest(first.ID); err != nil || done.State != "done" {
		t.Fatalf("first request not completed: %+v %v", done, err)
	}
	if err := json.Unmarshal([]byte(allowed("subdriver", "handoff", "--repo", "fixture", "--key", "second", "crash beta")), &second); err != nil {
		t.Fatal(err)
	}
	ack("held blocker", "subdriver-blocker", "runner exited")
	held, err := state.Subdriver(second.SubdriverID)
	if err != nil || held.State != "held" || held.Model != "fixture-initial-model" || second.SubdriverID != first.SubdriverID {
		t.Fatalf("held owner = %+v %v", held, err)
	}
	waitFor(t, "held harness exit", func() bool { return !process.Alive(held.HarnessPID) })

	local("subdriver", "adopt-request", first.ID, "--from-driver", owner, "--driver-id", "driver:other")
	var diagnosis control.SubdriverDiagnosis
	if err := json.Unmarshal([]byte(allowed("subdriver", "diagnose", held.ID)), &diagnosis); err != nil {
		t.Fatal(err)
	}
	if diagnosis.Recoverable || len(diagnosis.RequestOwners) != 2 || !diagnosis.LaunchIdentityComplete || diagnosis.Runner.Status != "absent" || diagnosis.HarnessProcess.Status != "absent" {
		t.Fatalf("mixed-owner diagnosis = %+v", diagnosis)
	}
	refused(nil, "subdriver_owner_refused", "subdriver", "recover", held.ID, "--generation", fmt.Sprint(held.Generation), "--model", "fixture-recovered-model")
	local("subdriver", "adopt-request", first.ID, "--from-driver", "driver:other", "--driver-id", owner)
	refused(nil, "generation changed", "subdriver", "recover", held.ID, "--generation", fmt.Sprint(held.Generation-1), "--model", "fixture-recovered-model")
	refused(nil, "exact pi model identifier", "subdriver", "recover", held.ID, "--generation", fmt.Sprint(held.Generation), "--model", "two words")
	refused(nil, "wake_watch_active", "subdriver", "resume", held.ID, "--generation", fmt.Sprint(held.Generation))
	if current, err := state.Subdriver(held.ID); err != nil || current.State != "held" || current.Model != held.Model || current.Generation != held.Generation {
		t.Fatalf("refused recovery changed owner: %+v %v", current, err)
	}

	output := allowed("subdriver", "diagnose", held.ID)
	if err := json.Unmarshal([]byte(output), &diagnosis); err != nil {
		t.Fatal(err)
	}
	if !diagnosis.Recoverable || diagnosis.Endpoint != "unrecorded" || len(diagnosis.Blockers) != 0 || !diagnosis.Pending {
		t.Fatalf("owned diagnosis = %+v", diagnosis)
	}
	for _, leaked := range []string{"token", "session-", "Fixture turn"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("diagnosis leaked %q: %s", leaked, output)
		}
	}
	launches := len(models())
	var recovered model.Subdriver
	if err := json.Unmarshal([]byte(allowed("subdriver", "recover", held.ID, "--generation", fmt.Sprint(held.Generation), "--model", "fixture-recovered-model")), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Model != "fixture-recovered-model" || recovered.Harness != "pi" {
		t.Fatalf("recovery output = %+v", recovered)
	}
	ack("question after watcher activation", "subdriver-question", "Next? crash beta")
	waitFor(t, "recovered turn idle", func() bool {
		current, err := state.Subdriver(held.ID)
		return err == nil && current.State == "idle" && current.Generation == held.Generation+1
	})
	if got := models(); len(got) != launches+1 || got[len(got)-1] != "fixture-recovered-model" {
		t.Fatalf("watcher activation models = %v (before %d)\n%s", got, launches, watchLog.String())
	}

	if err := watch.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("watcher exit: %v\n%s", err, watchLog.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not stop after SIGTERM")
	}
	question := int64(0)
	if notices, err := state.Notifications(""); err == nil {
		for _, notice := range notices {
			if notice.Kind == "subdriver-question" && notice.RequestID == second.ID {
				question = notice.SubdriverEventID
			}
		}
	}
	if question == 0 {
		t.Fatal("question event not recorded")
	}
	allowed("subdriver", "reply", second.ID, "finish gamma", "--reply-to", fmt.Sprint(question), "--key", "reply-gamma")
	idle, err := state.Subdriver(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	launches = len(models())
	for _, check := range []struct {
		extra map[string]string
		argv  []string
		want  string
	}{
		{map[string]string{"SHEPHRD_WORKER_RUNTIME": "herdr"}, []string{"subdriver", "resume", idle.ID, "--generation", fmt.Sprint(idle.Generation)}, "explicitly headless"},
		{map[string]string{"SHEPHRD_WORKER_RUNTIME": "auto"}, []string{"subdriver", "resume", idle.ID, "--generation", fmt.Sprint(idle.Generation)}, "explicitly headless"},
		{nil, []string{"subdriver", "resume", idle.ID, "--generation", fmt.Sprint(idle.Generation), "--foreground"}, "never runs a foreground"},
		{nil, []string{"subdriver", "resume", idle.ID}, "requires --generation"},
		{nil, []string{"subdriver", "resume", idle.ID, "--generation", fmt.Sprint(idle.Generation - 1)}, "generation changed"},
	} {
		refused(check.extra, check.want, check.argv...)
	}
	if current, err := state.Subdriver(idle.ID); err != nil || current.Generation != idle.Generation || current.State != "idle" || len(models()) != launches {
		t.Fatalf("refused resume launched: %+v %v %v", current, err, models())
	}
	allowed("subdriver", "resume", idle.ID, "--generation", fmt.Sprint(idle.Generation))
	waitFor(t, "manual headless turn completes", func() bool {
		current, err := state.Subdriver(idle.ID)
		done, requestErr := state.SubdriverRequest(second.ID)
		return err == nil && requestErr == nil && current.State == "idle" && current.Generation == idle.Generation+1 && done.State == "done"
	})
	if got := models(); len(got) != launches+1 || got[len(got)-1] != "fixture-recovered-model" {
		t.Fatalf("manual resume models = %v", got)
	}
	audit, err := os.ReadFile(filepath.Join(root, "data", "gate", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(audit)), "\n") {
		var record gateAuditRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil || record.DriverID != owner || !slices.Contains(gateCommands, record.Command) {
			t.Fatalf("audit line %q %v", line, err)
		}
	}
	if strings.Contains(string(audit), "fixture-recovered-model") || strings.Contains(string(audit), "finish gamma") {
		t.Fatalf("audit recorded arguments: %s", audit)
	}
}

func TestGateBuiltCLIRecoveryRefusesLiveUncertainAndUnassertedLaunches(t *testing.T) {
	cli := newGateCLI(t)
	const owner = "driver:hermes"
	gate := func(argv ...string) (string, map[string]string, int) {
		t.Helper()
		body, err := json.Marshal(argv)
		if err != nil {
			t.Fatal(err)
		}
		return cli.run(t, owner, string(body), "")
	}
	state, err := store.Open(cli.database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
	held := func(key, runtime string, runner, harness int, endpoint *model.TerminalEndpoint) model.Subdriver {
		t.Helper()
		request, err := state.HandoffSubdriver("", "Recovery fixture", owner, key, "Original "+key, "", "")
		if err != nil {
			t.Fatal(err)
		}
		f, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "retained", runtime)
		if err != nil {
			t.Fatal(err)
		}
		if endpoint != nil {
			if err := state.SetSubdriverEndpoint(f, *endpoint); err != nil {
				t.Fatal(err)
			}
		}
		if runner != 0 {
			if err := state.StartSubdriver(f, runner, "session"); err != nil {
				t.Fatal(err)
			}
			if err := state.SubdriverHarnessPID(f, harness); err != nil {
				t.Fatal(err)
			}
		}
		if err := state.FinishSubdriver(f, "", "fixture failure"); err != nil {
			t.Fatal(err)
		}
		c, err := state.Subdriver(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	diagnose := func(c model.Subdriver) control.SubdriverDiagnosis {
		t.Helper()
		stdout, diagnostic, status := gate("subdriver", "diagnose", c.ID)
		var d control.SubdriverDiagnosis
		if status != 0 || diagnostic != nil || json.Unmarshal([]byte(stdout), &d) != nil {
			t.Fatalf("diagnose %s: %d %v %s", c.ID, status, diagnostic, stdout)
		}
		return d
	}
	recover := func(c model.Subdriver, want string, extra ...string) {
		t.Helper()
		stdout, diagnostic, status := gate(append([]string{"subdriver", "recover", c.ID, "--generation", fmt.Sprint(c.Generation)}, extra...)...)
		if want == "" {
			if status != 0 || diagnostic != nil {
				t.Fatalf("recover %s: %d %v", c.ID, status, diagnostic)
			}
			return
		}
		if status != 1 || stdout != "" || !strings.Contains(diagnostic["error"], want) {
			t.Fatalf("recover %s: %d %q %v, want %q", c.ID, status, stdout, diagnostic, want)
		}
		if current, err := state.Subdriver(c.ID); err != nil || current.State != "held" || current.Model != "retained" {
			t.Fatalf("refused recovery changed owner: %+v %v", current, err)
		}
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := dead.ProcessState.Pid()

	running := held("live", "headless", live.Process.Pid, deadPID, nil)
	if d := diagnose(running); d.Recoverable || d.Runner.Status != "live_or_uncertain" || d.HarnessProcess.Status != "absent" {
		t.Fatalf("live diagnosis %+v", d)
	}
	recover(running, "still live", "--launch-absent", "Operator cannot override a live recorded runner")

	herdr := &model.TerminalEndpoint{Backend: "herdr", SocketPath: "/nonexistent", WorkspaceID: "workspace", TabID: "tab", PaneID: "pane"}
	uncertain := held("endpoint", "herdr", deadPID, deadPID, herdr)
	if d := diagnose(uncertain); d.Recoverable || d.Endpoint != "uncertain" || d.EndpointBackend != "herdr" {
		t.Fatalf("endpoint diagnosis %+v", d)
	}
	recover(uncertain, "terminal absence is not proven", "--launch-absent", "Operator cannot override an uncertain endpoint")

	incomplete := held("incomplete", "headless", 0, 0, nil)
	if d := diagnose(incomplete); d.Recoverable || d.LaunchIdentityComplete || d.Runner.Status != "unrecorded" {
		t.Fatalf("incomplete diagnosis %+v", d)
	}
	recover(incomplete, "identity is incomplete")
	recover(incomplete, "identity is incomplete", "--launch-absent", "   ")
	recover(incomplete, "", "--launch-absent", "launcher.log has no runner start and no process carries the exact source")
	page, err := state.SubdriverPage(incomplete.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if last := page.Events[len(page.Events)-1]; last.Kind != "recovery" || !strings.Contains(last.Payload, "Operator asserted absent unrecorded launch effects: launcher.log") || !strings.Contains(last.Payload, owner) {
		t.Fatalf("assertion event %+v", last)
	}

	retained := held("herdr-retained", "headless", deadPID, deadPID, nil)
	recover(retained, "")
	f, err := state.ReserveSubdriver(retained.ID, retained.Generation, "pi", "retained", "herdr")
	if err != nil {
		t.Fatal(err)
	}
	if err = state.FinishSubdriver(f, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, diagnostic, status := gate("subdriver", "resume", retained.ID, "--generation", fmt.Sprint(f.Generation)); status != 1 || !strings.Contains(diagnostic["error"], `retained "herdr"`) {
		t.Fatalf("Herdr owner resumed from SSH: %d %v", status, diagnostic)
	}
	if current, err := state.Subdriver(retained.ID); err != nil || current.State != "idle" || current.Generation != f.Generation || current.RunnerPID != 0 {
		t.Fatalf("Herdr refusal changed owner: %+v %v", current, err)
	}
}
