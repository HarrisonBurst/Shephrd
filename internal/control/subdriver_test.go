package control

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/brief"
	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
)

func TestSubdriverBoundedContextAndMemoryAtResume(t *testing.T) {
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repoRoot := filepath.Join(root, "repo")
	if err = os.MkdirAll(filepath.Join(repoRoot, ".shephrd", "memory"), 0700); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{"AGENTS.md": "Repository guidance must be read", ".shephrd/context.md": "Scoped overview; no workflow selected", ".shephrd/memory.md": "MEMORY_INDEX_DO_NOT_HYDRATE", ".shephrd/memory/large.md": strings.Repeat("UNRELATED_TOPIC", 10000)} {
		if err = os.WriteFile(filepath.Join(repoRoot, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "bounded", Path: repoRoot, DefaultBranch: "main", ContextFile: "AGENTS.md"})
	if err != nil {
		t.Fatal(err)
	}
	var request model.SubdriverRequest
	for i := range 35 {
		request, err = state.HandoffSubdriver(repo.ID, "", "driver:main", fmt.Sprint(i), strings.Repeat("Original content remains in request records. ", 2000), "Read-only. Memory opt-out for this goal.", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	service := New(config.Config{DataDir: filepath.Join(root, "data"), Memory: config.MemoryConfig{Enabled: true}}, state)
	text, err := service.SubdriverContext(request.SubdriverID)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > subdriverStartupBytes || !strings.Contains(text, "--offset 20") || !strings.Contains(text, "AGENTS.md") || !strings.Contains(text, ".shephrd/context.md") {
		t.Fatalf("context lost bounds or pointers: %d %s", len(text), text)
	}
	events, found, diagnostic := adapter.ParseSubdriverCandidateSet(text)
	if !found || diagnostic != nil || len(events) != 2 || len(events[0].Checkpoint.Decisions) != 1 || len(events[0].Checkpoint.Checks) != 1 || !strings.Contains(text, "shephrd protocol validate --role subdriver") {
		t.Fatalf("brief lost valid typed examples or preflight: %+v %v", events, diagnostic)
	}
	for _, want := range []string{
		"Inspect the returned task, then use 'shephrd worker spawn",
		"Confirm the successful spawn receipt and current run before saying assigned or started",
		"A result immediately completes the conversational request",
		"reserve it for actual completion, never assignment or progress",
		"Worker completion is separate and does not complete the request",
		"do not reopen a done request or permit new dispatch",
		"subdriver handoff --lead-request <completed-request-id>",
		"Ordinary replies to nonterminal lead questions, blockers or handoffs continue through subdriver reply",
		"Check explicit lead_request_id and related request/worker links",
		"Preserve the original scope, actual user replies and existing task objective/acceptance contract",
		"no follow-up was queued or delivered and no hold was applied",
		"Annotations and narrative plans are not delivered instructions",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("coordination context missing %q", want)
		}
	}
	if strings.Contains(text, "UNRELATED_TOPIC") || strings.Contains(text, "MEMORY_INDEX_DO_NOT_HYDRATE") || strings.Contains(text, "Original content remains") {
		t.Fatal("hydrated unrelated memory or complete intake")
	}
	f, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "model-default", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err = state.StartSubdriver(f, 4242, "old-transcript-never-replayed"); err != nil {
		t.Fatal(err)
	}
	if err = state.FinishSubdriver(f, "Inspect durable request IDs before continuing", ""); err != nil {
		t.Fatal(err)
	}
	service.Config.Memory.Enabled = false
	text, err = service.SubdriverContext(request.SubdriverID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, brief.MemoryDisabledInstruction) || strings.Contains(text, "Follow `.shephrd/memory.md`") {
		t.Fatal("resume did not honor disabled memory")
	}
	if len(text) > subdriverStartupBytes {
		t.Fatal("resume context exceeds budget")
	}
}

func TestSubdriverRecoveryHoldsUnknownAndLiveProcesses(t *testing.T) {
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	request, err := state.HandoffSubdriver("", "Explicit research", "driver:main", "research", "question", "", "")
	if err != nil {
		t.Fatal(err)
	}
	service := New(config.Config{DataDir: root}, state)
	f, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "configured", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RecoverSubdriver(request.SubdriverID, f.Generation); err == nil {
		t.Fatal("unknown launch recovered without evidence")
	}
	if err = service.RecoverSubdriver(request.SubdriverID, f.Generation, "Operator confirmed source absent before any process launch"); err != nil {
		t.Fatal(err)
	}
	f, err = state.ReserveSubdriver(request.SubdriverID, f.Generation, "pi", "configured", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err = state.StartSubdriver(f, 123, "known"); err != nil {
		t.Fatal(err)
	}
	if err = state.SubdriverHarnessPID(f, 456); err != nil {
		t.Fatal(err)
	}
	service.processes.alive = func(pid int) bool { return pid == 456 }
	if err = service.RecoverSubdriver(request.SubdriverID, f.Generation, "must not override liveness"); err == nil {
		t.Fatal("live harness was ignored")
	}
	service.processes.alive = func(int) bool { return false }
	if err = service.PumpSubdrivers("driver:main"); err != nil {
		t.Fatal(err)
	}
	c, _ := state.Subdriver(request.SubdriverID)
	if c.State != "held" {
		t.Fatal("dead runner was not held")
	}
	notices, err := state.DrainNotifications("", "driver:main", "current", 10)
	if err != nil || len(notices.Notifications) != 1 || notices.Notifications[0].Kind != "subdriver-blocker" {
		t.Fatalf("held return: %+v %v", notices, err)
	}
	if err = service.RecoverSubdriver(c.ID, c.Generation); err != nil {
		t.Fatal(err)
	}
	if err = state.CheckSubdriverFence(f); err == nil {
		t.Fatal("old generation still has authority")
	}
}

func TestSubdriverPumpLaunchTimeoutUsesCurrentObservation(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("started=%t", started), func(t *testing.T) {
			state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			request, err := state.HandoffSubdriver("", "Research", "driver:main", "request", "Original", "", "")
			if err != nil {
				t.Fatal(err)
			}
			f, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "retained", "headless")
			if err != nil {
				t.Fatal(err)
			}
			observed, err := state.Subdriver(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			service := New(config.Config{}, state)
			service.clock.now = func() time.Time {
				if started {
					if err := state.StartSubdriver(f, 123, "registered-session"); err != nil {
						t.Fatal(err)
					}
				}
				return observed.UpdatedAt.Add(11 * time.Second)
			}
			if err := service.PumpSubdrivers("driver:other"); err != nil {
				t.Fatal(err)
			}
			untouched, err := state.Subdriver(f.ID)
			if err != nil || untouched.State != "starting" {
				t.Fatalf("foreign pump changed owner: %+v %v", untouched, err)
			}
			if err := service.PumpSubdrivers("driver:main"); err != nil {
				t.Fatal(err)
			}
			latest, err := state.Subdriver(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			notices, err := state.Notifications("")
			if err != nil {
				t.Fatal(err)
			}
			if started {
				if latest.State != "running" || latest.RunnerPID != 123 || latest.SessionID != "registered-session" || len(notices) != 0 {
					t.Fatalf("stale timeout held a registered runner: %+v %+v", latest, notices)
				}
			} else if latest.State != "held" || !strings.Contains(latest.Failure, "within 10 seconds") || len(notices) != 1 {
				t.Fatalf("genuine launch timeout not held: %+v %+v", latest, notices)
			}
		})
	}
}

type subdriverTerminalFixture struct {
	*intentFaultProvider
	store       *store.Store
	subdriverID string
	command     string
	label       string
	creates     int
}

func (p *subdriverTerminalFixture) WithSocket(string) terminal.RuntimeClient { return p }
func (p *subdriverTerminalFixture) CreateWorkspace(spec terminal.WorkspaceSpec) (terminal.Endpoint, error) {
	c, err := p.store.Subdriver(p.subdriverID)
	if err != nil {
		return terminal.Endpoint{}, err
	}
	if c.State != "starting" || !strings.Contains(spec.Source, c.ID) {
		return terminal.Endpoint{}, fmt.Errorf("launch intent was not durable")
	}
	p.creates++
	p.label = spec.Label
	return p.intentFaultProvider.CreateWorkspace(spec)
}
func (p *subdriverTerminalFixture) Start(endpoint terminal.Endpoint, command string) error {
	c, err := p.store.Subdriver(p.subdriverID)
	if err != nil {
		return err
	}
	if c.Endpoint == nil || c.Endpoint.PaneID != endpoint.PaneID {
		return fmt.Errorf("endpoint not recorded before start")
	}
	p.command = command
	p.started = true
	return nil
}
func TestSubdriverUsesSelectedTerminalAndRetainsUncertainEndpoint(t *testing.T) {
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	request, err := state.HandoffSubdriver("", "Explicit research", "driver:main", "research", "question", "", "")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err = os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pi", "shephrd"} {
		if err = os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHEPHRD_EXECUTABLE", filepath.Join(bin, "shephrd"))
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	t.Setenv("FIXTURE_TERMINAL_PARENT", "1")
	provider := &subdriverTerminalFixture{intentFaultProvider: &intentFaultProvider{endpoint: terminal.Endpoint{Backend: "fixture", SocketPath: "/fixture", WorkspaceID: "owned", TabID: "tab", PaneID: "pane"}}, store: state, subdriverID: request.SubdriverID}
	service := New(config.Config{DefaultHarness: "pi", WorkerRuntime: "fixture", DataDir: root}, state)
	service.TerminalProviders = []terminal.Provider{provider}
	service.processes.alive = func(int) bool { return false }
	c, err := service.ResumeSubdriver(request.SubdriverID, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Runtime != "fixture" || c.Endpoint == nil || provider.creates != 1 || !strings.Contains(provider.command, "'_run'") {
		t.Fatalf("runtime not preserved: %+v %q", c, provider.command)
	}
	if provider.label != "Sub-driver: General" || !strings.Contains(provider.command, "'subdriver' '_run' '"+c.ID+"'") {
		t.Fatalf("general terminal label = %q command = %q", provider.label, provider.command)
	}
	if _, err = service.ResumeSubdriver(c.ID, false); err == nil || provider.creates != 1 {
		t.Fatal("duplicate terminal session")
	}
	if err = service.RecoverSubdriver(c.ID, c.Generation, "cannot override a present endpoint"); err == nil {
		t.Fatal("present endpoint discarded")
	}
	if err = service.settleSubdriverEndpoint(c); err == nil {
		t.Fatal("foreground process closed")
	}
	provider.started = false
	if err = service.settleSubdriverEndpoint(c); err != nil {
		t.Fatal(err)
	}
	if !provider.closed {
		t.Fatal("owned settled endpoint not closed")
	}
}

func TestSubdriverModelOverrideRetainsLiveAndUncertainSessions(t *testing.T) {
	for _, stateName := range []string{"starting", "running", "held", "idle"} {
		for _, livePID := range []int{123, 456} {
			t.Run(fmt.Sprintf("%s/%d", stateName, livePID), func(t *testing.T) {
				state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer state.Close()
				request, err := state.HandoffSubdriver("", "Research", "driver:main", "selection", "Original", "", "")
				if err != nil {
					t.Fatal(err)
				}
				f, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "retained", "headless")
				if err != nil {
					t.Fatal(err)
				}
				if stateName != "starting" {
					if err := state.StartSubdriver(f, 123, "session"); err != nil {
						t.Fatal(err)
					}
					if err := state.SubdriverHarnessPID(f, 456); err != nil {
						t.Fatal(err)
					}
				}
				if stateName == "held" || stateName == "idle" {
					failure := ""
					if stateName == "held" {
						failure = "failed turn"
					}
					if err := state.FinishSubdriver(f, "retained checkpoint", failure); err != nil {
						t.Fatal(err)
					}
				}
				service := New(config.Config{}, state)
				service.processes.alive = func(pid int) bool { return pid == livePID }
				_, err = service.ResumeSubdriverWithModelSelection(f.ID, true, "replacement", true, &f.Generation)
				if err == nil || stateName == "idle" && !strings.Contains(err.Error(), "still live or uncertain") || stateName != "idle" && !strings.Contains(err.Error(), "is "+stateName) {
					t.Fatalf("unsafe resume: %v", err)
				}
				if stateName == "idle" {
					if err := service.PumpSubdrivers("driver:main"); err != nil {
						t.Fatal(err)
					}
				}
				c, err := state.Subdriver(f.ID)
				if err != nil {
					t.Fatal(err)
				}
				if c.Generation != f.Generation || c.State != stateName || c.Model != "retained" || c.Harness != "pi" {
					t.Fatalf("refused selection changed owner: %+v", c)
				}
			})
		}
	}
}

func TestSubdriverParserDoesNotWeakenWorkerArtifactContract(t *testing.T) {
	text := `<shephrd-event>{"type":"done","payload":"Session handled"}</shephrd-event>`
	if _, _, err := adapter.ParseEvents(text); err == nil {
		t.Fatal("ordinary worker done accepted without artifact")
	}
	events, found, diagnostic := adapter.ParseSubdriverCandidateSet(text)
	if !found || diagnostic != nil || len(events) != 1 {
		t.Fatalf("sub-driver session end: %+v %v", events, diagnostic)
	}
	for _, text := range []string{
		`<shephrd-event>{"type":"done","payload":"landed","artifact":"branch:main"}</shephrd-event>`,
		`<shephrd-event>{"type":"question","payload":"uncorrelated question"}</shephrd-event>`,
		`<shephrd-event>{"type":"done","payload":"end","approval":true}</shephrd-event>`,
	} {
		if _, _, diagnostic := adapter.ParseSubdriverCandidateSet(text); diagnostic == nil {
			t.Fatalf("invalid sub-driver event accepted: %s", text)
		}
	}
}

func TestSubdriverTerminalAndHeadlessNamesShowRegisteredRepository(t *testing.T) {
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repoRoot := filepath.Join(root, "repo")
	if err = os.MkdirAll(repoRoot, 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "Shephrd", Path: repoRoot, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := state.HandoffSubdriver(repo.ID, "", "driver:main", "naming", "Name the session", "", "")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err = os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	invocations := filepath.Join(root, "pi-invocations.log")
	if err = os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+invocations+"\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(bin, "shephrd"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHEPHRD_EXECUTABLE", filepath.Join(bin, "shephrd"))
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	t.Setenv("FIXTURE_TERMINAL_PARENT", "1")
	provider := &subdriverTerminalFixture{intentFaultProvider: &intentFaultProvider{endpoint: terminal.Endpoint{Backend: "fixture", SocketPath: "/fixture", WorkspaceID: "owned", TabID: "tab", PaneID: "pane"}}, store: state, subdriverID: request.SubdriverID}
	service := New(config.Config{DefaultHarness: "pi", WorkerRuntime: "fixture", DataDir: root}, state)
	service.TerminalProviders = []terminal.Provider{provider}
	service.processes.alive = func(int) bool { return false }
	c, err := service.ResumeSubdriver(request.SubdriverID, false)
	if err != nil {
		t.Fatal(err)
	}
	if provider.label != "Sub-driver: Shephrd" || c.DriverID() != "coordinator:"+c.ID || !strings.Contains(provider.command, "'subdriver' '_run' '"+c.ID+"'") {
		t.Fatalf("repository terminal label = %q owner = %q command = %q", provider.label, c.DriverID(), provider.command)
	}
	docsRoot := filepath.Join(root, "docs")
	if err = os.MkdirAll(docsRoot, 0700); err != nil {
		t.Fatal(err)
	}
	docs, err := state.UpsertRepo(model.Repo{Name: "Shephrd docs", Path: docsRoot, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	headless, err := state.HandoffSubdriver(docs.ID, "", "driver:main", "naming-headless", "Name the headless session", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f, err := state.ReserveSubdriver(headless.SubdriverID, 0, "pi", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	_ = service.RunSubdriver(f, io.Discard)
	body, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "--name Sub-driver: Shephrd docs ") {
		t.Fatalf("headless invocation did not carry the repository name: %s", body)
	}
}
