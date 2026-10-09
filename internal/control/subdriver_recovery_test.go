package control

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
)

type endpointProbeFixture struct {
	*intentFaultProvider
	probeErr error
}

func (p *endpointProbeFixture) WithSocket(string) terminal.RuntimeClient { return p }
func (p *endpointProbeFixture) ProcessInfo(endpoint terminal.Endpoint) (terminal.ProcessInfo, error) {
	if p.probeErr != nil {
		return terminal.ProcessInfo{}, p.probeErr
	}
	return p.intentFaultProvider.ProcessInfo(endpoint)
}

func errorKind(err error) string {
	var typed *model.KindError
	if errors.As(err, &typed) {
		return typed.Kind
	}
	return ""
}

func heldOwnerFixture(t *testing.T, runtime string, runner, harness int) (*store.Store, model.SubdriverRequest, model.SubdriverFence) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	request, err := state.HandoffSubdriver("", "Research", "driver:hermes", "owned", "Original", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "retained", runtime)
	if err != nil {
		t.Fatal(err)
	}
	if runtime != "headless" {
		if err = state.SetSubdriverEndpoint(f, model.TerminalEndpoint{Backend: "fixture", SocketPath: "/fixture", WorkspaceID: "owned", TabID: "tab", PaneID: "pane"}); err != nil {
			t.Fatal(err)
		}
	}
	if runner != 0 {
		if err = state.StartSubdriver(f, runner, "session"); err != nil {
			t.Fatal(err)
		}
		if harness != 0 {
			if err = state.SubdriverHarnessPID(f, harness); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = state.FinishSubdriver(f, "retained checkpoint", "failed turn"); err != nil {
		t.Fatal(err)
	}
	return state, request, f
}

func TestOwnerRecoveryKeepsProcessEndpointAndIdentityFences(t *testing.T) {
	const owner = "driver:hermes"
	selected := "replacement"
	for _, test := range []struct {
		name         string
		runtime      string
		runner       int
		live         int
		probeErr     error
		launchAbsent string
		endpoint     string
		recovered    bool
	}{
		{name: "proven absent", runtime: "headless", runner: 101, endpoint: "unrecorded", recovered: true},
		{name: "live runner", runtime: "headless", runner: 101, live: 101, endpoint: "unrecorded"},
		{name: "live harness despite assertion", runtime: "headless", runner: 101, live: 202, launchAbsent: "Launcher log shows nothing else", endpoint: "unrecorded"},
		{name: "incomplete identity", runtime: "headless", endpoint: "unrecorded"},
		{name: "incomplete identity with assertion", runtime: "headless", launchAbsent: "Launcher log and exact source show no harness start", endpoint: "unrecorded", recovered: true},
		{name: "present endpoint", runtime: "fixture", runner: 101, launchAbsent: "cannot override", endpoint: "present"},
		{name: "uncertain endpoint", runtime: "fixture", runner: 101, probeErr: errors.New("socket unavailable"), launchAbsent: "cannot override", endpoint: "uncertain"},
		{name: "absent endpoint", runtime: "fixture", runner: 101, probeErr: terminal.ErrNotFound, endpoint: "absent", recovered: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := 0
			if test.runner != 0 {
				harness = 202
			}
			state, _, f := heldOwnerFixture(t, test.runtime, test.runner, harness)
			service := New(config.Config{}, state)
			service.TerminalProviders = []terminal.Provider{&endpointProbeFixture{intentFaultProvider: &intentFaultProvider{}, probeErr: test.probeErr}}
			service.processes.alive = func(pid int) bool { return pid > 0 && pid == test.live }
			d, err := service.DiagnoseSubdriver(f.ID, owner)
			if err != nil {
				t.Fatal(err)
			}
			if d.Endpoint != test.endpoint || d.Recoverable != (test.recovered && test.launchAbsent == "") || d.State != "held" || len(d.RequestOwners) != 1 || d.RequestOwners[0].DriverID != owner {
				t.Fatalf("diagnosis %+v", d)
			}
			err = service.RecoverSubdriver(f.ID, f.Generation, model.SubdriverRecovery{DriverID: owner, LaunchAbsent: test.launchAbsent, Model: &selected})
			c, readErr := state.Subdriver(f.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if test.recovered != (err == nil) || test.recovered != (c.State == "idle") || test.recovered != (c.Model == selected) {
				t.Fatalf("recovery err=%v owner=%+v", err, c)
			}
		})
	}
}

func TestOwnerRecoveryRefusesForeignStaleAndInvalidSelection(t *testing.T) {
	state, request, f := heldOwnerFixture(t, "headless", 101, 202)
	service := New(config.Config{}, state)
	service.processes.alive = func(int) bool { return false }
	if _, err := service.DiagnoseSubdriver(f.ID, "driver:stranger"); errorKind(err) != "subdriver_owner_refused" {
		t.Fatalf("foreign diagnosis: %v", err)
	}
	selected := "two words"
	for _, check := range []struct {
		recovery   model.SubdriverRecovery
		generation int
		want       string
	}{
		{model.SubdriverRecovery{DriverID: "driver:stranger"}, f.Generation, "not owned solely"},
		{model.SubdriverRecovery{DriverID: "driver:hermes"}, f.Generation - 1, "generation changed"},
		{model.SubdriverRecovery{DriverID: "driver:hermes", Model: &selected}, f.Generation, "exact pi model identifier"},
	} {
		if err := service.RecoverSubdriver(f.ID, check.generation, check.recovery); err == nil || !strings.Contains(err.Error(), check.want) {
			t.Fatalf("%+v: %v", check.recovery, err)
		}
	}
	if err := state.AdoptSubdriverRequest(request.ID, "driver:hermes", "driver:other"); err != nil {
		t.Fatal(err)
	}
	d, err := service.DiagnoseSubdriver(f.ID, "driver:other")
	if err != nil || !d.Recoverable || len(d.Blockers) != 0 {
		t.Fatalf("sole new owner: %+v %v", d, err)
	}
}

func TestOwnerResumeRequiresExplicitHeadlessGenerationAndOwner(t *testing.T) {
	state, request, f := heldOwnerFixture(t, "headless", 101, 202)
	if err := state.RecoverSubdriver(f.ID, f.Generation, model.SubdriverRecovery{}); err != nil {
		t.Fatal(err)
	}
	generation := f.Generation
	for _, check := range []struct {
		runtime    string
		env        string
		foreground bool
		owner      string
		generation *int
		want       string
	}{
		{runtime: "headless", owner: "driver:hermes", want: "requires --generation"},
		{runtime: "headless", owner: "driver:hermes", generation: &generation, foreground: true, want: "never runs a foreground"},
		{runtime: "auto", owner: "driver:hermes", generation: &generation, want: "explicitly headless"},
		{runtime: "herdr", owner: "driver:hermes", generation: &generation, want: "explicitly headless"},
		{runtime: "headless", env: "auto", owner: "driver:hermes", generation: &generation, want: "explicitly headless"},
		{runtime: "headless", owner: "driver:stranger", generation: &generation, want: "not owned solely"},
	} {
		t.Setenv("SHEPHRD_WORKER_RUNTIME", check.env)
		service := New(config.Config{WorkerRuntime: check.runtime}, state)
		service.processes.alive = func(int) bool { return false }
		if _, err := service.ResumeSubdriverWithModelSelection(request.SubdriverID, check.foreground, "", false, check.generation, check.owner); err == nil || !strings.Contains(err.Error(), check.want) {
			t.Fatalf("%+v: %v", check, err)
		}
	}
	c, err := state.Subdriver(f.ID)
	if err != nil || c.State != "idle" || c.Generation != f.Generation {
		t.Fatalf("refused owner resume changed state: %+v %v", c, err)
	}
	f, err = state.ReserveSubdriver(f.ID, f.Generation, "pi", "retained", "herdr")
	if err != nil {
		t.Fatal(err)
	}
	if err = state.FinishSubdriver(f, "", ""); err != nil {
		t.Fatal(err)
	}
	generation = f.Generation
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	service := New(config.Config{WorkerRuntime: "headless"}, state)
	if _, err = service.ResumeSubdriverWithModelSelection(f.ID, false, "", false, &generation, "driver:hermes"); err == nil || !strings.Contains(err.Error(), `retained "herdr"`) {
		t.Fatalf("retained Herdr owner resumed headless: %v", err)
	}
}
