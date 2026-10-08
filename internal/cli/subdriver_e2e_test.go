package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
	"shephrd/internal/process"
	"shephrd/internal/store"
)

func TestSubdriverCLIEndToEnd(t *testing.T) {
	subdriverCLIEndToEnd(t, false)
}

func TestPiWatcherOutstandingClaimActivationE2E(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node fixture runtime unavailable")
	}
	subdriverCLIEndToEnd(t, true)
}

func subdriverCLIEndToEnd(t *testing.T, watcher bool) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 fixture runtime unavailable")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "shephrd")
	repo := filepath.Join(root, "repo")
	bin := filepath.Join(root, "bin")
	for _, path := range []string{repo, bin} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", output, err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "--allow-empty", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	cfg := filepath.Join(root, "config.toml")
	db := filepath.Join(root, "state.db")
	data := filepath.Join(root, "data")
	body := fmt.Sprintf("default_harness = \"pi\"\nworker_runtime = \"headless\"\ndatabase_path = %q\ndata_dir = %q\nworktree_root = %q\n[memory]\nenabled = false\n", db, data, filepath.Join(root, "worktrees"))
	if watcher {
		body += "[wake]\nclaim_ttl = \"30s\"\nclaim_ttl_min = \"30s\"\n[pi_watcher]\nenabled = true\npoll_min = \"1s\"\npoll_max = \"1s\"\n"
	}
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/usr/bin/env python3
import os,sys,json,subprocess,re,time
exe=os.environ['SHEPHRD_EXECUTABLE']
root=os.environ['FIXTURE_ROOT']
def cli(*args,ok=True):
 p=subprocess.run([exe,*args,'--json'],capture_output=True,text=True)
 if not ok:
  assert p.returncode != 0,(args,p.stdout,p.stderr)
  return
 assert p.returncode == 0,(args,p.stdout,p.stderr)
 return json.loads(p.stdout)
def emit(text):
 print(json.dumps({'type':'message_end','message':{'role':'assistant','content':[{'type':'text','text':text}],'stopReason':'stop'}}),flush=True)
def event(v):return '<shephrd-event>'+json.dumps(v)+'</shephrd-event>'
def finish(artifact=None):
 cp={'schema_version':1,'summary':'Operational pointers only','completed':[],'next_steps':[],'decisions':[],'changed_paths':[],'checks':[],'blockers':[]}
 e={'type':'done','payload':'Fixture turn complete'}
 if artifact:e['artifact']=artifact
 emit(event({'type':'checkpoint','payload':'checkpoint','checkpoint':cp})+'\n'+event(e))
 print(json.dumps({'type':'agent_end'}),flush=True)
prompt=sys.argv[-1]
if os.environ.get('SHEPHRD_SUBDRIVER_ID'):
 assert os.environ['SHEPHRD_SUBDRIVER_ID']==os.environ['SHEPHRD_COORDINATOR_ID']
 assert os.environ['SHEPHRD_SUBDRIVER_GENERATION']==os.environ['SHEPHRD_COORDINATOR_GENERATION']
 assert os.environ['SHEPHRD_SUBDRIVER_TOKEN']==os.environ['SHEPHRD_COORDINATOR_TOKEN']
 assert '--session' not in sys.argv
 assert len(prompt.encode()) <= 49152
 assert 'Automatic project memory is disabled' in prompt or 'memory' in prompt
 with open(root+'/sessions.jsonl','a') as f:f.write(json.dumps({'args':sys.argv[1:-1],'prompt':prompt})+'\n')
 cid=os.environ['SHEPHRD_SUBDRIVER_ID']
 page=cli('subdriver','inspect',cid)
 cli('subdriver','handoff','recursive','--general-context','bad','--key','recursive',ok=False)
 cli('repo','add',root,ok=False)
 cli('wake','drain',ok=False)
 for e in page['events']:
  r=cli('subdriver','request',e['request_id'])
  with open(root+'/intake.jsonl','a') as f:f.write(json.dumps(r)+'\n')
  text=r['original']
  if e['kind']=='reply':
   assert e['reply_to']>0
   cli('subdriver','return',r['id'],'User reply: '+e['payload'],'--key','answer','--kind','result')
  elif text.startswith('worker') or text.startswith('crash'):
   args=('subdriver','dispatch',r['id'],'Write a bounded report','--key','step-one','--feature',r['id'],'--deliverable','report')
   a=cli(*args);b=cli(*args);assert a['id']==b['id']
   if text.startswith('crash') and not os.path.exists(root+'/crashed'):
    cli('subdriver','handled',str(e['id']))
    open(root+'/crashed','w').close();sys.exit(7)
   if text.startswith('worker') and a['status']=='queued':cli('worker','spawn',a['id'])
   if text.startswith('crash'):cli('subdriver','return',r['id'],'Recovered queued worker without duplicate dispatch','--key','recovered','--kind','result')
  elif text.startswith('question'):
   q=cli('subdriver','return',r['id'],'Which format? Recommend plain text.','--key','format','--kind','question')
   q2=cli('subdriver','return',r['id'],'Which format? Recommend plain text.','--key','format','--kind','question');assert q['id']==q2['id']
  else:
   if page['subdriver'].get('repo_id','')=='':assert page['subdriver']['context']=='Explicit research context'
   cli('subdriver','return',r['id'],'Concise result','--key','complete','--kind','result')
  if os.environ.get('SHEPHRD_TEST_WAKE_ROOT'):
   cli('subdriver','return',r['id'],'Intake complete; child still running','--key','closed','--kind','result')
  cli('subdriver','handled',str(e['id']))
 for line in prompt.splitlines():
  if not line.startswith('{"notification_id"'):continue
  n=json.loads(line)
  task=cli('task','inspect',n['task_id'])
  link=next(w for w in page['workers'] if w['task_id']==n['task_id'])
  cli('subdriver','return',link['request_id'],'Worker result: '+n['payload'],'--key','worker:'+n['notification_id'],'--kind','result')
  cli('wake','ack','--claim-token',n['claim']['claim_token'])
 finish()
else:
 open(root+'/worker.pid','w').write(str(os.getpid()))
 assert os.environ.get('SHEPHRD_WORKER')=='1'
 assert not any(v for k,v in os.environ.items() if k.startswith(('SHEPHRD_SUBDRIVER_','SHEPHRD_COORDINATOR_')))
 cli('subdriver','handoff','recursive worker','--general-context','bad','--key','worker',ok=False)
 end=time.time()+15
 while not os.path.exists(root+'/release-worker') and time.time()<end:time.sleep(.02)
 assert os.path.exists(root+'/release-worker')
 task=re.search(r'# Task (task_[a-z0-9]+):',prompt).group(1)
 path=re.search(r'Write the investigation report to (.+?) and use artifact',prompt).group(1)
 os.makedirs(os.path.dirname(path),exist_ok=True)
 open(path,'w').write('Deterministic report.\n')
 finish('report:'+path)
`
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	environment := compoundCLIEnvironment(os.Environ(), map[string]string{"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "SHEPHRD_CONFIG": cfg, "SHEPHRD_EXECUTABLE": binary, "SHEPHRD_WORKER_RUNTIME": "headless", "SHEPHRD_WORKER": "", "SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "", "SHEPHRD_DRIVER_HARNESS": "pi", "SHEPHRD_DRIVER_MODEL": "fixture-configured-model", "PI_SESSION_ID": "", "FIXTURE_ROOT": root})
	mainDriver := "driver:main"
	if watcher {
		mainDriver = "driver:pi:session-owner"
		environment = compoundCLIEnvironment(environment, map[string]string{"SHEPHRD_TEST_WAKE_ROOT": root})
	}
	call := func(ok bool, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, append(args, "--json")...)
		cmd.Env = environment
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		if ok && err != nil {
			t.Fatalf("%v: %v %s %s", args, err, out.String(), stderr.String())
		}
		if !ok && err == nil {
			t.Fatalf("unexpected success %v: %s", args, out.String())
		}
		return out.Bytes()
	}
	call(true, "repo", "add", repo, "--name", "fixture")
	decodeRequest := func(body []byte) model.SubdriverRequest {
		t.Helper()
		var r model.SubdriverRequest
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	original := "worker original\nPreserve whitespace, decisions and no merge authority.\n"
	requestFile := filepath.Join(root, "request.txt")
	if err := os.WriteFile(requestFile, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	first := decodeRequest(call(true, "subdriver", "handoff", "--repo", "fixture", "--driver-id", mainDriver, "--key", "goal-one", "--request-file", requestFile, "--context", "Prior context verbatim", "--queue"))
	repeat := decodeRequest(call(true, "subdriver", "handoff", "--repo", "fixture", "--driver-id", mainDriver, "--key", "goal-one", "--request-file", requestFile, "--context", "Prior context verbatim", "--queue"))
	if first.ID != repeat.ID || first.Original != original {
		t.Fatalf("request fidelity: %+v %+v", first, repeat)
	}
	secondOriginal := "question goal"
	if watcher {
		secondOriginal = "earlier main result"
	}
	second := decodeRequest(call(true, "subdriver", "handoff", secondOriginal, "--repo", "fixture", "--driver-id", mainDriver, "--key", "goal-two", "--queue"))
	if first.SubdriverID != second.SubdriverID {
		t.Fatal("two owners for one repository")
	}
	call(false, "subdriver", "handoff", "changed original", "--repo", "fixture", "--driver-id", mainDriver, "--key", "goal-one", "--queue")
	call(true, "subdriver", "resume", first.SubdriverID, "--foreground")
	state, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	t.Cleanup(func() {
		if body, err := os.ReadFile(filepath.Join(root, "worker.pid")); err == nil {
			pid, _ := strconv.Atoi(string(body))
			if process.Alive(pid) {
				_ = process.Stop(pid)
			}
		}
		for _, driver := range []string{mainDriver, "driver:replacement"} {
			cs, _ := state.SubdriverCandidates(driver)
			for _, c := range cs {
				if c.State == "running" {
					if process.Alive(c.HarnessPID) {
						_ = process.Stop(c.HarnessPID)
					}
					if process.Alive(c.RunnerPID) {
						_ = process.Stop(c.RunnerPID)
					}
				}
			}
		}
		attempts, _ := state.RecordedRunnerAttempts()
		for _, a := range attempts {
			if process.Alive(a.RunnerPID) {
				_ = process.Stop(a.RunnerPID)
			}
		}
	})
	page, err := state.SubdriverPage(first.SubdriverID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Subdriver.State != "idle" || len(page.Workers) != 1 {
		t.Fatalf("first session: %+v", page)
	}
	workerID := page.Workers[0].TaskID
	worker, err := state.Task(workerID)
	if err != nil {
		t.Fatal(err)
	}
	if worker.DriverID != "coordinator:"+first.SubdriverID {
		t.Fatal("wrong worker owner")
	}
	call(false, "task", "adopt", workerID, "--driver-id", worker.DriverID, "--new-driver-id", "driver:replacement")
	call(false, "worker", "retry", workerID)
	if watcher {
		command := exec.Command("node", "--test", "--test-name-pattern=^built CLI outstanding main claim activation$", "tests/pi/shephrd-wake.test.ts")
		command.Dir = "../.."
		command.Env = compoundCLIEnvironment(environment, map[string]string{"SHEPHRD_TEST_EXECUTABLE": binary, "SHEPHRD_TEST_SUBDRIVER": first.SubdriverID, "SHEPHRD_TEST_WORKER": workerID, "SHEPHRD_PI_WATCHER_ENABLED": "1"})
		output, err := command.CombinedOutput()
		t.Log(string(output))
		if err != nil {
			t.Fatal(err)
		}
		notices, err := state.Notifications("")
		if err != nil {
			t.Fatal(err)
		}
		for _, notice := range notices {
			if notice.State != model.NotificationAcknowledged || notice.DeliveryAttempts != 1 || notice.HandlingID == "" {
				t.Fatalf("notification not handled exactly once: %+v", notice)
			}
		}
		attempts, err := state.Attempts(workerID)
		if err != nil || len(attempts) != 1 {
			t.Fatalf("worker restarted: %+v %v", attempts, err)
		}
		return
	}
	var drain model.NotificationDrain
	if err = json.Unmarshal(call(true, "wake", "drain", "--driver-id", "driver:main", "--driver-generation", "main:one"), &drain); err != nil {
		t.Fatal(err)
	}
	if len(drain.Notifications) != 1 || drain.Notifications[0].RequestID != second.ID || drain.Notifications[0].Kind != "subdriver-question" {
		t.Fatalf("main notifications: %+v", drain)
	}
	question := drain.Notifications[0]
	call(true, "wake", "ack", "--claim-token", question.ClaimToken, "--driver-id", "driver:main")
	if err = os.WriteFile(filepath.Join(root, "release-worker"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitForCLITask(t, db, workerID, func(task model.Task) bool {
		return task.Status == model.TaskStatusDone && !task.ProcessAlive && task.Landed
	})
	dormant, _ := state.Subdriver(first.SubdriverID)
	if dormant.State != "idle" || dormant.Generation != 1 {
		t.Fatalf("dormant sub-driver changed: %+v", dormant)
	}
	call(true, "subdriver", "reply", second.ID, "Plain text please", "--reply-to", fmt.Sprint(question.SubdriverEventID), "--driver-id", "driver:main", "--key", "reply-one")
	call(true, "subdriver", "reply", second.ID, "Plain text please", "--reply-to", fmt.Sprint(question.SubdriverEventID), "--driver-id", "driver:main", "--key", "reply-one")
	call(false, "subdriver", "reply", first.ID, "Wrong request", "--reply-to", fmt.Sprint(question.SubdriverEventID), "--driver-id", "driver:main", "--key", "wrong")
	call(true, "wake", "drain", "--driver-id", "driver:main", "--driver-generation", "main:pump")
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := state.Subdriver(first.SubdriverID)
		if err != nil {
			t.Fatal(err)
		}
		if c.Generation == 2 && c.State == "idle" {
			break
		}
		if c.State == "held" || time.Now().After(deadline) {
			t.Fatalf("watcher pump failed: %+v", c)
		}
		time.Sleep(10 * time.Millisecond)
	}
	page, err = state.SubdriverPage(first.SubdriverID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Subdriver.Generation != 2 || page.Subdriver.State != "idle" || len(page.Workers) != 1 {
		t.Fatalf("resumed: %+v", page)
	}
	attempts, _ := state.Attempts(workerID)
	if len(attempts) != 1 {
		t.Fatalf("worker restarted: %+v", attempts)
	}
	call(true, "subdriver", "adopt-request", first.ID, "--from-driver", "driver:main", "--driver-id", "driver:replacement")
	call(true, "subdriver", "adopt-request", second.ID, "--from-driver", "driver:main", "--driver-id", "driver:replacement")
	if err = json.Unmarshal(call(true, "wake", "drain", "--driver-id", "driver:replacement", "--driver-generation", "main:replacement"), &drain); err != nil {
		t.Fatal(err)
	}
	if len(drain.Notifications) != 2 {
		t.Fatalf("correlated returns lost: %+v", drain)
	}
	for _, n := range drain.Notifications {
		if n.Kind != "subdriver-result" || n.RequestID == "" {
			t.Fatalf("grandchild wake leaked: %+v", n)
		}
		call(true, "wake", "ack", "--claim-token", n.ClaimToken, "--driver-id", "driver:replacement")
	}
	call(true, "subdriver", "resume", first.SubdriverID, "--foreground")
	call(true, "subdriver", "resume", first.SubdriverID, "--foreground", "--generation", "2", "--model", "unused-no-pending-work")
	idle, _ := state.Subdriver(first.SubdriverID)
	if idle.Generation != 2 || idle.Model != "fixture-configured-model" {
		t.Fatal("empty queue made a model call or changed selection")
	}
	worker, _ = state.Task(workerID)
	if worker.DriverID != "coordinator:"+first.SubdriverID {
		t.Fatal("main restart stole worker")
	}
	replay := decodeRequest(call(true, "subdriver", "handoff", "--repo", "fixture", "--driver-id", "driver:main", "--key", "goal-one", "--request-file", requestFile, "--context", "Prior context verbatim", "--queue"))
	if replay.ID != first.ID || replay.DriverID != "driver:replacement" {
		t.Fatal("adoption broke replay identity")
	}
	general := decodeRequest(call(true, "subdriver", "handoff", "research this", "--general-context", "Explicit research context", "--driver-id", "driver:replacement", "--key", "research", "--lead-request", first.ID, "--queue"))
	call(true, "subdriver", "resume", general.SubdriverID, "--foreground")
	gp, _ := state.SubdriverPage(general.SubdriverID, 0)
	if gp.Subdriver.RepoID != "" || len(gp.Workers) != 0 || gp.Requests[0].LeadRequestID != first.ID {
		t.Fatalf("general scope: %+v", gp)
	}
	crash := decodeRequest(call(true, "subdriver", "handoff", "crash once", "--repo", "fixture", "--driver-id", "driver:main", "--key", "crash", "--queue"))
	call(false, "subdriver", "resume", crash.SubdriverID, "--foreground")
	held, _ := state.Subdriver(crash.SubdriverID)
	if held.State != "held" {
		t.Fatalf("failure not held: %+v", held)
	}
	call(false, "subdriver", "resume", crash.SubdriverID, "--foreground")
	call(false, "subdriver", "recover", crash.SubdriverID, "--generation", "1")
	call(true, "subdriver", "recover", crash.SubdriverID, "--generation", fmt.Sprint(held.Generation))
	call(true, "subdriver", "resume", crash.SubdriverID, "--foreground")
	cp, _ := state.SubdriverPage(crash.SubdriverID, 0)
	if len(cp.Workers) != 2 || cp.Subdriver.State != "idle" {
		t.Fatalf("crash recovery duplicated workers: %+v", cp)
	}
	sessions, err := os.ReadFile(filepath.Join(root, "sessions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(sessions), []byte("\n"))
	if len(lines) != 5 {
		t.Fatalf("session count = %d", len(lines))
	}
	seen := map[string]bool{}
	for _, line := range lines {
		var session struct {
			Args   []string `json:"args"`
			Prompt string   `json:"prompt"`
		}
		if err := json.Unmarshal(line, &session); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(session.Args, " "), "--model fixture-configured-model") {
			t.Fatal("configured model lost")
		}
		for i, arg := range session.Args {
			if arg == "--session-id" {
				id := session.Args[i+1]
				if seen[id] {
					t.Fatal("session reused transcript")
				}
				seen[id] = true
			}
		}
	}
	intake, err := os.ReadFile(filepath.Join(root, "intake.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(intake, []byte("Plain text please")) && !bytes.Contains(intake, []byte("Prior context verbatim")) {
		t.Fatalf("intake lost: %s", intake)
	}
}

func TestSubdriverCLIRejectsMissingGeneralContext(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "shephrd")
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", output, err)
	}
	command := exec.Command(binary, "subdriver", "handoff", "question", "--key", "general", "--driver-id", "driver:test", "--queue")
	command.Env = compoundCLIEnvironment(os.Environ(), map[string]string{
		"SHEPHRD_CONFIG": filepath.Join(root, "config.toml"), "SHEPHRD_STATE_DIR": filepath.Join(root, "state"), "SHEPHRD_DATA_DIR": filepath.Join(root, "data"),
		"SHEPHRD_WORKER": "", "SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
	})
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "general-context") {
		t.Fatalf("%s %v", output, err)
	}
}
