package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
		return env.OK("main", "status")["state"] == "idle" && env.OK("main", "status")["turn"] == float64(1)
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
}

func TestMainCoordinatorHoldsUnacknowledgedInbox(t *testing.T) {
	env, _ := withHarness(t)
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Report to main")
	env.OK("task", "start", "t_1")
	env.Eventually("inbox", func() bool { return len(env.OK("inbox")["items"].([]any)) == 1 })
	env.Script(map[string][][]map[string]any{"__main__": {{{"sleep": "100ms"}}}})
	env.OK("main", "start")
	env.StartDaemon()
	env.Eventually("unacknowledged turn held", func() bool { return env.OK("main", "status")["state"] == "held" })
	s := env.OK("main", "status")
	if s["reason"] != "inbox item not acknowledged" || s["item"] != float64(1) {
		t.Fatalf("hold: %v", s)
	}
	env.Refused("coordinator_owned", "inbox", "ack", "1")
	env.OK("main", "recover", "--evidence", "read turn log and verified response handled")
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

func TestNativeClaudeHeadlessCoordinatorCompatibility(t *testing.T) {
	if os.Getenv("SHEPHRD_TEST_CLAUDE") != "1" {
		t.Skip("set SHEPHRD_TEST_CLAUDE=1 to exercise authenticated native Claude headless start and resume")
	}
	env := initialized(t)
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
