package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

const gateTestOwner = "driver:hermes"

func gateRequest(t *testing.T, argv ...string) string {
	t.Helper()
	body, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestGatePlanForcesJSONOwnerAndQueue(t *testing.T) {
	for _, test := range []struct {
		request    []string
		want       []string
		stdinIndex int
	}{
		{[]string{"repo", "list"}, []string{"repo", "list", "--json=true"}, 0},
		{[]string{"shephrd", "repo", "context", "/tmp/project"}, []string{"repo", "context", "--json=true", "--", "/tmp/project"}, 0},
		{[]string{"subdriver", "handoff", "--general-context", "ctx", "--key", "k", "--request-file", "-"},
			[]string{"subdriver", "handoff", "--driver-id=" + gateTestOwner, "--general-context=ctx", "--json=true", "--key=k", "--queue=true", "--request-file=-"}, 7},
		{[]string{"subdriver", "handoff", "--repo", "demo", "--key", "k", "--queue=false", "--driver-id", gateTestOwner, "original request"},
			[]string{"subdriver", "handoff", "--driver-id=" + gateTestOwner, "--json=true", "--key=k", "--queue=true", "--repo=demo", "--", "original request"}, 0},
		{[]string{"shephrd", "subdriver", "reply", "request_1", "<text>", "--reply-to", "42", "--key", "<key>", "--driver-id", gateTestOwner, "--json"},
			[]string{"subdriver", "reply", "--driver-id=" + gateTestOwner, "--json=true", "--key=<key>", "--reply-to=42", "--", "request_1", "<text>"}, 0},
		{[]string{"subdriver", "inspect", "coord_1", "--offset", "20"}, []string{"subdriver", "inspect", "--json=true", "--offset=20", "--", "coord_1"}, 0},
		{[]string{"shephrd", "subdriver", "request", "request_1", "--json"}, []string{"subdriver", "request", "--json=true", "--", "request_1"}, 0},
		{[]string{"shephrd", "subdriver", "event", "42", "--json"}, []string{"subdriver", "event", "--json=true", "--", "42"}, 0},
		{[]string{"subdriver", "ls"}, []string{"subdriver", "ls", "--driver-id=" + gateTestOwner, "--json=true"}, 0},
		{[]string{"shephrd", "task", "inspect", "task_1", "--json"}, []string{"task", "inspect", "--json=true", "--", "task_1"}, 0},
		{[]string{"task", "obligations", "--details"}, []string{"task", "obligations", "--details=true", "--driver-id=" + gateTestOwner, "--json=true"}, 0},
		{[]string{"shephrd", "wake", "ack", "--claim-token", "token", "--driver-id", gateTestOwner, "--json"},
			[]string{"wake", "ack", "--claim-token=token", "--driver-id=" + gateTestOwner, "--json=true"}, 0},
		{[]string{"wake", "ack", "--help", "--driver-id", gateTestOwner}, []string{"wake", "ack", "--help", "--json"}, 0},
		{[]string{"task", "obligations", "--driver-id", gateTestOwner, "--driver-id=" + gateTestOwner}, []string{"task", "obligations", "--driver-id=" + gateTestOwner, "--json=true"}, 0},
		{[]string{"subdriver", "reply", "request_1", "--reply-to", "1", "--key", "k", "--", "--driver-id=driver:other"},
			[]string{"subdriver", "reply", "--driver-id=" + gateTestOwner, "--json=true", "--key=k", "--reply-to=1", "--", "request_1", "--driver-id=driver:other"}, 0},
		{[]string{"shephrd", "subdriver", "handoff", "-h"}, []string{"subdriver", "handoff", "--help", "--json"}, 0},
	} {
		plan, err := planGate(gateTestOwner, gateRequest(t, test.request...))
		if err != nil {
			t.Fatalf("%v: %v", test.request, err)
		}
		if !slices.Contains(gateCommands, plan.path) || !slices.Equal(plan.argv, test.want) || plan.stdinIndex != test.stdinIndex {
			t.Fatalf("%v planned %+v, want %v stdin=%d", test.request, plan, test.want, test.stdinIndex)
		}
	}
}

func TestGatePlanRefusesOutsideTheAllowlist(t *testing.T) {
	for _, test := range []struct {
		request string
		kind    string
	}{
		{`["repo","add","/tmp/project","--setup-hook","touch pwned"]`, "gate_command_refused"},
		{`["repo","scan"]`, "gate_command_refused"},
		{`["repo"]`, "gate_command_refused"},
		{`["wake","drain","--driver-id","driver:hermes"]`, "gate_command_refused"},
		{`["wake","pump","--driver-id","driver:hermes"]`, "gate_command_refused"},
		{`["wake","renew","--claim-token","token"]`, "gate_command_refused"},
		{`["wake","watch","--driver-id","driver:hermes"]`, "gate_command_refused"},
		{`["task","adopt","task_1","--new-driver-id","driver:hermes"]`, "gate_command_refused"},
		{`["subdriver","adopt-request","request_1","--from-driver","driver:old"]`, "gate_command_refused"},
		{`["subdriver","recover","coord_1","--generation","1"]`, "gate_command_refused"},
		{`["subdriver","resume","coord_1"]`, "gate_command_refused"},
		{`["subdriver","dispatch","request_1","objective"]`, "gate_command_refused"},
		{`["subdriver","return","request_1","text"]`, "gate_command_refused"},
		{`["subdriver","_run","coord_1"]`, "gate_command_refused"},
		{`["coordinator","handoff","--repo","demo","--key","k","text"]`, "gate_command_refused"},
		{`["task","create","objective","--driver-id","driver:hermes"]`, "gate_command_refused"},
		{`["task","verify-delivery","task_1"]`, "gate_command_refused"},
		{`["task","attest-delivery","task_1"]`, "gate_command_refused"},
		{`["task","archive","task_1"]`, "gate_command_refused"},
		{`["task","annotate","task_1","judgment"]`, "gate_command_refused"},
		{`["worker","send","task_1","text"]`, "gate_command_refused"},
		{`["worker","send","--help"]`, "gate_command_refused"},
		{`["worker","spawn","task_1"]`, "gate_command_refused"},
		{`["worker","stop","task_1","--discard"]`, "gate_command_refused"},
		{`["worker","retry","task_1"]`, "gate_command_refused"},
		{`["workspace","release","task_1","--discard"]`, "gate_command_refused"},
		{`["workspace","reconcile"]`, "gate_command_refused"},
		{`["plan","create","--driver-id","driver:hermes"]`, "gate_command_refused"},
		{`["plan","dispatch","item_1"]`, "gate_command_refused"},
		{`["protocol","validate","--role","worker"]`, "gate_command_refused"},
		{`["completion","bash"]`, "gate_command_refused"},
		{`["gate","--driver-id","driver:other"]`, "gate_command_refused"},
		{`["help"]`, "gate_command_refused"},
		{`["--help"]`, "gate_command_refused"},
		{`["--json","repo","list"]`, "gate_command_refused"},
		{`["bash","-c","id"]`, "gate_command_refused"},
		{`["wake","ack","--claim-token","token","--driver-generation","generation"]`, "gate_flag_refused"},
		{`["task","obligations","--all-drivers"]`, "gate_flag_refused"},
		{`["subdriver","ls","--all-drivers"]`, "gate_flag_refused"},
		{`["repo","list","--legacy-coordinator-json"]`, "gate_flag_refused"},
		{`["wake","ack","--help","--driver-generation","other"]`, "gate_flag_refused"},
		{`["task","obligations","-h","--all-drivers"]`, "gate_flag_refused"},
		{`["task","obligations","--driver-id","driver:other"]`, "gate_owner_mismatch"},
		{`["task","obligations","--help","--driver-id","driver:other"]`, "gate_owner_mismatch"},
		{`["task","obligations","--driver-id","driver:other","--driver-id","driver:hermes"]`, "gate_owner_mismatch"},
		{`["task","obligations","--driver-id=driver:other","--driver-id=driver:hermes"]`, "gate_owner_mismatch"},
		{`["task","obligations","--driver-id","driver:hermes","--driver-id","driver:other"]`, "gate_owner_mismatch"},
		{`["subdriver","ls","--driver-id=","--driver-id","driver:hermes"]`, "gate_owner_mismatch"},
		{`["wake","ack","--claim-token","token","--driver-id="]`, "gate_owner_mismatch"},
		{`["subdriver","reply","request_1","text","--reply-to","1","--key","k","--driver-id","driver:pi:session"]`, "gate_owner_mismatch"},
		{`["repo","list","--config","/tmp/other.toml"]`, "gate_request_invalid"},
		{`["repo","list","--database","/tmp/other.db"]`, "gate_request_invalid"},
		{`["task","inspect"]`, "gate_request_invalid"},
		{`repo list`, "gate_request_invalid"},
		{`["repo","list"`, "gate_request_invalid"},
		{`["repo","list"] ["wake","drain"]`, "gate_request_invalid"},
		{`{"argv":["repo","list"]}`, "gate_request_invalid"},
		{`["repo",1]`, "gate_request_invalid"},
		{`["repo",["list"]]`, "gate_request_invalid"},
		{`null`, "gate_request_invalid"},
		{`[null]`, "gate_request_invalid"},
		{`[null,"repo","list"]`, "gate_request_invalid"},
		{`["shephrd",null]`, "gate_request_invalid"},
		{`["repo","list",null]`, "gate_request_invalid"},
		{`["subdriver","handoff","--general-context","fixture","--key","null-element","--context",null,"Original"]`, "gate_request_invalid"},
		{`[]`, "gate_request_invalid"},
		{`["shephrd"]`, "gate_request_invalid"},
		{``, "gate_request_invalid"},
		{`["task","inspect","task_1\u0000"]`, "gate_request_invalid"},
		{`["repo","context","` + strings.Repeat("a", gateMaxInputBytes) + `"]`, "gate_request_invalid"},
	} {
		plan, err := planGate(gateTestOwner, test.request)
		if ErrorKind(err) != test.kind || plan.argv != nil {
			t.Fatalf("%.80s: plan=%+v err=%v, want %s", test.request, plan, err, test.kind)
		}
	}
}

type gateFixture struct {
	root  string
	data  string
	state *store.Store
}

func newGateFixture(t *testing.T) gateFixture {
	t.Helper()
	root := t.TempDir()
	fixture := gateFixture{root: root, data: filepath.Join(root, "data")}
	database := filepath.Join(root, "state.db")
	config := fmt.Sprintf("database_path = %q\ndata_dir = %q\nworktree_root = %q\n[memory]\nenabled = false\n", database, fixture.data, filepath.Join(root, "worktrees"))
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("TMPDIR", filepath.Join(root, "tmp"))
	if err := os.Mkdir(filepath.Join(root, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range gateRefusedEnvironment {
		t.Setenv(name, "")
	}
	state, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	fixture.state = state
	return fixture
}

func (f gateFixture) run(t *testing.T, owner, request string, stdin io.Reader) (string, map[string]string, int) {
	t.Helper()
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	var stdout, stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetIn(stdin)
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	status := runGate(command, owner, request)
	var diagnostic map[string]string
	if stderr.Len() > 0 {
		if err := json.Unmarshal(stderr.Bytes(), &diagnostic); err != nil {
			t.Fatalf("stderr is not one JSON error: %q", stderr.String())
		}
	}
	return stdout.String(), diagnostic, status
}

func (f gateFixture) audit(t *testing.T) []gateAuditRecord {
	t.Helper()
	path := filepath.Join(f.data, "gate", "audit.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit mode = %v", info.Mode().Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []gateAuditRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record gateAuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("audit line %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	return records
}

func (f gateFixture) tempEntries(t *testing.T) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.root, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Error("gate read stdin without --request-file -")
	return 0, io.EOF
}

func TestGateRefusesIneligibleOwnersAndIdentityEnvironmentAtStartup(t *testing.T) {
	fixture := newGateFixture(t)
	request := gateRequest(t, "repo", "list")
	for owner, kind := range map[string]string{"": "driver_context_required", "driver:pi:session": "gate_owner_refused", "coordinator:coord_1": "gate_owner_refused", "subdriver:coord_1": "gate_owner_refused"} {
		stdout, diagnostic, status := fixture.run(t, owner, request, failingReader{t})
		if status != 1 || stdout != "" || diagnostic["error_kind"] != kind {
			t.Fatalf("owner %q: status=%d stdout=%q diagnostic=%v", owner, status, stdout, diagnostic)
		}
	}
	for _, name := range gateRefusedEnvironment {
		t.Setenv(name, "1")
		stdout, diagnostic, status := fixture.run(t, gateTestOwner, request, failingReader{t})
		if status != 1 || stdout != "" || diagnostic["error_kind"] != "gate_environment_refused" {
			t.Fatalf("%s: status=%d stdout=%q diagnostic=%v", name, status, stdout, diagnostic)
		}
		t.Setenv(name, "")
	}
	for _, record := range fixture.audit(t) {
		if record.Decision != "refused" || record.ExitStatus != 1 || record.Command != "" {
			t.Fatalf("startup refusal audit = %+v", record)
		}
	}
}

func TestGateRefusedRequestsDoNotExecute(t *testing.T) {
	fixture := newGateFixture(t)
	marker := filepath.Join(fixture.root, "pwned")
	for _, request := range []string{
		gateRequest(t, "repo", "add", fixture.root, "--setup-hook", "touch "+marker),
		gateRequest(t, "subdriver", "handoff", "--general-context", "ctx", "--key", "k", "--driver-id", "driver:other", "text"),
		gateRequest(t, "task", "obligations", "--all-drivers"),
		gateRequest(t, "wake", "drain", "--driver-id", gateTestOwner),
		`["repo","list"] && touch ` + marker,
	} {
		stdout, diagnostic, status := fixture.run(t, gateTestOwner, request, failingReader{t})
		if status != 1 || stdout != "" || !strings.HasPrefix(diagnostic["error_kind"], "gate_") {
			t.Fatalf("%s: status=%d stdout=%q diagnostic=%v", request, status, stdout, diagnostic)
		}
	}
	if repos, err := fixture.state.Repos(); err != nil || len(repos) != 0 {
		t.Fatalf("refused repo add registered %v %v", repos, err)
	}
	if subdrivers, err := fixture.state.Subdrivers("", 0); err != nil || len(subdrivers) != 0 {
		t.Fatalf("refused handoff created %v %v", subdrivers, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("refused request ran a shell: %v", err)
	}
	records := fixture.audit(t)
	want := []string{"repo add", "subdriver handoff", "task obligations", "wake drain", ""}
	if len(records) != len(want) {
		t.Fatalf("audit = %+v", records)
	}
	for index, record := range records {
		if record.DriverID != gateTestOwner || record.Command != want[index] || record.Decision != "refused" || record.ExitStatus != 1 || !strings.HasPrefix(record.ErrorKind, "gate_") {
			t.Fatalf("audit %d = %+v", index, record)
		}
	}
}

func TestGateRepliesWithHostileTextArriveLiterally(t *testing.T) {
	fixture := newGateFixture(t)
	request, err := fixture.state.HandoffSubdriver("", "Gate fixture", gateTestOwner, "hostile", "Original", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.state.ReserveSubdriver(request.SubdriverID, 0, "pi", "fixture", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.state.StartSubdriver(fence, 0, "fixture"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture.root, "pwned")
	for index, text := range []string{
		"$(touch " + marker + "); `touch " + marker + "` | tee " + marker + " && echo \"double\" 'single' \\ > " + marker,
		"line one\nline two\r\n\ttabbed; rm -rf ~ * ?",
		"--json --driver-id driver:other '\"' \\\"",
		"-",
	} {
		question, err := fixture.state.SubdriverReturn(fence, request.ID, fmt.Sprintf("question-%d", index), "question", "Question?")
		if err != nil {
			t.Fatal(err)
		}
		stdout, diagnostic, status := fixture.run(t, gateTestOwner, gateRequest(t, "shephrd", "subdriver", "reply", request.ID, "--reply-to", fmt.Sprint(question.ID), "--key", fmt.Sprintf("reply-%d", index), "--", text), failingReader{t})
		if status != 0 || diagnostic != nil {
			t.Fatalf("reply %d: status=%d diagnostic=%v", index, status, diagnostic)
		}
		var reply model.SubdriverEvent
		if err := json.Unmarshal([]byte(stdout), &reply); err != nil {
			t.Fatal(err)
		}
		stored, err := fixture.state.SubdriverEvent(reply.ID)
		if err != nil || stored.Payload != text || stored.Kind != "reply" || stored.ReplyTo != question.ID {
			t.Fatalf("reply %d stored %+v %v, want %q", index, stored, err, text)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("reply text was evaluated: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(fixture.data, "gate", "audit.jsonl"))
	if err != nil || strings.Contains(string(body), "touch") || strings.Contains(string(body), "line one") || strings.Contains(string(body), request.ID) {
		t.Fatalf("audit leaked request content: %s %v", body, err)
	}
}

func TestGateRequestFileStdinUsesPrivateTemporaryFile(t *testing.T) {
	fixture := newGateFixture(t)
	original := "Original request with $(id) and `id`\nsecond line 'quoted' \"double\"\x01\n"
	stdout, diagnostic, status := fixture.run(t, gateTestOwner, gateRequest(t, "subdriver", "handoff", "--general-context", "Remote work", "--key", "stdin", "--request-file", "-"), strings.NewReader(original))
	if status != 0 || diagnostic != nil {
		t.Fatalf("handoff: status=%d diagnostic=%v", status, diagnostic)
	}
	var handoff model.SubdriverRequest
	if err := json.Unmarshal([]byte(stdout), &handoff); err != nil {
		t.Fatal(err)
	}
	stored, err := fixture.state.SubdriverRequest(handoff.ID)
	if err != nil || stored.Original != original || stored.OriginDriverID != gateTestOwner {
		t.Fatalf("stored request %+v %v", stored, err)
	}
	subdriver, err := fixture.state.Subdriver(handoff.SubdriverID)
	if err != nil || subdriver.Generation != 0 || subdriver.RunnerPID != 0 {
		t.Fatalf("handoff was not queued: %+v %v", subdriver, err)
	}
	if entries := fixture.tempEntries(t); len(entries) != 0 {
		t.Fatalf("temporary request file was not removed: %v", entries)
	}

	oversized := strings.NewReader(strings.Repeat("a", gateMaxInputBytes+1))
	stdout, diagnostic, status = fixture.run(t, gateTestOwner, gateRequest(t, "subdriver", "handoff", "--general-context", "Remote work", "--key", "oversized", "--request-file", "-"), oversized)
	if status != 1 || stdout != "" || diagnostic["error_kind"] != "gate_request_invalid" {
		t.Fatalf("oversized stdin: status=%d stdout=%q diagnostic=%v", status, stdout, diagnostic)
	}
	if entries := fixture.tempEntries(t); len(entries) != 0 {
		t.Fatalf("oversized request file was not removed: %v", entries)
	}

	path, err := gateRequestFile(strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("request file mode = %v %v", info, err)
	}

	stdout, diagnostic, status = fixture.run(t, gateTestOwner, gateRequest(t, "repo", "list"), failingReader{t})
	if status != 0 || diagnostic != nil || strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("repo list: status=%d stdout=%q diagnostic=%v", status, stdout, diagnostic)
	}
	records := fixture.audit(t)
	if len(records) != 3 || records[0] != (gateAuditRecord{Time: records[0].Time, DriverID: gateTestOwner, Command: "subdriver handoff", Decision: "allowed"}) ||
		records[1].Decision != "refused" || records[1].ErrorKind != "gate_request_invalid" || records[2].Command != "repo list" || records[2].Decision != "allowed" {
		t.Fatalf("audit = %+v", records)
	}
}
