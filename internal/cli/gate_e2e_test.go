package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"shephrd/internal/driverdelivery"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

type gateCLI struct {
	binary, database, data, temp string
	environment                  []string
}

func newGateCLI(t *testing.T) gateCLI {
	t.Helper()
	root := t.TempDir()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	cli := gateCLI{binary: filepath.Join(root, "shephrd"), database: filepath.Join(root, "state.db"), data: filepath.Join(root, "data"), temp: filepath.Join(root, "tmp")}
	build := exec.Command("go", "build", "-o", cli.binary, "./cmd/shephrd")
	build.Dir = filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shephrd: %s %v", output, err)
	}
	if err := os.Mkdir(cli.temp, 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("database_path = %q\ndata_dir = %q\nworktree_root = %q\n[memory]\nenabled = false\n", cli.database, cli.data, filepath.Join(root, "worktrees"))
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	replacements := map[string]string{"SHEPHRD_CONFIG": filepath.Join(root, "config.toml"), "TMPDIR": cli.temp}
	for _, name := range gateRefusedEnvironment {
		replacements[name] = ""
	}
	cli.environment = compoundCLIEnvironment(os.Environ(), replacements)
	return cli
}

func (cli gateCLI) run(t *testing.T, owner, request, stdin string) (string, map[string]string, int) {
	t.Helper()
	command := exec.Command(cli.binary, "gate", "--driver-id", owner)
	command.Env = append(slices.Clone(cli.environment), "SSH_ORIGINAL_COMMAND="+request)
	command.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	status := 0
	if exit, ok := err.(*exec.ExitError); ok {
		status = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var diagnostic map[string]string
	if stderr.Len() > 0 {
		if err := json.Unmarshal(stderr.Bytes(), &diagnostic); err != nil {
			t.Fatalf("stderr is not one JSON error: %q", stderr.String())
		}
	}
	return stdout.String(), diagnostic, status
}

func (cli gateCLI) audit(t *testing.T) []string {
	t.Helper()
	audit, err := os.ReadFile(filepath.Join(cli.data, "gate", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(audit)), "\n") {
		var record gateAuditRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%s|%s|%s|%d", record.DriverID, record.Command, record.Decision, record.ExitStatus))
	}
	return lines
}

func TestGateBuiltCLIHandoffReplyAckOverForcedCommand(t *testing.T) {
	cli := newGateCLI(t)
	binary, database, data, temp, environment := cli.binary, cli.database, cli.data, cli.temp, cli.environment
	const owner = "driver:hermes"
	gate := func(owner string, request []string, stdin string) (string, map[string]string, int) {
		t.Helper()
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		return cli.run(t, owner, string(body), stdin)
	}

	for refused, kind := range map[string]string{"driver:pi:session": "gate_owner_refused", "coordinator:coord_1": "gate_owner_refused"} {
		if stdout, diagnostic, status := gate(refused, []string{"repo", "list"}, ""); status != 1 || stdout != "" || diagnostic["error_kind"] != kind {
			t.Fatalf("%s: status=%d stdout=%q diagnostic=%v", refused, status, stdout, diagnostic)
		}
	}
	if stdout, diagnostic, status := gate(owner, []string{"wake", "drain", "--driver-id", owner}, ""); status != 1 || stdout != "" || diagnostic["error_kind"] != "gate_command_refused" {
		t.Fatalf("wake drain: status=%d stdout=%q diagnostic=%v", status, stdout, diagnostic)
	}

	original := "Build it; keep $(literal) `text` and \"quotes\"\n"
	stdout, diagnostic, status := gate(owner, []string{"shephrd", "subdriver", "handoff", "--general-context", "Remote fixture", "--key", "remote-1", "--request-file", "-"}, original)
	if status != 0 || diagnostic != nil {
		t.Fatalf("handoff: status=%d diagnostic=%v", status, diagnostic)
	}
	var handoff model.SubdriverRequest
	if err := json.Unmarshal([]byte(stdout), &handoff); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(temp); err != nil || len(entries) != 0 {
		t.Fatalf("request temp file remains: %v %v", entries, err)
	}

	state, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	stored, err := state.SubdriverRequest(handoff.ID)
	if err != nil || stored.Original != original || stored.OriginDriverID != owner {
		t.Fatalf("stored request %+v %v", stored, err)
	}
	if subdriver, err := state.Subdriver(handoff.SubdriverID); err != nil || subdriver.Generation != 0 || subdriver.RunnerPID != 0 {
		t.Fatalf("gate handoff started a session instead of queueing: %+v %v", subdriver, err)
	}

	fence, err := state.ReserveSubdriver(handoff.SubdriverID, 0, "pi", "fixture", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.StartSubdriver(fence, 0, "fixture"); err != nil {
		t.Fatal(err)
	}
	question, err := state.SubdriverReturn(fence, handoff.ID, "question", "question", "Which base?")
	if err != nil {
		t.Fatal(err)
	}
	drain, err := state.DrainNotifications("", owner, "watch:fixture", 1, time.Minute)
	if err != nil || len(drain.Notifications) != 1 || drain.Notifications[0].SubdriverEventID != question.ID {
		t.Fatalf("drain = %+v %v", drain, err)
	}
	notification := drain.Notifications[0]
	commands := driverdelivery.NewRequest(notification, "watch:fixture", 1, nil).Commands

	direct := exec.Command(binary, append(commands.Request[1:], "--json")...)
	direct.Env = environment
	directOutput, err := direct.Output()
	if err != nil {
		t.Fatal(err)
	}
	if stdout, diagnostic, status := gate(owner, commands.Request, ""); status != 0 || diagnostic != nil || stdout != string(directOutput) {
		t.Fatalf("request read: status=%d diagnostic=%v stdout=%q direct=%q", status, diagnostic, stdout, directOutput)
	}

	answer := "Use main; don't run `rm -rf ~` or $(id) | cat && echo 'quoted' \"double\"\nsecond line"
	reply := slices.Clone(commands.Reply)
	reply[slices.Index(reply, "<text>")] = answer
	reply[slices.Index(reply, "<key>")] = "answer-1"
	stdout, diagnostic, status = gate(owner, reply, "ignored stdin")
	if status != 0 || diagnostic != nil {
		t.Fatalf("reply: status=%d diagnostic=%v", status, diagnostic)
	}
	var replyEvent model.SubdriverEvent
	if err := json.Unmarshal([]byte(stdout), &replyEvent); err != nil {
		t.Fatal(err)
	}
	if event, err := state.SubdriverEvent(replyEvent.ID); err != nil || event.Payload != answer || event.ReplyTo != question.ID || event.Kind != "reply" {
		t.Fatalf("stored reply %+v %v", event, err)
	}

	mismatched := slices.Clone(commands.Ack)
	mismatched[slices.Index(mismatched, owner)] = "driver:other"
	if stdout, diagnostic, status := gate(owner, mismatched, ""); status != 1 || stdout != "" || diagnostic["error_kind"] != "gate_owner_mismatch" {
		t.Fatalf("mismatched ack: status=%d stdout=%q diagnostic=%v", status, stdout, diagnostic)
	}
	if stdout, diagnostic, status := gate(owner, []string{"task", "inspect", "task_missing"}, ""); status != 1 || stdout != "" || diagnostic["error"] != `task "task_missing" does not exist` || diagnostic["error_kind"] != "not_found" {
		t.Fatalf("failing allowlisted command: status=%d stdout=%q diagnostic=%v", status, stdout, diagnostic)
	}
	stdout, diagnostic, status = gate(owner, commands.Ack, "")
	if status != 0 || diagnostic != nil {
		t.Fatalf("ack: status=%d diagnostic=%v", status, diagnostic)
	}
	var receipt struct {
		SchemaVersion  int    `json:"schema_version"`
		NotificationID string `json:"notification_id"`
	}
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil || receipt.SchemaVersion != 1 || receipt.NotificationID != notification.NotificationID {
		t.Fatalf("ack receipt %q %v", stdout, err)
	}
	if acknowledged, err := state.Notification(notification.NotificationID); err != nil || acknowledged.State != model.NotificationAcknowledged {
		t.Fatalf("notification = %+v %v", acknowledged, err)
	}

	audit, err := os.ReadFile(filepath.Join(data, "gate", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{notification.ClaimToken, "rm -rf", "Build it", "Which base"} {
		if strings.Contains(string(audit), secret) {
			t.Fatalf("audit leaked %q: %s", secret, audit)
		}
	}
	lines := cli.audit(t)
	want := []string{
		"driver:pi:session||refused|1", "coordinator:coord_1||refused|1",
		owner + "|wake drain|refused|1", owner + "|subdriver handoff|allowed|0", owner + "|subdriver request|allowed|0", owner + "|subdriver reply|allowed|0",
		owner + "|wake ack|refused|1", owner + "|task inspect|allowed|1", owner + "|wake ack|allowed|0",
	}
	slices.Sort(lines[:2])
	slices.Sort(want[:2])
	if !slices.Equal(lines, want) {
		t.Fatalf("audit lines:\n%s", strings.Join(lines, "\n"))
	}
}

func TestGateBuiltCLIMissingIDsReturnTypedErrors(t *testing.T) {
	cli := newGateCLI(t)
	const owner = "driver:hermes"
	for _, test := range []struct {
		name, kind, message string
		args                []string
	}{
		{"request", "not_found", `sub-driver request "request_missing" does not exist`, []string{"subdriver", "request", "request_missing"}},
		{"event", "not_found", `sub-driver event 42 does not exist`, []string{"subdriver", "event", "42"}},
		{"inspect", "not_found", `sub-driver "coord_missing" does not exist`, []string{"subdriver", "inspect", "coord_missing"}},
		{"reply", "not_found", `sub-driver request "request_missing" does not exist`, []string{"subdriver", "reply", "request_missing", "answer", "--key", "answer", "--reply-to", "42", "--driver-id", owner}},
		{"task inspect", "not_found", `task "task_missing" does not exist`, []string{"task", "inspect", "task_missing"}},
		{"wake ack", "claim_conflict", "claim token does not identify a stored notification claim", []string{"wake", "ack", "--claim-token", "missing", "--driver-id", owner}},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := func(source, stdout string, diagnostic map[string]string, status int) {
				t.Helper()
				if status != 1 || stdout != "" || diagnostic["error_kind"] != test.kind || !strings.Contains(diagnostic["error"], test.message) || strings.Contains(diagnostic["error"], "sql: no rows") {
					t.Fatalf("%s: status=%d stdout=%q diagnostic=%v", source, status, stdout, diagnostic)
				}
			}
			cmd := exec.Command(cli.binary, append(slices.Clone(test.args), "--json")...)
			cmd.Env = cli.environment
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			var diagnostic map[string]string
			if decodeErr := json.Unmarshal(stderr.Bytes(), &diagnostic); decodeErr != nil {
				t.Fatalf("direct: %q %v (%v)", stderr.String(), decodeErr, err)
			}
			status := 0
			if exit, ok := err.(*exec.ExitError); ok {
				status = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			check("direct", stdout.String(), diagnostic, status)
			request, err := json.Marshal(test.args)
			if err != nil {
				t.Fatal(err)
			}
			body, diagnostic, status := cli.run(t, owner, string(request), "")
			check("gate", body, diagnostic, status)
		})
	}
}

func TestGateBuiltCLIRefusesNullHelpBypassAndHiddenOwnerWithoutExecuting(t *testing.T) {
	cli := newGateCLI(t)
	const owner = "driver:hermes"
	for _, test := range []struct {
		request, kind, command string
	}{
		{`["subdriver","handoff","--general-context","fixture","--key","null-element","--context",null,"Original"]`, "gate_request_invalid", ""},
		{`["subdriver","handoff","--general-context","fixture","--key","null-original",null]`, "gate_request_invalid", ""},
		{`[null,"subdriver","handoff","--general-context","fixture","--key","null-first","Original"]`, "gate_request_invalid", ""},
		{`["task","obligations","--help","--driver-id","driver:other"]`, "gate_owner_mismatch", "task obligations"},
		{`["wake","ack","--help","--driver-generation","other"]`, "gate_flag_refused", "wake ack"},
		{`["subdriver","ls","-h","--all-drivers"]`, "gate_flag_refused", "subdriver ls"},
		{`["task","obligations","--driver-id","driver:other","--driver-id","driver:hermes"]`, "gate_owner_mismatch", "task obligations"},
		{`["task","obligations","--driver-id=driver:other","--driver-id=driver:hermes"]`, "gate_owner_mismatch", "task obligations"},
		{`["subdriver","handoff","--general-context","fixture","--key","hidden-owner","--driver-id","driver:other","--driver-id","driver:hermes","Original"]`, "gate_owner_mismatch", "subdriver handoff"},
		{`["worker","send","task_1","follow-up"]`, "gate_command_refused", "worker send"},
		{`["worker","send","--help"]`, "gate_command_refused", "worker send"},
	} {
		stdout, diagnostic, status := cli.run(t, owner, test.request, "")
		if status != 1 || stdout != "" || diagnostic["error_kind"] != test.kind {
			t.Fatalf("%s: status=%d stdout=%q diagnostic=%v", test.request, status, stdout, diagnostic)
		}
	}
	state, err := store.Open(cli.database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	if subdrivers, err := state.Subdrivers("", 0); err != nil || len(subdrivers) != 0 {
		t.Fatalf("refused requests created sub-drivers: %v %v", subdrivers, err)
	}
	want := []string{
		owner + "||refused|1", owner + "||refused|1", owner + "||refused|1",
		owner + "|task obligations|refused|1", owner + "|wake ack|refused|1", owner + "|subdriver ls|refused|1",
		owner + "|task obligations|refused|1", owner + "|task obligations|refused|1", owner + "|subdriver handoff|refused|1",
		owner + "|worker send|refused|1", owner + "|worker send|refused|1",
	}
	if lines := cli.audit(t); !slices.Equal(lines, want) {
		t.Fatalf("audit lines:\n%s", strings.Join(lines, "\n"))
	}

	stdout, diagnostic, status := cli.run(t, owner, `["task","obligations","--driver-id","driver:hermes","--driver-id=driver:hermes","--help"]`, "")
	if status != 0 || diagnostic != nil || !strings.Contains(stdout, `"command":"shephrd task obligations"`) {
		t.Fatalf("owner help: status=%d stdout=%q diagnostic=%v", status, stdout, diagnostic)
	}
	text := "--driver-id=driver:other"
	request, err := state.HandoffSubdriver("", "Gate fixture", owner, "literal", "Original", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fence, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "fixture", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.StartSubdriver(fence, 0, "fixture"); err != nil {
		t.Fatal(err)
	}
	question, err := state.SubdriverReturn(fence, request.ID, "question", "question", "Question?")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal([]string{"subdriver", "reply", request.ID, "--reply-to", fmt.Sprint(question.ID), "--key", "literal", "--driver-id", owner, "--", text})
	if err != nil {
		t.Fatal(err)
	}
	stdout, diagnostic, status = cli.run(t, owner, string(body), "")
	if status != 0 || diagnostic != nil {
		t.Fatalf("literal reply: status=%d diagnostic=%v", status, diagnostic)
	}
	var reply model.SubdriverEvent
	if err := json.Unmarshal([]byte(stdout), &reply); err != nil {
		t.Fatal(err)
	}
	if stored, err := state.SubdriverEvent(reply.ID); err != nil || stored.Payload != text || stored.ReplyTo != question.ID {
		t.Fatalf("literal reply stored %+v %v", stored, err)
	}
}
