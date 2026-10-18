package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func zombieLeader(t *testing.T, script string, args ...string) (*exec.Cmd, Identity) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", "read line; " + script, "sh"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	input.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", id.PID))
		if err == nil && strings.Fields(string(stat)[strings.LastIndexByte(string(stat), ')')+1:])[0] == "Z" {
			return cmd, id
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("group leader %d did not become a zombie", id.PID)
	return nil, Identity{}
}

func TestZombieOnlyGroupIsGone(t *testing.T) {
	_, id := zombieLeader(t, "exit 0")
	if err := syscall.Kill(-id.PID, 0); err != nil {
		t.Fatalf("zombie must still be visible to kill(0): %v", err)
	}
	if got := id.Probe(); got != "exited" {
		t.Fatalf("zombie identity: %s", got)
	}
	if GroupAlive(id.PID) || !StopGroup(id.PID, 100*time.Millisecond) {
		t.Fatal("zombie-only group was treated as live")
	}
}

func TestLiveGroupWithZombieLeaderIsNotGone(t *testing.T) {
	childFile := filepath.Join(t.TempDir(), "child")
	_, id := zombieLeader(t, "sleep 60 & echo $! > \"$1\"", childFile)
	body, err := os.ReadFile(childFile)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Of(child); err != nil {
		t.Fatalf("live child missing: %v", err)
	}
	if !GroupAlive(id.PID) {
		t.Fatal("live child in zombie leader's group was missed")
	}
	if got := id.Probe(); got != "exited" {
		t.Fatalf("zombie leader: %s", got)
	}
	if err := syscall.Kill(-id.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
}
