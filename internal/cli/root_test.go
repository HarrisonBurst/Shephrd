package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"shephrd/internal/control"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestErrorKindClassifiesNotificationClaimLoss(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", store.ErrNotificationConflict), "claim_conflict"},
		{fmt.Errorf("wrapped: %w", store.ErrNotificationStale), "claim_stale"},
		{fmt.Errorf("wrapped: %w", store.ErrNotificationExpired), "claim_expired"},
		{fmt.Errorf("wrapped: %w", model.Failure("pr_not_merged", "not merged")), "pr_not_merged"},
		{fmt.Errorf("wrapped: %w", store.ErrProtocolViolation), ""},
		{fmt.Errorf("wrapped: %w", store.ErrRepoNameConflict), ""},
		{fmt.Errorf("wrapped: %w", store.ErrTaskFeatureConflict), ""},
		{fmt.Errorf("wrapped: %w", store.ErrPlanNameConflict), ""},
		{fmt.Errorf("wrapped: %w", store.ErrPlanPrerequisiteCycle), ""},
		{fmt.Errorf("transient"), ""},
	} {
		if got := ErrorKind(test.err); got != test.want {
			t.Fatalf("ErrorKind(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}

func TestWorkerModelFlagsAreAvailable(t *testing.T) {
	root := New()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"worker", "spawn", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "--model") || !strings.Contains(output.String(), "cmux") {
		t.Fatalf("spawn help = %q", output.String())
	}
	output.Reset()
	root.SetArgs([]string{"worker", "retry", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "--model") || !strings.Contains(output.String(), "cmux") {
		t.Fatalf("retry help = %q", output.String())
	}
}

func TestTaskAttestDeliveryAndVerifyFlagsAreAvailable(t *testing.T) {
	for _, test := range []struct {
		args []string
		want []string
	}{
		{args: []string{"task", "attest-delivery", "--help"}, want: []string{"--pr", "--commit", "--attempt", "--driver-id"}},
		{args: []string{"task", "attest-report-recovery", "--help"}, want: []string{"--attempt", "--run-generation", "--checkpoint-revision", "--checkpoint-cursor", "--reason", "--driver-id"}},
		{args: []string{"task", "verify-delivery", "--help"}, want: []string{"--attempt", "--driver-id", "--local"}},
	} {
		root := New()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(test.args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		for _, want := range test.want {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("%v help missing %q: %s", test.args, want, output.String())
			}
		}
	}
	for _, test := range []struct {
		args []string
		kind string
	}{
		{args: []string{"task", "attest-delivery", "task"}, kind: "pr_url_invalid"},
		{args: []string{"task", "attest-delivery", "task", "--attempt="}, kind: "attempt_not_eligible"},
		{args: []string{"task", "attest-delivery", "task", "--pr", "https://github.com/acme/demo/pull/1"}, kind: "sealed_commit_mismatch"},
		{args: []string{"task", "attest-report-recovery", "task"}, kind: "attempt_not_eligible"},
		{args: []string{"task", "verify-delivery", "task", "--attempt="}, kind: "attempt_not_eligible"},
	} {
		root := New()
		root.SetArgs(test.args)
		err := root.Execute()
		if err == nil || ErrorKind(err) != test.kind {
			t.Fatalf("%v error=%v kind=%q", test.args, err, ErrorKind(err))
		}
	}
}

func TestTaskArchiveCommandAndListFlagAreAvailable(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"task", "archive", "--help"}, want: "Soft-archive a terminal task"},
		{args: []string{"task", "list", "--help"}, want: "--include-archived"},
	} {
		root := New()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(test.args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), test.want) {
			t.Fatalf("%v help = %q", test.args, output.String())
		}
	}
}

func TestPortableSkillCommandMapCoversPublicCommandFamilies(t *testing.T) {
	root := New()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	public := map[string]bool{}
	for _, command := range root.Commands() {
		if !command.Hidden {
			public[command.Name()] = true
		}
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate source root")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	body, err := os.ReadFile(filepath.Join(projectRoot, ".agents", "skills", "shephrd", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	const heading = "## Command map\n"
	start := strings.Index(string(body), heading)
	if start < 0 {
		t.Fatal("SKILL.md is missing the command map")
	}
	section := string(body)[start+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	for _, required := range []string{"shephrd --help", "shephrd <command> --help", "uncommon", "version-specific"} {
		if !strings.Contains(section, required) {
			t.Errorf("SKILL.md command map is missing targeted-help rule %q", required)
		}
	}
	mapped := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		const prefix = "- **`"
		const separator = "`**:"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(line, prefix), separator, 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			t.Fatalf("invalid command-map entry %q", line)
		}
		mapped[parts[0]] = true
	}

	omitted := map[string]string{}
	for name := range public {
		if mapped[name] {
			continue
		}
		if strings.TrimSpace(omitted[name]) == "" {
			t.Errorf("public command family %q is absent from the SKILL.md command map", name)
		}
	}
	for name := range mapped {
		if !public[name] {
			t.Errorf("SKILL.md maps non-public command family %q", name)
		}
	}
	for name, reason := range omitted {
		if !public[name] || strings.TrimSpace(reason) == "" {
			t.Errorf("invalid explicit command-map omission %q: %q", name, reason)
		}
	}
}

func TestGeneratedCommandManifestMatchesCurrentContract(t *testing.T) {
	root := New()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	var visible, hidden []string
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		for _, command := range parent.Commands() {
			path := command.CommandPath()
			if command.Hidden {
				hidden = append(hidden, path)
			} else {
				visible = append(visible, path)
			}
			walk(command)
		}
	}
	walk(root)
	sort.Strings(visible)
	sort.Strings(hidden)
	wantVisible := []string{
		"shephrd subdriver", "shephrd subdriver adopt-request", "shephrd subdriver context", "shephrd subdriver dispatch", "shephrd subdriver event", "shephrd subdriver handled", "shephrd subdriver handoff", "shephrd subdriver inspect", "shephrd subdriver ls", "shephrd subdriver notification", "shephrd subdriver recover", "shephrd subdriver reply", "shephrd subdriver request", "shephrd subdriver resume", "shephrd subdriver return",
		"shephrd completion", "shephrd completion bash", "shephrd completion fish", "shephrd completion zsh",
		"shephrd help", "shephrd plan", "shephrd plan add", "shephrd plan adopt", "shephrd plan annotate", "shephrd plan annotations", "shephrd plan create", "shephrd plan dispatch", "shephrd plan edit", "shephrd plan ls", "shephrd plan report", "shephrd plan report add", "shephrd plan report move", "shephrd plan report rm", "shephrd plan report select", "shephrd plan requires", "shephrd plan requires add", "shephrd plan requires rm", "shephrd plan rm", "shephrd plan show",
		"shephrd legacy", "shephrd legacy export",
		"shephrd protocol", "shephrd protocol validate",
		"shephrd repo", "shephrd repo add", "shephrd repo context", "shephrd repo list", "shephrd repo scan",
		"shephrd task", "shephrd task adopt", "shephrd task annotate", "shephrd task annotations", "shephrd task archive", "shephrd task attest-delivery", "shephrd task attest-report-recovery", "shephrd task create", "shephrd task inspect", "shephrd task list", "shephrd task obligations", "shephrd task verify-delivery",
		"shephrd wake", "shephrd wake ack", "shephrd wake drain", "shephrd wake parked", "shephrd wake pump", "shephrd wake renew", "shephrd wake unpark", "shephrd wake watch", "shephrd worker", "shephrd worker focus", "shephrd worker intent", "shephrd worker intent clear", "shephrd worker peek", "shephrd worker relaunch", "shephrd worker retry", "shephrd worker send", "shephrd worker spawn", "shephrd worker status", "shephrd worker stop", "shephrd workspace", "shephrd workspace reconcile", "shephrd workspace release",
	}
	sort.Strings(wantVisible)
	wantHidden := []string{"shephrd _claude-hook", "shephrd _run", "shephrd subdriver _run"}
	if strings.Join(visible, "\n") != strings.Join(wantVisible, "\n") {
		t.Fatalf("visible command manifest:\n%s", strings.Join(visible, "\n"))
	}
	if strings.Join(hidden, "\n") != strings.Join(wantHidden, "\n") {
		t.Fatalf("hidden command manifest: %v", hidden)
	}
	for path, flags := range map[string][]string{
		"task create":       {"title"},
		"plan add":          {"title"},
		"plan edit":         {"title"},
		"plan dispatch":     {"spawn", "harness", "model", "runtime"},
		"task annotate":     {"driver-id", "reason", "next-action", "expect-revision"},
		"plan annotate":     {"driver-id", "reason", "next-action", "expect-revision"},
		"task adopt":        {"driver-id", "new-driver-id"},
		"plan adopt":        {"driver-id", "new-driver-id"},
		"worker retry":      {"harness", "model", "runtime"},
		"workspace release": {"attempt", "discard"},
		"wake watch":        {"driver-id", "json-log"},
		"wake parked":       {"driver-id"},
		"wake unpark":       {"driver-id"},
	} {
		command, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range flags {
			if command.Flags().Lookup(flag) == nil && command.InheritedFlags().Lookup(flag) == nil {
				t.Fatalf("%s missing --%s", path, flag)
			}
		}
	}
	for _, path := range []string{"task create", "task obligations"} {
		command, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Fatal(err)
		}
		if command.Flags().Lookup("group") != nil {
			t.Fatalf("%s retains --group", path)
		}
	}
	for _, path := range []string{"task create", "plan add"} {
		command, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Fatal(err)
		}
		flag := command.Flags().Lookup("title")
		if flag == nil {
			t.Fatalf("%s is missing --title", path)
		}
		if values := flag.Annotations[cobra.BashCompOneRequiredFlag]; len(values) != 0 {
			t.Fatalf("%s --title is still marked required for generated completion", path)
		}
	}
}

func TestHelpJSONAndCompletionOutputContracts(t *testing.T) {
	for _, args := range [][]string{{"help", "worker", "--json"}, {"worker", "--help", "--json"}} {
		root := New()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var response helpResponse
		if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.SchemaVersion != 1 || response.Command != "shephrd worker" || !strings.Contains(response.Help, "spawn") {
			t.Fatalf("%v help response=%+v err=%v body=%q", args, response, err, output.String())
		}
	}
	root := New()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"completion", "bash", "--json"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "not supported") || output.Len() != 0 {
		t.Fatalf("JSON completion error=%v output=%q", err, output.String())
	}
	for _, test := range []struct {
		shell string
		want  string
	}{
		{shell: "bash", want: "bash completion"},
		{shell: "fish", want: "fish completion"},
		{shell: "zsh", want: "#compdef shephrd"},
	} {
		root = New()
		output.Reset()
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs([]string{"completion", test.shell})
		if err := root.Execute(); err != nil || !strings.Contains(output.String(), test.want) {
			t.Fatalf("%s completion error=%v output=%q", test.shell, err, output.String())
		}
	}
	completion, _, err := New().Find([]string{"completion"})
	if err != nil {
		t.Fatal(err)
	}
	var shells []string
	for _, command := range completion.Commands() {
		shells = append(shells, command.Name())
	}
	if strings.Join(shells, ",") != "bash,fish,zsh" {
		t.Fatalf("completion shells = %v", shells)
	}
}

func TestCoreFreshnessCommandAndAdvisoryFieldsAreAbsent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SHEPHRD_CONFIG", configPath)
	root := New()
	root.SetArgs([]string{"freshness", "--json"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("freshness command error = %v", err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("removed command bootstrapped config: %v", err)
	}
	body, err := json.Marshal(model.SpawnResult{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("freshness")) || bytes.Contains(body, []byte("advisory")) {
		t.Fatalf("spawn result retains freshness JSON: %s", body)
	}
}

func TestFreshnessClassifierIsOutsideCoreDependencyGraph(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	dependencies := func(commandPath string) string {
		t.Helper()
		command := exec.Command("go", "list", "-deps", commandPath)
		command.Dir = root
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %s: %v", commandPath, output, err)
		}
		return "\n" + string(output)
	}
	const classifier = "\nshephrd/internal/freshness\n"
	if core := dependencies("./cmd/shephrd"); strings.Contains(core, classifier) {
		t.Fatal("core delegation binary depends on the freshness classifier")
	}
	if diagnostic := dependencies("./cmd/shephrd-freshness"); !strings.Contains(diagnostic, classifier) {
		t.Fatal("standalone diagnostic does not reuse the freshness classifier")
	}
}

func TestRuntimeExecutableIsVisibleInTaskAndWorkerDetails(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("SHEPHRD_STATE_DIR", stateDir)
	t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
	state, err := store.Open(filepath.Join(stateDir, "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: dir, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "runtime", Objective: "show runtime"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", dir, "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	const executable = "/configured/shephrd"
	if err := state.SetRuntimeExecutableForRun(attempt.ID, attempt.RunGeneration, executable); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		read func([]byte) string
	}{
		{args: []string{"task", "inspect", task.ID, "--json"}, read: func(body []byte) string {
			var detail model.TaskDetail
			if err := json.Unmarshal(body, &detail); err != nil {
				t.Fatal(err)
			}
			return detail.Attempts[0].RuntimeExecutable
		}},
		{args: []string{"worker", "status", task.ID, "--json"}, read: func(body []byte) string {
			var value map[string]any
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprint(value["runtime_executable"])
		}},
	} {
		root := New()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(test.args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if got := test.read(output.Bytes()); got != executable {
			t.Fatalf("%v runtime executable = %q", test.args, got)
		}
	}
}

func TestWorkerStatusAndPeekJSONContractRetireLegacyEndpointFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("SHEPHRD_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
	state, err := store.Open(filepath.Join(dir, "state", "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: dir, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Endpoint contract", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "endpoint-contract", Objective: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	runJSON := func(args ...string) []byte {
		t.Helper()
		root := New()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return output.Bytes()
	}
	var status map[string]any
	if err := json.Unmarshal(runJSON("worker", "status", task.ID, "--json"), &status); err != nil {
		t.Fatal(err)
	}
	if status["schema_version"] != float64(2) {
		t.Fatalf("status schema version = %v", status["schema_version"])
	}
	if _, present := status["terminal_endpoint"]; !present {
		t.Fatalf("terminal_endpoint absent from status JSON: %v", status)
	}
	if _, present := status["endpoint_present"]; !present {
		t.Fatalf("endpoint_present absent from status JSON: %v", status)
	}
	for _, retired := range []string{"herdr_socket_path", "herdr_workspace_id", "herdr_tab_id", "herdr_pane_id", "pane_present"} {
		if _, present := status[retired]; present {
			t.Fatalf("status JSON still exposes retired field %q", retired)
		}
	}
	state, err = store.Open(filepath.Join(dir, "state", "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetRuntimeBackend(attempt.ID, "herdr"); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetTerminalEndpointForRun(attempt.ID, attempt.RunGeneration, model.TerminalEndpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: "ws", TabID: "tab", PaneID: "pane"}); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	var detail model.TaskDetail
	if err := json.Unmarshal(runJSON("task", "inspect", task.ID, "--json"), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Attempts[0].TerminalEndpoint == nil || detail.Attempts[0].TerminalEndpoint.SocketPath != "/socket" || detail.Attempts[0].TerminalEndpoint.WorkspaceID != "ws" || detail.Attempts[0].TerminalEndpoint.TabID != "tab" || detail.Attempts[0].TerminalEndpoint.PaneID != "pane" {
		t.Fatalf("inspect terminal endpoint = %+v", detail.Attempts[0].TerminalEndpoint)
	}
	attemptJSON, err := json.Marshal(detail.Attempts[0])
	if err != nil {
		t.Fatal(err)
	}
	var attemptMap map[string]any
	if err := json.Unmarshal(attemptJSON, &attemptMap); err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"herdr_socket_path", "herdr_workspace_id", "herdr_tab_id", "herdr_pane_id", "pane_present"} {
		if _, present := attemptMap[retired]; present {
			t.Fatalf("attempt JSON still exposes retired field %q", retired)
		}
	}
	var peekBody []byte
	if peekBody, err = json.Marshal(control.PeekResult{SchemaVersion: 1, EndpointPresent: true}); err != nil {
		t.Fatal(err)
	}
	var peek map[string]any
	if err := json.Unmarshal(peekBody, &peek); err != nil {
		t.Fatal(err)
	}
	if _, present := peek["endpoint_present"]; !present {
		t.Fatalf("peek JSON lost endpoint_present: %v", peek)
	}
	if _, present := peek["pane_present"]; present {
		t.Fatalf("peek JSON still exposes retired field pane_present")
	}
}

func TestRunRequiresPositiveGeneration(t *testing.T) {
	for _, args := range [][]string{{"_run", "attempt", "--input", "brief"}, {"_run", "attempt", "--input", "brief", "--run-generation", "0"}} {
		dir := t.TempDir()
		t.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
		t.Setenv("SHEPHRD_STATE_DIR", filepath.Join(dir, "state"))
		t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
		root := New()
		root.SetArgs(args)
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--run-generation must be at least 1") {
			t.Fatalf("%v error = %v", args, err)
		}
	}
}

func TestTaskCreateInfersPiDriverAndSupportsExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("SHEPHRD_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
	repoPath := filepath.Join(dir, "repo")
	if err := exec.Command("git", "init", "-b", "main", repoPath).Run(); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repoPath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base").CombinedOutput(); err != nil {
		t.Fatalf("create repo commit: %s: %v", output, err)
	}
	runCLI := func(args ...string) string {
		t.Helper()
		root := New()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, output.String())
		}
		return output.String()
	}
	runCLI("repo", "add", repoPath, "--json")
	t.Setenv("PI_SESSION_ID", "pi-session")
	var inferredTask model.Task
	for _, test := range []struct {
		feature string
		extra   []string
		want    string
		title   string
	}{
		{feature: "inferred", want: "driver:pi:pi-session", title: "work"},
		{feature: "explicit", extra: []string{"--driver-id", "driver:explicit", "--title", "Test task"}, want: "driver:explicit", title: "Test task"},
	} {
		args := []string{"task", "create", "--repo", repoPath, "--feature", test.feature, "work", "--json"}
		args = append(args, test.extra...)
		body := runCLI(args...)
		if strings.Contains(body, `"group"`) {
			t.Fatalf("task create JSON retains group: %s", body)
		}
		var task model.Task
		if err := json.Unmarshal([]byte(body), &task); err != nil {
			t.Fatal(err)
		}
		if task.DriverID != test.want {
			t.Fatalf("driver = %q, want %q", task.DriverID, test.want)
		}
		if task.Title != test.title {
			t.Fatalf("title = %q, want %q", task.Title, test.title)
		}
		if test.feature == "inferred" {
			inferredTask = task
		}
	}
	var inventory []model.Task
	if err := json.Unmarshal([]byte(runCLI("task", "list", "--json")), &inventory); err != nil {
		t.Fatal(err)
	}
	foundTitle := false
	for _, task := range inventory {
		if task.ID == inferredTask.ID && task.Title == inferredTask.Title {
			foundTitle = true
		}
	}
	if !foundTitle {
		t.Fatalf("task list omitted title for %s: %+v", inferredTask.ID, inventory)
	}
	var detail model.TaskDetail
	if err := json.Unmarshal([]byte(runCLI("task", "inspect", inferredTask.ID, "--json")), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Task.Title != inferredTask.Title {
		t.Fatalf("task inspect title = %q", detail.Task.Title)
	}
	var adoption model.TaskAdoption
	if err := json.Unmarshal([]byte(runCLI("task", "adopt", inferredTask.ID, "--driver-id", "driver:pi:pi-session", "--new-driver-id", "driver:adopted", "--json")), &adoption); err != nil {
		t.Fatal(err)
	}
	if adoption.PreviousDriverID != "driver:pi:pi-session" || adoption.DriverID != "driver:adopted" {
		t.Fatalf("adoption = %+v", adoption)
	}
}

func TestTaskGroupFlagsAreRejected(t *testing.T) {
	for _, args := range [][]string{
		{"task", "create", "--group", "removed", "objective"},
		{"task", "obligations", "--group", "removed"},
	} {
		root := New()
		root.SetArgs(args)
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "unknown flag: --group") {
			t.Fatalf("%v error = %v", args, err)
		}
	}
}

func TestTaskCreateReportInputFlagIsRepeatableAndRejectsEmptyValues(t *testing.T) {
	root := New()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"task", "create", "--title", "Test task", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "--with-report-from") {
		t.Fatalf("task create help = %q", output.String())
	}
	root = New()
	root.SetArgs([]string{"task", "create", "--title", "Test task", "--driver-id", "driver:test", "--repo", "repo", "--feature", "target", "--with-report-from", "task_one", "--with-report-from", "task_two", "objective"})
	if err := root.Execute(); err == nil || strings.Contains(err.Error(), "only once") {
		t.Fatalf("repeated flag was not accepted: %v", err)
	}
	root = New()
	root.SetArgs([]string{"task", "create", "--title", "Test task", "--repo", "repo", "--feature", "target", "--with-report-from=", "objective"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "requires a producer") {
		t.Fatalf("empty flag error = %v", err)
	}
}

func TestPlanDispatchSpawnValidationAndPartialFailure(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("SHEPHRD_STATE_DIR", stateDir)
	t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("PI_SESSION_ID", "dispatch-partial")
	state, err := store.Open(filepath.Join(stateDir, "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: dir, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := state.CreatePlan("partial", "driver:pi:dispatch-partial")
	if err != nil {
		t.Fatal(err)
	}
	item, err := state.AddPlanItem(list.ID, model.PlanItem{Title: "Partial spawn", FeatureKey: "partial-spawn", Objective: "Dispatch then fail spawn", AcceptanceCriteria: "queued task retained", RepoID: repo.ID}, model.PlanItemRelations{})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"plan", "dispatch", list.ID, item.ID, "--harness", "pi"},
		{"plan", "dispatch", list.ID, item.ID, "--spawn", "--model", "model-only"},
	} {
		root := New()
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
	}
	root := New()
	root.SetArgs([]string{"plan", "dispatch", list.ID, item.ID, "--spawn", "--harness", "invalid"})
	dispatchErr := root.Execute()
	if dispatchErr == nil {
		t.Fatal("dispatch with invalid spawn harness succeeded")
	}
	state, err = store.Open(filepath.Join(stateDir, "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	persisted, err := state.PlanItem(list.ID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.DispatchedTaskID == "" {
		t.Fatal("failed spawn rolled back dispatch")
	}
	task, err := state.Task(persisted.DispatchedTaskID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := state.Attempts(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovery := "shephrd worker spawn " + task.ID
	if task.Status != model.TaskStatusQueued || task.CurrentAttemptID != "" || len(attempts) != 0 || !strings.Contains(dispatchErr.Error(), task.ID) || !strings.Contains(dispatchErr.Error(), recovery) {
		t.Fatalf("partial dispatch task=%+v attempts=%+v error=%v", task, attempts, dispatchErr)
	}
}

func TestTaskAndPlanItemCreateDeriveAndReturnTitles(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("SHEPHRD_STATE_DIR", stateDir)
	t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("PI_SESSION_ID", "title-derivation")
	state, err := store.Open(filepath.Join(stateDir, "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.UpsertRepo(model.Repo{Name: "demo", Path: dir, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		root := New()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return output.String()
	}
	objective := "\n  Ship café ✅\n across\tregions!  "
	var task model.Task
	if err := json.Unmarshal([]byte(run("task", "create", "--repo", "demo", "--feature", "derived-direct", "--title", " ", objective, "--json")), &task); err != nil {
		t.Fatal(err)
	}
	if task.Title != "Ship café ✅ across regions!" {
		t.Fatalf("direct derived title = %q", task.Title)
	}
	var list model.Plan
	if err := json.Unmarshal([]byte(run("plan", "create", "derived-list", "--json")), &list); err != nil {
		t.Fatal(err)
	}
	var item model.PlanItem
	if err := json.Unmarshal([]byte(run("plan", "add", list.ID, "Investigate 🐑\n punctuation, safely.", "--json")), &item); err != nil {
		t.Fatal(err)
	}
	if item.Title != "Investigate 🐑 punctuation, safely." {
		t.Fatalf("item derived title = %q", item.Title)
	}
	humanItem := run("plan", "add", list.ID, "Human item title")
	if !strings.Contains(humanItem, "Human item title") {
		t.Fatalf("human item output omitted derived title: %s", humanItem)
	}
	human := run("task", "create", "--repo", "demo", "--feature", "derived-human", "Human output title")
	if !strings.Contains(human, "Human output title") {
		t.Fatalf("human creation output omitted derived title: %s", human)
	}
}

func TestTaskCreateRequiresDriverOutsidePi(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("SHEPHRD_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("PI_SESSION_ID", "")
	root := New()
	root.SetArgs([]string{"task", "create", "--title", "Test task", "--repo", "missing", "--feature", "work", "objective", "--json"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--driver-id is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestFreshTaskListJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("SHEPHRD_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
	root := New()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"task", "list", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != "[]" {
		t.Fatalf("output = %q", output.String())
	}
}
