package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"shephrd/internal/coord"
	"shephrd/internal/store"
	"time"
)

func TestMainCoordinatorSerializesSurfaceTurnsAndFencesManualOwner(t *testing.T) {
	env := initialized(t)
	env.InstallHarnesses()
	env.Script(map[string][][]map[string]any{"__main__": {{{"sleep": "1s"}}, {}}})
	first := env.OK("surface", "submit", "--thread", "room", "one", "--as", "surface:larry")
	second := env.OK("surface", "submit", "--thread", "room", "two", "--as", "surface:phone")
	if env.OK("main", "status")["state"] != "disabled" {
		t.Fatal("coordinator started implicitly")
	}
	started := env.OK("main", "start")
	if started["session"] == "" || env.OK("main", "start")["session"] != started["session"] {
		t.Fatal("session identity changed")
	}
	env.Refused("coordinator_owned", "inbox")
	env.Refused("coordinator_owned", "inbox", "--as", "driver:main")
	env.Refused("coordinator_owned", "task", "list")
	env.StartDaemon()
	env.Eventually("first turn running", func() bool { return env.OK("main", "status")["state"] == "running" })
	if got := env.OK("surface", "replies", "--thread", "room", "--as", "surface:phone")["replies"].([]any); len(got) != 0 {
		t.Fatalf("second turn ran concurrently: %v", got)
	}
	env.Eventually("both replies", func() bool {
		if s := env.OK("main", "status"); s["state"] == "held" {
			log, _ := os.ReadFile(filepath.Join(env.Home, ".local/share/shephrd/main/1/session.log"))
			t.Fatalf("held: %v; daemon: %s; log: %s", s, env.DaemonLog(), log)
		}
		return len(env.OK("surface", "replies", "--thread", "room", "--as", "surface:phone")["replies"].([]any)) == 1
	})
	larry := env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any)
	phone := env.OK("surface", "replies", "--thread", "room", "--as", "surface:phone")["replies"].([]any)
	if larry[0].(map[string]any)["input"] != first["id"] || phone[0].(map[string]any)["input"] != second["id"] {
		t.Fatalf("routing: %v %v", larry, phone)
	}
	var calls []map[string]any
	for _, call := range env.Calls() {
		if call["task"] == "__main__" && call["harness"] == "claude" {
			calls = append(calls, call)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("turns: %v", calls)
	}
	args0 := strings.Join(toStrings(calls[0]["args"]), " ")
	args1 := strings.Join(toStrings(calls[1]["args"]), " ")
	if !strings.Contains(args0, "--session-id "+started["session"].(string)) || !strings.Contains(args1, "--resume "+started["session"].(string)) || calls[0]["cwd"] != filepath.Join(env.Home, ".local/share/shephrd/main") {
		t.Fatalf("native session: %s / %s / %v", args0, args1, calls[0]["cwd"])
	}
}

func TestMainCoordinatorHoldsFailedTurnAndRequiresResolvedOutput(t *testing.T) {
	env := initialized(t)
	env.InstallHarnesses()
	env.Script(map[string][][]map[string]any{"__main__": {{{"exit": 1}}, {}}})
	env.OK("surface", "submit", "--thread", "room", "one", "--as", "surface:larry")
	env.OK("surface", "submit", "--thread", "room", "two", "--as", "surface:larry")
	env.OK("main", "start")
	stop := env.StartDaemon()
	env.Eventually("held failure", func() bool { return env.OK("main", "status")["state"] == "held" })
	stop()
	env.Refused("input_pending", "main", "recover", "--evidence", "inspected log")
	if len(env.OK("main", "pending")["inputs"].([]any)) != 2 {
		t.Fatal("lost input")
	}
	env.OK("main", "reply", "1", "operator inspected and answered")
	env.OK("main", "recover", "--evidence", "confirmed failed process exited and resolved input")
	env.StartDaemon()
	env.Eventually("next turn answered", func() bool {
		return len(env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any)) == 2
	})
	var calls []map[string]any
	for _, c := range env.Calls() {
		if c["task"] == "__main__" && c["harness"] == "claude" {
			calls = append(calls, c)
		}
	}
	if len(calls) != 2 || !strings.Contains(strings.Join(toStrings(calls[1]["args"]), " "), "--session-id") {
		t.Fatalf("failed first session was resumed without a valid result: %v", calls)
	}
}

func TestMainCoordinatorConsumesInboxAsSoleOwner(t *testing.T) {
	env, _ := withHarness(t)
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Report to main")
	env.OK("task", "start", "t_1")
	env.Eventually("pending inbox item", func() bool { return len(env.OK("inbox")["items"].([]any)) == 1 })
	env.OK("main", "start")
	env.StartDaemon()
	env.Eventually("inbox handled", func() bool {
		status := env.OK("main", "status")
		return status["state"] == "idle" && status["turn"] == float64(1)
	})
	var acked bool
	for _, call := range env.Calls() {
		if call["task"] == "__main__" && call["command"] != nil && strings.Contains(strings.Join(toStrings(call["command"]), " "), "inbox ack 1") && call["error"] == "<nil>" {
			acked = true
		}
	}
	if !acked {
		t.Fatalf("sole session did not ack: %v", env.Calls())
	}
	if s := env.OK("main", "status"); !strings.Contains(s["result"].(string), "fake reply") {
		t.Fatalf("inbox output not retained locally: %v", s)
	}
	if got := env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any); len(got) != 0 {
		t.Fatalf("default leaked unsolicited output: %v", got)
	}
}

func TestMainReturnRequiresExplicitHomeScope(t *testing.T) {
	env := initialized(t)
	env.AppendConfig("[main_return]\nsurface = \"larry\"")
	env.Refused("invalid_config", "main", "start")
}

func TestMainInboxReturnRoutePinnedAndSurfaceCursor(t *testing.T) {
	env, _ := withHarness(t)
	env.AppendConfig("[main_return]\nsurface = \"larry\"\nthread = \"room\"")
	env.Script(map[string][][]map[string]any{"__main__": {{{"sleep": "700ms"}, {"shephrd": []string{"inbox", "ack", "1"}}}, {}}})
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Report to main")
	env.OK("task", "start", "t_1")
	env.Eventually("inbox queued", func() bool { return len(env.OK("inbox")["items"].([]any)) == 1 })
	env.OK("main", "start")
	stop := env.StartDaemon()
	env.Eventually("inbox running", func() bool { return env.OK("main", "status")["state"] == "running" })
	config, err := os.ReadFile(env.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.ConfigPath(), []byte(strings.Replace(string(config), "surface = \"larry\"", "surface = \"phone\"", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	env.Eventually("inbox result routed", func() bool {
		return len(env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any)) == 1
	})
	stop()
	larry := env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any)
	out := larry[0].(map[string]any)
	if out["inbox_item"] != float64(1) || out["turn"] != float64(1) || out["body"] != "fake reply 0" || out["input"] != nil {
		t.Fatalf("routed source: %v", out)
	}
	if other := env.OK("surface", "replies", "--thread", "room", "--as", "surface:phone")["replies"].([]any); len(other) != 0 {
		t.Fatalf("changed route leaked output: %v", other)
	}
	env.Refused("invalid_cursor", "surface", "ack", "--thread", "room", "1", "--as", "surface:phone")
	env.Refused("invalid_cursor", "surface", "ack", "--thread", "other", "1", "--as", "surface:larry")
	env.StartDaemon()
	time.Sleep(350 * time.Millisecond)
	if got := env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any); len(got) != 1 {
		t.Fatalf("restart lost or duplicated output: %v", got)
	}
	env.OK("surface", "ack", "--thread", "room", "1", "--as", "surface:larry")
	env.OK("surface", "ack", "--thread", "room", "1", "--as", "surface:larry")
	if got := env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any); len(got) != 0 {
		t.Fatalf("ack did not prune: %v", got)
	}
	env.OK("surface", "submit", "--thread", "room", "follow-up", "--as", "surface:larry")
	env.Eventually("correlated output", func() bool {
		return len(env.OK("surface", "replies", "--thread", "room", "--after", "1", "--as", "surface:larry")["replies"].([]any)) == 1
	})
	later := env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any)[0].(map[string]any)
	if later["input"] != float64(1) || later["id"].(float64) <= out["id"].(float64) {
		t.Fatalf("shared cursor order: %v %v", out, later)
	}
	env.Refused("coordinator_owned", "surface", "ack", "--thread", "room", fmt.Sprint(later["id"]))
}

func TestMainInboxReturnCapacityHoldsUntilRecovery(t *testing.T) {
	env, _ := withHarness(t)
	env.AppendConfig("[main_return]\nsurface = \"larry\"\nthread = \"room\"")
	env.Script(map[string][][]map[string]any{"__main__": {{{"sleep": "100ms"}}}})
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Report to main")
	env.OK("task", "start", "t_1")
	env.Eventually("inbox queued", func() bool { return len(env.OK("inbox")["items"].([]any)) == 1 })
	db, err := store.Open(context.Background(), filepath.Join(env.Home, ".local/state/shephrd/shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	err = db.Write(context.Background(), func(tx *store.Tx) error {
		for i := 0; i < 1024; i++ {
			input, err := coord.SubmitSurface(tx, "other", "room", "input")
			if err != nil {
				return err
			}
			if _, err := coord.ReplySurface(tx, input.ID, "reply"); err != nil {
				return err
			}
		}
		return nil
	})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	env.OK("main", "start")
	stop := env.StartDaemon()
	env.Eventually("output mailbox full hold", func() bool { return env.OK("main", "status")["state"] == "held" })
	stop()
	s := env.OK("main", "status")
	if s["reason"] != "surface output mailbox full" || s["result"] != "fake reply 0" || s["route_surface"] != "larry" {
		t.Fatalf("output lost on full mailbox: %v", s)
	}
	if got := env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any); len(got) != 0 {
		t.Fatalf("exposed result before persistence: %v", got)
	}
	env.Refused("mailbox_full", "main", "recover", "--evidence", "inspected result")
	env.OK("surface", "ack", "--thread", "room", "100", "--as", "surface:other")
	env.OK("main", "recover", "--evidence", "independently inspected completed turn and result")
	out := env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any)
	if len(out) != 1 || out[0].(map[string]any)["id"] != float64(1025) {
		t.Fatalf("recovered output: %v", out)
	}
	env.StartDaemon()
	time.Sleep(350 * time.Millisecond)
	seen := 0
	for _, item := range env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any) {
		if item.(map[string]any)["inbox_item"] == float64(1) {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("restart duplicated or lost recovered output: %d", seen)
	}
}

func TestMainCoordinatorCreatesTaskAsMainDuringSurfaceTurn(t *testing.T) {
	env, _ := withHarness(t)
	env.Script(map[string][][]map[string]any{"__main__": {{
		{"shephrd": []string{"task", "create", "--role", "driver", "--repo", "api", "--objective", "Investigate the request"}},
		{"shephrd": []string{"task", "show", "t_1"}},
	}}})
	env.OK("surface", "submit", "--thread", "room", "Investigate the request", "--as", "surface:larry")
	env.OK("main", "start")
	env.Refused("coordinator_owned", "task", "create", "--role", "driver", "--repo", "api", "--objective", "Manual duplicate")
	env.StartDaemon()
	env.Eventually("turn reply", func() bool {
		return len(env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any)) == 1
	})
	commands := []map[string]any{}
	for _, call := range env.Calls() {
		if call["task"] == "__main__" && call["command"] != nil {
			commands = append(commands, call)
		}
	}
	if len(commands) != 2 {
		t.Fatalf("coordinator commands: %v", commands)
	}
	for _, call := range commands {
		if call["error"] != "<nil>" {
			t.Fatalf("coordinator command failed: %v", call)
		}
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(commands[0]["stdout"].(string)), &created); err != nil {
		t.Fatal(err)
	}
	if created["id"] != "t_1" || created["driver"] != "driver:main" {
		t.Fatalf("task ownership: %v", created)
	}
	var shown map[string]any
	if err := json.Unmarshal([]byte(commands[1]["stdout"].(string)), &shown); err != nil {
		t.Fatal(err)
	}
	if task := shown["task"].(map[string]any); task["driver"] != "driver:main" {
		t.Fatalf("task read after create: %v", task)
	}
}

func TestMainCoordinatorHoldsUnacknowledgedInbox(t *testing.T) {
	env, _ := withHarness(t)
	env.AppendConfig("[main_return]\nsurface = \"larry\"\nthread = \"room\"")
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Report to main")
	env.OK("task", "start", "t_1")
	env.Eventually("inbox", func() bool { return len(env.OK("inbox")["items"].([]any)) == 1 })
	env.Script(map[string][][]map[string]any{"__main__": {{{"sleep": "100ms"}}}})
	env.OK("main", "start")
	stop := env.StartDaemon()
	env.Eventually("unacknowledged turn held", func() bool { return env.OK("main", "status")["state"] == "held" })
	stop()
	s := env.OK("main", "status")
	if s["reason"] != "inbox item not acknowledged" || s["item"] != float64(1) {
		t.Fatalf("hold: %v", s)
	}
	env.Refused("coordinator_owned", "inbox", "ack", "1")
	if got := env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any); len(got) != 1 || got[0].(map[string]any)["inbox_item"] != float64(1) {
		t.Fatalf("unacknowledged inbox output not retained: %v", got)
	}
	env.OK("surface", "ack", "--thread", "room", "1", "--as", "surface:larry")
	if s := env.OK("main", "status"); s["state"] != "held" {
		t.Fatalf("surface acknowledgement cleared inbox hold: %v", s)
	}
	env.OK("main", "recover", "--evidence", "read turn log and verified response handled")
	if got := env.OK("surface", "replies", "--thread", "room", "--as", "surface:larry")["replies"].([]any); len(got) != 0 {
		t.Fatalf("recovery replayed acknowledged output: %v", got)
	}
	if s := env.OK("main", "status"); s["state"] != "idle" {
		t.Fatalf("recovery: %v", s)
	}
}

func TestMainCoordinatorRecoveryRefusesLiveProcess(t *testing.T) {
	env := initialized(t)
	env.InstallHarnesses()
	env.Script(map[string][][]map[string]any{"__main__": {{{"sleep": "3s"}}}})
	env.OK("surface", "submit", "--thread", "room", "one", "--as", "surface:larry")
	env.OK("main", "start")
	stop := env.StartDaemon()
	env.Eventually("running", func() bool { return env.OK("main", "status")["state"] == "running" })
	stop()
	env.Refused("turn_active", "main", "reply", "1", "override")
	env.Refused("coordinator_owned", "inbox", "ack", "1")
}

func TestMainCoordinatorRestartHoldsMissingExitRecord(t *testing.T) {
	env := initialized(t)
	env.InstallHarnesses()
	env.Script(map[string][][]map[string]any{"__main__": {{{"sleep": "3s"}}}})
	env.OK("surface", "submit", "--thread", "room", "one", "--as", "surface:larry")
	env.OK("surface", "submit", "--thread", "room", "two", "--as", "surface:larry")
	env.OK("main", "start")
	stop := env.StartDaemon()
	env.Eventually("running", func() bool { return env.OK("main", "status")["state"] == "running" })
	s := env.OK("main", "status")
	env.Eventually("harness launched", func() bool {
		for _, c := range env.Calls() {
			if c["task"] == "__main__" && c["harness"] == "claude" {
				return true
			}
		}
		return false
	})
	stop()
	syscall.Kill(-int(s["pid"].(float64)), syscall.SIGKILL)
	env.Eventually("process exited", func() bool { return !processExists(int(s["pid"].(float64))) })
	env.StartDaemon()
	env.Eventually("held uncertain turn", func() bool { return env.OK("main", "status")["state"] == "held" })
	time.Sleep(500 * time.Millisecond)
	if len(env.OK("main", "pending")["inputs"].([]any)) != 2 {
		t.Fatal("restart dropped pending input")
	}
	var turns int
	for _, call := range env.Calls() {
		if call["task"] == "__main__" && call["harness"] == "claude" {
			turns++
		}
	}
	if turns != 1 {
		t.Fatalf("uncertain turn replayed: %d", turns)
	}
}

func processExists(pid int) bool { return syscall.Kill(pid, 0) == nil }

func cloneClaudeTestAuth(t *testing.T, target string) {
	t.Helper()
	source := os.Getenv("SHEPHRD_TEST_CLAUDE_AUTH_HOME")
	if source == "" {
		t.Skip("set SHEPHRD_TEST_CLAUDE_AUTH_HOME to a separate disposable authenticated Claude profile; never use your live HOME")
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	live, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved == live || strings.HasPrefix(resolved, live+string(os.PathSeparator)) || resolved == target {
		t.Fatal("Claude test auth source must not be the live or destination HOME")
	}
	copied := 0
	for _, name := range []string{".claude.json", filepath.Join(".claude", ".credentials.json")} {
		if name != ".claude.json" {
			dir, err := os.Lstat(filepath.Join(resolved, ".claude"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if !dir.IsDir() {
				t.Fatal("Claude test auth directory must not be a symlink")
			}
		}
		from := filepath.Join(resolved, name)
		info, err := os.Lstat(from)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("Claude auth source %s is not a regular file", name)
		}
		body, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		to := filepath.Join(target, name)
		if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, body, 0600); err != nil {
			t.Fatal(err)
		}
		copied++
	}
	if copied == 0 {
		t.Fatalf("Claude test auth source %s has no file-based profile", resolved)
	}
}

func TestNativeClaudeAuthCloneLeavesSourceUntouched(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"test":"disposable"}`)
	path := filepath.Join(source, ".claude", ".credentials.json")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_TEST_CLAUDE_AUTH_HOME", source)
	cloneClaudeTestAuth(t, target)
	for _, file := range []string{path, filepath.Join(target, ".claude", ".credentials.json")} {
		body, err := os.ReadFile(file)
		if err != nil || string(body) != string(original) {
			t.Fatalf("auth profile copy %s: %v %s", file, err, body)
		}
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("auth file permissions %s: %v %v", file, info, err)
		}
	}
}

func TestNativeClaudeHeadlessCoordinatorCompatibility(t *testing.T) {
	if os.Getenv("SHEPHRD_TEST_CLAUDE") != "1" {
		t.Skip("set SHEPHRD_TEST_CLAUDE=1 to exercise authenticated native Claude headless start and resume")
	}
	env := initialized(t)
	cloneClaudeTestAuth(t, env.Home)
	env.Path = []string{filepath.Dir(env.Bin)}
	env.OK("surface", "submit", "--thread", "smoke", "Reply with exactly OK.", "--as", "surface:smoke")
	env.OK("main", "start")
	env.StartDaemon()
	env.Eventually("native first turn", func() bool {
		return len(env.OK("surface", "replies", "--thread", "smoke", "--as", "surface:smoke")["replies"].([]any)) == 1 || env.OK("main", "status")["state"] == "held"
	})
	if s := env.OK("main", "status"); s["state"] == "held" {
		log, _ := os.ReadFile(s["log"].(string))
		t.Fatalf("native Claude start failed: %v; log tail: %s", s, string(log[max(0, len(log)-1800):]))
	}
	env.OK("surface", "submit", "--thread", "smoke", "Reply with exactly AGAIN.", "--as", "surface:smoke")
	env.Eventually("native resume", func() bool {
		return len(env.OK("surface", "replies", "--thread", "smoke", "--as", "surface:smoke")["replies"].([]any)) == 2 || env.OK("main", "status")["state"] == "held"
	})
	if s := env.OK("main", "status"); s["state"] == "held" {
		log, _ := os.ReadFile(s["log"].(string))
		t.Fatalf("native Claude resume failed: %v; log tail: %s", s, string(log[max(0, len(log)-1800):]))
	}
}
