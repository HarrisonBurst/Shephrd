package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHerdrExtensionUnprovenAbortIsAnUnknownCreateSideEffect(t *testing.T) {
	temp := t.TempDir()
	script := filepath.Join(temp, "shephrd-terminal-herdr")
	body := `#!/bin/sh
set -eu
case "$1" in
  describe)
    printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"shephrd.terminal.herdr","version":"1.0.0"},"capabilities":[{"name":"terminal.herdr","version":1,"operations":["detect","diagnostics","create","start","inspect","process_info","read","focus","close","report_agent","release_agent","report_metadata"]}]}'
    ;;
  invoke)
    IFS= read -r request
    id=$(printf '%s' "$request" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
    printf '{"wire":{"major":1,"minor":0},"request_id":"%s","capability":"terminal.herdr","capability_version":1,"operation":"create","status":"prepared","result":{"endpoint":{"backend":"herdr","socket_path":"/socket"}},"extension":{"id":"shephrd.terminal.herdr","version":"1.0.0"}}\n' "$id"
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	provider := newTerminalExtensionProvider(herdrExtensionSpec(), []string{script}, hex.EncodeToString(digest[:]), nil)
	spec := WorkspaceSpec{WorkspaceID: "w7", CWD: "/tree", Label: "fixture", Harness: "pi", Source: "source", Generation: 1}
	prepared, err := provider.PrepareWorkspace(spec)
	if err == nil {
		t.Fatal("prepare should fail when the prepared endpoint is incomplete and the abort is unproven")
	}
	if prepared == nil {
		t.Fatalf("prepared workspace was not retained for recovery: err = %v", err)
	}
	if effect := CreateEffectFromError(prepared.Endpoint(), err); effect != CreateEffectUnknown {
		t.Fatalf("create side effect = %q, err = %v", effect, err)
	}
	var sideEffect *CreateSideEffect
	if !errors.As(err, &sideEffect) || sideEffect.Effect != CreateEffectUnknown {
		t.Fatalf("unproven abort was not classified as an unknown side effect: %v", err)
	}
}

func TestHerdrAndCmuxExtensionsUseSameTerminalCapabilityContract(t *testing.T) {
	herdr := HerdrExtensionCapability()
	cmux := CmuxExtensionCapability()
	if herdr.Version != cmux.Version || !reflect.DeepEqual(herdr.Operations, cmux.Operations) {
		t.Fatalf("herdr = %+v, cmux = %+v", herdr, cmux)
	}
	if herdr.Name != "terminal.herdr" || cmux.Name != "terminal.cmux" {
		t.Fatalf("herdr = %+v, cmux = %+v", herdr, cmux)
	}
	manifest := HerdrExtensionManifest()
	if manifest.Extension.ID != "shephrd.terminal.herdr" || len(manifest.Capabilities) != 1 || !reflect.DeepEqual(manifest.Capabilities[0], herdr) {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestHerdrExtensionPreservesConcreteTerminalOperations(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	binary := filepath.Join(temp, "shephrd-terminal-herdr")
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd-terminal-herdr")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Herdr extension: %s: %v", output, err)
	}
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	logPath := filepath.Join(temp, "herdr.log")
	closedPath := filepath.Join(temp, "closed")
	fakeBin := filepath.Join(temp, "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
set -eu
command="$*"
printf '%%s\n' "$command" >> %q
printf 'environment:%%s\n' "${UNRELATED_SECRET:-absent}" >> %q
case "$command" in
  "status server --json")
    printf '%%s\n' '{"running":true,"protocol":22,"compatible":true,"endpoint_compatible":true,"socket":"/socket","version":"0.9.1"}'
    ;;
  "pane get w7:p1")
    printf '%%s\n' '{"result":{"pane":{"pane_id":"w7:p1","tab_id":"w7:t1","workspace_id":"w7"}}}'
    ;;
  "tab get w7:t1")
    printf '%%s\n' '{"result":{"tab":{"tab_id":"w7:t1","workspace_id":"w7","pane_count":1}}}'
    ;;
  "tab create --workspace w7 --cwd /tree --label fixture --no-focus --env SAFE=value")
    rm -f %q
    printf '%%s\n' '{"result":{"root_pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7","cwd":"/tree","focused":false},"tab":{"tab_id":"w7:t2","workspace_id":"w7","label":"fixture","pane_count":1,"focused":false}}}'
    ;;
  "pane get w7:p2")
    if [ -e %q ]; then
      printf '%%s\n' '{"error":{"code":"pane_not_found","message":"gone"}}'
    else
      printf '%%s\n' '{"result":{"pane":{"pane_id":"w7:p2","tab_id":"w7:t2","workspace_id":"w7","cwd":"/tree","agent_status":"working","focused":false}}}'
    fi
    ;;
  "tab get w7:t2")
    printf '%%s\n' '{"result":{"tab":{"tab_id":"w7:t2","workspace_id":"w7","label":"fixture","agent_status":"working","pane_count":1,"focused":false}}}'
    ;;
  "pane report-metadata w7:p2 --source source --agent pi --title fixture --display-agent fixture --state-label working=working")
    printf '%%s\n' '{"result":{"type":"ok"}}'
    ;;
  "pane run w7:p2 exec '/launcher'")
    printf '%%s\n' '{"result":{"type":"ok"}}'
    ;;
  "pane process-info --pane w7:p2")
    printf '%%s\n' '{"result":{"process_info":{"pane_id":"w7:p2","shell_pid":900,"foreground_process_group_id":900,"foreground_processes":[{"pid":901,"pgid":900,"name":"shephrd","path":"/absolute/shephrd","cmdline":"shephrd _run attempt","argv":["/absolute/shephrd","_run","attempt"]}]}}}'
    ;;
  "pane read w7:p2 --source recent-unwrapped --lines 25 --format text")
    printf 'fixture output\n'
    ;;
  "pane report-agent w7:p2 --source source --agent pi --state working --seq 1 --agent-session-id session")
    printf '%%s\n' '{"result":{"type":"ok"}}'
    ;;
  "pane release-agent w7:p2 --source source --agent pi --seq 2")
    printf '%%s\n' '{"result":{"type":"ok"}}'
    ;;
  "workspace focus w7"|"tab focus w7:t2")
    printf '%%s\n' '{"result":{"type":"ok"}}'
    ;;
  "pane close w7:p2")
    : > %q
    printf '%%s\n' '{"result":{"type":"ok"}}'
    ;;
  *)
    printf 'unexpected Herdr fixture command: %%s\n' "$command" >&2
    exit 1
    ;;
esac
`, logPath, logPath, closedPath, closedPath, closedPath)
	if err := os.WriteFile(filepath.Join(fakeBin, "herdr"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	environment := []string{"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + temp, "UNRELATED_SECRET=not-forwarded"}
	provider := NewHerdrExtensionProvider([]string{binary}, hex.EncodeToString(digest[:]), environment)
	context := ParentContext{Values: map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": "/socket", "HERDR_WORKSPACE_ID": "w7", "HERDR_PANE_ID": "w7:p1"}, InvalidKeys: map[string]bool{}}
	detection := provider.Detect(context)
	if detection.State != DetectionMatched || detection.Parent.TabID != "w7:t1" || detection.Diagnostics.ProtocolVersion != "22" || detection.Diagnostics.ProviderVersion != "0.9.1" {
		t.Fatalf("detection = %+v", detection)
	}
	client := provider.WithSocket("/socket")
	if diagnostics, err := client.Diagnostics(); err != nil || !reflect.DeepEqual(diagnostics.Capabilities, []string{"exact-pane-lifecycle", "process-info", "state-report"}) {
		t.Fatalf("diagnostics = %+v, err = %v", diagnostics, err)
	}
	spec := WorkspaceSpec{WorkspaceID: "w7", CWD: "/tree", Label: "fixture", Harness: "pi", Source: "source", Environment: []string{"SAFE=value"}}
	aborted, err := client.(WorkspacePreparer).PrepareWorkspace(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := aborted.Abort(); err != nil {
		t.Fatal(err)
	}
	prepared, err := client.(WorkspacePreparer).PrepareWorkspace(spec)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := prepared.Endpoint()
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if endpoint.Backend != "herdr" || endpoint.WorkspaceID != "w7" || endpoint.TabID != "w7:t2" || endpoint.PaneID != "w7:p2" {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if err := client.Start(endpoint, "exec '/launcher'"); err != nil {
		t.Fatal(err)
	}
	state, err := client.Inspect(endpoint)
	if err != nil || state.Label != "fixture" || state.PaneCount != 1 || state.SurfaceCount != 1 || state.Focused {
		t.Fatalf("state = %+v, err = %v", state, err)
	}
	info, err := client.ProcessInfo(endpoint)
	if err != nil || info.ForegroundProcessGroup != 900 || len(info.ForegroundProcesses) != 1 || info.ForegroundProcesses[0].Path != "/absolute/shephrd" {
		t.Fatalf("process info = %+v, err = %v", info, err)
	}
	if output, err := client.Read(endpoint, 25); err != nil || output != "fixture output\n" {
		t.Fatalf("output = %q, err = %v", output, err)
	}
	if err := client.ReportAgent(endpoint, AgentReport{Source: "source", Agent: "pi", State: "working", Sequence: 1, SessionID: "session"}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseAgent(endpoint, "source", "pi", 2); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportMetadata(endpoint, "fixture", "pi", "source", "working"); err != nil {
		t.Fatal(err)
	}
	if err := client.Focus(endpoint); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(endpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(endpoint); !errors.Is(err, ErrNotFound) || Classify(err) != ErrorEndpointAbsent {
		t.Fatalf("closed inspect error = %v", err)
	}
	logBody, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBody)
	for _, command := range []string{
		"status server --json",
		"pane get w7:p1",
		"tab get w7:t1",
		"tab create --workspace w7 --cwd /tree --label fixture --no-focus --env SAFE=value",
		"pane report-metadata w7:p2 --source source --agent pi --title fixture --display-agent fixture --state-label working=working",
		"pane run w7:p2 exec '/launcher'",
		"pane process-info --pane w7:p2",
		"pane read w7:p2 --source recent-unwrapped --lines 25 --format text",
		"pane report-agent w7:p2 --source source --agent pi --state working --seq 1 --agent-session-id session",
		"pane release-agent w7:p2 --source source --agent pi --seq 2",
		"workspace focus w7",
		"tab focus w7:t2",
		"pane close w7:p2",
	} {
		if !strings.Contains(log, command+"\n") {
			t.Fatalf("command %q missing from log:\n%s", command, log)
		}
	}
	if strings.Count(log, "pane close w7:p2\n") < 2 {
		t.Fatalf("prepared abort and exact close were not both observed:\n%s", log)
	}
	if strings.Contains(log, "not-forwarded") || !strings.Contains(log, "environment:absent\n") {
		t.Fatalf("unrelated environment leaked into extension log:\n%s", log)
	}
}
