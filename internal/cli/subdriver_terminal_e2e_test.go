package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/process"
	"shephrd/internal/store"
)

func TestSubdriverTerminalCLIEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 fixture runtime unavailable")
	}
	node, nodeErr := exec.LookPath("node")
	root := t.TempDir()
	transportFixture, err := filepath.Abs("../pibridge/testdata/transport-harness.mjs")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "shephrd")
	build := func(output, pkg string) {
		t.Helper()
		cmd := exec.Command("go", "build", "-o", output, pkg)
		cmd.Dir = "../.."
		if body, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %s %v", body, err)
		}
		if err := os.Chmod(output, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	build(binary, "./cmd/shephrd")
	for _, runtime := range []string{"headless", "herdr", "cmux"} {
		extension := filepath.Join(root, runtime)
		digest := ""
		if runtime != "headless" {
			build(extension, "./internal/terminal/testdata/"+runtime+"extension")
			body, err := os.ReadFile(extension)
			if err != nil {
				t.Fatal(err)
			}
			digest = fmt.Sprintf("%x", sha256.Sum256(body))
		}
		for _, harness := range []string{"pi", "claude-code"} {
			if runtime == "headless" && harness != "pi" {
				continue
			}
			modes := []string{"valid"}
			if runtime == "herdr" {
				modes = append(modes, "artifact", "missing-checkpoint", "oversized", "unknown-field", "stale")
			}
			if harness == "pi" && runtime != "cmux" {
				modes = append(modes, "repair-decisions", "repair-checks", "repair-exhausted", "repair-duplicate", "repair-artifact", "repair-prose", "repair-session", "repair-checkpoint-only", "transport-empty", "transport-partial", "transport-complete", "transport-exhausted", "transport-cancel", "transport-abort", "transport-auth", "transport-schema", "transport-normal", "repair-transport", "repair-transport-exhausted", "repair-transport-invalid")
			}
			for _, mode := range modes {
				t.Run(runtime+"/"+harness+"/"+mode, func(t *testing.T) {
					if strings.Contains(mode, "transport-") && nodeErr != nil {
						t.Skip("node fixture runtime unavailable for the Pi bridge transport fixture")
					}
					dir := t.TempDir()
					cfg := filepath.Join(dir, "config.toml")
					db := filepath.Join(dir, "state.db")
					data := filepath.Join(dir, "data")
					operations := filepath.Join(dir, "operations")
					text := fmt.Sprintf("database_path = %q\ndata_dir = %q\n[memory]\nenabled = false\n", db, data)
					if runtime != "headless" {
						text += fmt.Sprintf("[terminal_extensions.%s]\ncommand = [%q, %q, %q]\nsha256 = %q\n", runtime, extension, operations, binary, digest)
					}
					if err := os.WriteFile(cfg, []byte(text), 0600); err != nil {
						t.Fatal(err)
					}
					name := harness
					if name == "claude-code" {
						name = "claude"
					}
					if err := os.WriteFile(filepath.Join(dir, name), []byte(subdriverTerminalHarness), 0700); err != nil {
						t.Fatal(err)
					}
					env := compoundCLIEnvironment(os.Environ(), map[string]string{
						"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "SHEPHRD_CONFIG": cfg,
						"SHEPHRD_EXECUTABLE": binary, "SHEPHRD_WORKER": "", "SHEPHRD_SUBDRIVER_ID": "",
						"SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
						"SHEPHRD_HERDR_LIFECYCLE_SEQ": "", "FIXTURE_MODE": mode, "FIXTURE_DIR": dir,
						"FIXTURE_TRANSPORT": transportFixture, "FIXTURE_NODE": node,
					})
					state, err := store.Open(db)
					if err != nil {
						t.Fatal(err)
					}
					defer state.Close()
					repo, err := state.UpsertRepo(model.Repo{Name: "fixture", Path: dir, DefaultBranch: "main"})
					if err != nil {
						t.Fatal(err)
					}
					request, err := state.HandoffSubdriver(repo.ID, "", "driver:fixture", "initial", "Original intake", "Context", "")
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						c, _ := state.Subdriver(request.SubdriverID)
						if process.Alive(c.HarnessPID) {
							_ = process.Stop(c.HarnessPID)
						}
					}()
					endpoint := model.TerminalEndpoint{Backend: runtime, SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}
					if runtime == "cmux" {
						endpoint = model.TerminalEndpoint{Backend: runtime, SocketPath: "/socket", WindowID: "11111111-1111-4111-8111-111111111111", WorkspaceID: "22222222-2222-4222-8222-222222222222", PaneID: "33333333-3333-4333-8333-333333333333", SurfaceID: "44444444-4444-4444-8444-444444444444"}
					}
					turns := 1
					if mode == "valid" {
						turns = 2
					}
					previousSession := ""
					for generation := 1; generation <= turns; generation++ {
						if generation > 1 {
							if _, err := state.HandoffSubdriver(repo.ID, "", "driver:fixture", "next", "Next intake", "", ""); err != nil {
								t.Fatal(err)
							}
						}
						fence, err := state.ReserveSubdriver(request.SubdriverID, generation-1, harness, "retained-model", runtime)
						if err != nil {
							t.Fatal(err)
						}
						if runtime != "headless" {
							if err := state.SetSubdriverEndpoint(fence, endpoint); err != nil {
								t.Fatal(err)
							}
						}
						command := "subdriver"
						turnEnv := env
						if generation == 2 {
							command = "coordinator"
							turnEnv = compoundCLIEnvironment(env, map[string]string{
								"SHEPHRD_COORDINATOR_ID": fence.ID, "SHEPHRD_COORDINATOR_GENERATION": fmt.Sprint(fence.Generation), "SHEPHRD_COORDINATOR_TOKEN": fence.Token,
							})
						}
						cmd := exec.Command(binary, command, "_run", fence.ID, "--generation", fmt.Sprint(fence.Generation), "--token", fence.Token)
						cmd.Env = turnEnv
						var stdout, stderr bytes.Buffer
						cmd.Stdout, cmd.Stderr = &stdout, &stderr
						err = cmd.Run()
						c, readErr := state.Subdriver(fence.ID)
						if readErr != nil {
							t.Fatal(readErr)
						}
						if !strings.Contains(stdout.String(), "NATIVE_FIXTURE_CONTENTS") || c.HarnessPID == 0 || process.Alive(c.HarnessPID) || process.Alive(c.RunnerPID) {
							t.Fatalf("output or process ownership: %s %s %+v", stdout.String(), stderr.String(), c)
						}
						if strings.HasPrefix(mode, "repair-") || strings.HasPrefix(mode, "transport-") {
							page, readErr := state.SubdriverPage(fence.ID, 0)
							actions, actionErr := os.ReadFile(filepath.Join(dir, "actions"))
							if readErr != nil || actionErr != nil || string(actions) != "dispatch\nreturn\nhandled\n" || len(page.Workers) != 1 {
								t.Fatalf("repeated or lost actions: %q %+v %v %v", actions, page, readErr, actionErr)
							}
							worker, workerErr := state.Task(page.Workers[0].TaskID)
							if workerErr != nil || worker.Status != model.TaskStatusQueued || worker.DriverID != c.DriverID() {
								t.Fatalf("reporting changed existing work: %+v %v", worker, workerErr)
							}
							log, _ := os.ReadFile(filepath.Join(data, fence.ID, fmt.Sprintf("session-%d.log", fence.Generation)))
							if strings.HasPrefix(mode, "repair-") && (!bytes.Contains(log, []byte("EVENT_SCHEMA_TYPE_MISMATCH")) || !bytes.Contains(log, []byte("CandidateHash"))) {
								t.Fatalf("missing repair evidence: %s", log)
							}
							if strings.HasPrefix(mode, "transport-") && bytes.Contains(log, []byte("CandidateHash")) {
								t.Fatalf("transport failure requested reporting repair: %s", log)
							}
						}
						success := mode == "valid" || mode == "repair-decisions" || mode == "repair-checks" || mode == "repair-transport" || mode == "transport-empty" || mode == "transport-partial" || mode == "transport-complete" || mode == "transport-normal"
						if !success {
							if err == nil || c.State != "held" || c.Failure == "" {
								t.Fatalf("invalid turn accepted: %v %+v", err, c)
							}
							if (strings.HasPrefix(mode, "repair-") || strings.HasPrefix(mode, "transport-")) && c.Checkpoint != "" {
								t.Fatalf("partial rejected batch persisted: %+v", c)
							}
							return
						}
						if err != nil || c.State != "idle" || c.Generation != generation || c.Model != "retained-model" || c.SessionID == "" || c.SessionID == previousSession || len(c.Checkpoint) > 4096 || strings.Contains(c.Checkpoint, "FAILED TRANSPORT") {
							t.Fatalf("turn: %v %s %+v", err, stderr.String(), c)
						}
						if strings.HasPrefix(mode, "repair-") {
							var checkpoint model.Checkpoint
							if err := json.Unmarshal([]byte(c.Checkpoint), &checkpoint); err != nil || len(checkpoint.Decisions) != 1 || len(checkpoint.Checks) != 1 {
								t.Fatalf("corrected fields were dropped: %s %v", c.Checkpoint, err)
							}
						}
						previousSession = c.SessionID
						if err := state.CheckSubdriverFence(fence); err == nil {
							t.Fatal("settled session retained authority")
						}
					}
					if runtime != "headless" {
						log, err := os.ReadFile(operations)
						if err != nil || !strings.Contains(string(log), "report_agent") || !strings.Contains(string(log), "release_agent") || strings.Contains(string(log), "focus") || strings.Contains(string(log), "close") {
							t.Fatalf("lifecycle: %s %v", log, err)
						}
					}
					notices, err := state.DrainNotifications("", "driver:fixture", "fixture", 10)
					if err != nil || len(notices.Notifications) != turns {
						t.Fatalf("returns: %+v %v", notices, err)
					}
					for _, notice := range notices.Notifications {
						if notice.SubdriverID != request.SubdriverID || notice.RequestID == "" || notice.SubdriverEventID == 0 || notice.Kind != "subdriver-result" {
							t.Fatalf("uncorrelated return: %+v", notice)
						}
					}
				})
			}
		}
	}
}

const subdriverTerminalHarness = `#!/usr/bin/env python3
import os,sys,json,socket,subprocess,time,signal
args=sys.argv[1:]
headless='--mode' in args
repair='--session' in args
assert '--resume' not in args
if not headless:assert '--print' not in args and '-p' not in args and not repair
assert args[args.index('--model')+1]=='retained-model'
assert len(args[-1].encode())<=49152
assert os.environ['SHEPHRD_PI_WATCHER_ENABLED']=='0'
assert os.environ.get('SHEPHRD_WORKER','')==''
assert os.environ['SHEPHRD_DRIVER_MODEL']=='retained-model'
session=args[args.index('--session' if repair else '--session-id')+1]
mode=os.environ['FIXTURE_MODE']
exe=os.environ['SHEPHRD_EXECUTABLE']
def cli(*args,ok=True):
 p=subprocess.run([exe,*args,'--json'],capture_output=True,text=True)
 if not ok:
  assert p.returncode!=0
  return
 assert p.returncode==0,(p.stdout,p.stderr)
 return json.loads(p.stdout)
cid=os.environ['SHEPHRD_SUBDRIVER_ID']
def action(name):
 with open(os.path.join(os.environ['FIXTURE_DIR'],'actions'),'a') as f:f.write(name+'\n')
if repair:
 assert '--no-tools' in args
 assert 'SHEPHRD_PROTOCOL_REPAIR' in args[-1] and 'correction attempt 1 of 1' in args[-1]
 assert 'Candidate SHA-256:' in args[-1] and 'Never dispatch' in args[-1]
else:
 page=cli('subdriver','inspect',cid)
 assert page['subdriver']['harness_pid']==os.getpid()
 assert page['subdriver']['runner_pid']==os.getppid()
 cli('wake','drain',ok=False)
 cli('subdriver','handoff','recursive','--general-context','bad','--key','bad',ok=False)
 for e in page['events']:
  r=cli('subdriver','request',e['request_id'])
  if mode.startswith(('repair-','transport-','pump-')):
   cli('subdriver','dispatch',r['id'],'Already dispatched work','--key','worker','--feature','fixture-worker','--deliverable','code')
   action('dispatch')
  if not mode.startswith('pump-'):
   cli('subdriver','return',r['id'],'Fixture result','--key','result','--kind','result')
   action('return')
  cli('subdriver','handled',str(e['id']))
  action('handled')
print('NATIVE_FIXTURE_CONTENTS',flush=True)
if mode.startswith('pump-'):
 root=os.environ['FIXTURE_DIR']
 open(root+'/ready','w').close()
 deadline=time.time()+15
 while not os.path.exists(root+'/finish-turn'):
  assert time.time()<deadline,'finish gate timed out'
  time.sleep(.005)
 if mode=='pump-runner-loss':
  os.kill(os.getppid(),signal.SIGKILL)
  sys.exit(0)
cp={'schema_version':1,'summary':'bounded checkpoint','completed':[],'next_steps':[],'decisions':[],'changed_paths':[],'checks':[],'blockers':[]}
if mode=='oversized':cp['summary']='x'*5000
if mode=='unknown-field':cp['surprise']=True
def event(e):return '<shephrd-event>'+json.dumps(e)+'</shephrd-event>'
checkpoint=event({'type':'checkpoint','payload':'checkpoint','checkpoint':cp})
done={'type':'done','payload':'Turn handled'}
if mode=='artifact':done['artifact']='branch:not-a-worker'
envelopes=([checkpoint] if mode!='missing-checkpoint' else [])+[event(done)]
def corrected():
 fixed=dict(cp,decisions=[{'decision':'Retain dispatched work','reason':'Already dispatched'}],checks=[{'command':'inspect','result':'running'}])
 if mode in ('repair-exhausted','repair-transport-invalid'):fixed['checks']=['still wrong']
 result=[event({'type':'checkpoint','payload':'corrected','checkpoint':fixed}),event({'type':'done','payload':'turn handled'})]
 if mode=='repair-duplicate':result.append(result[-1])
 if mode=='repair-artifact':result[-1]=event({'type':'done','payload':'wrong artifact','artifact':'branch:wrong'})
 if mode=='repair-prose':result.insert(0,'Unexpected prose')
 if mode=='repair-checkpoint-only':result=result[:1]
 return '\n'.join(result)
if mode.startswith('repair-'):
 bad=dict(cp)
 bad['checks' if mode=='repair-checks' else 'decisions']=['Already dispatched work']
 envelopes=[event({'type':'checkpoint','payload':'rejected','checkpoint':bad}),event(done)]
if mode.startswith('transport-') or mode.startswith('repair-transport'):
 bridge=args[args.index('--extension')+1] if not headless else ''
 p=subprocess.run([os.environ['FIXTURE_NODE'],os.environ['FIXTURE_TRANSPORT'],bridge],input=json.dumps(dict(session=session,mode=mode,headless=headless,envelopes='\n'.join(envelopes),corrected=corrected(),correction=repair)),text=True)
 sys.exit(p.returncode)
if headless:
 if repair and mode=='repair-session':session='wrong-session'
 print(json.dumps({'type':'session','id':session}),flush=True)
 text=corrected() if repair else '\n'.join(envelopes)
 print(json.dumps({'type':'message_end','message':{'role':'assistant','content':[{'type':'text','text':text}],'stopReason':'stop'}}),flush=True)
 print(json.dumps({'type':'agent_end'}),flush=True)
 sys.exit(0)
if '--extension' in args:
 assert os.path.isfile(args[args.index('--extension')+1])
 s=socket.socket(socket.AF_UNIX);s.connect(os.environ['SHEPHRD_BRIDGE_SOCKET']);f=s.makefile('r')
 seq=0
 response={}
 def send(kind,**kw):
  global seq,response
  seq+=1
  v=dict(schema_version=2,token=os.environ['SHEPHRD_BRIDGE_TOKEN'],attempt_id=os.environ['SHEPHRD_BRIDGE_ATTEMPT_ID'],run_generation=int(os.environ['SHEPHRD_BRIDGE_RUN_GENERATION']),seq=seq,kind=kind,**kw)
  if mode=='stale':v['run_generation']+=1
  s.sendall((json.dumps(v)+'\n').encode())
  line=f.readline()
  response=json.loads(line) if line else {'result':'fatal'}
  return response['result']
 if send('session',session_id=session)=='fatal':sys.exit(0)
 result=send('event_candidate',envelope='\n'.join(envelopes))
 if mode.startswith('repair-'):
  assert result=='repair',response
  d=response['diagnostic']
  assert d['code']=='EVENT_SCHEMA_TYPE_MISMATCH' and d['field'].startswith('checkpoint.')
  assert 'objects with string fields' in d['message'] and 'not strings' in d['message']
  if mode=='repair-session':
   send('session',session_id='wrong-session')
   sys.exit(0)
  result=send('event_candidate',envelope=corrected())
 if result=='fatal':sys.exit(0)
 assert result=='accepted_terminal',response
else:
 assert '--settings' in args
 settings=json.load(open(args[args.index('--settings')+1]))
 assert settings['hooks']
 def hook(name,**kw):
  v=dict(session_id=session,hook_event_name=name,**kw)
  if mode=='stale':v['session_id']='wrong-session'
  command=settings['hooks'][name][0]['hooks'][0]
  p=subprocess.run([command['command'],*command['args']],input=json.dumps(v),text=True,capture_output=True)
  assert p.returncode==0,(p.stdout,p.stderr)
  response=json.loads(p.stdout) if p.stdout.strip() else {}
  return {'terminate':response.get('continue') is False}
 if hook('SessionStart',source='startup')['terminate']:sys.exit(0)
 if hook('MessageDisplay',message_id='one',index=0,final=False,delta=envelopes[0])['terminate']:sys.exit(0)
 response=hook('Stop',last_assistant_message='\n'.join(envelopes))
 assert response['terminate']
`
