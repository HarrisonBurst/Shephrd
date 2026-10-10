package proc

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestIdentityProbeAndGroupStop(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 & sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if got := id.Probe(); got != "live" {
		t.Fatalf("probe running process: %s", got)
	}
	if got := (Identity{PID: id.PID, Start: id.Start + "0"}).Probe(); got != "exited" {
		t.Fatalf("probe with another start time: %s", got)
	}
	if !StopGroup(cmd.Process.Pid, 2*time.Second) {
		t.Fatal("group not confirmed stopped")
	}
	deadline := time.Now().Add(5 * time.Second)
	for id.Probe() != "exited" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := id.Probe(); got != "exited" {
		t.Fatalf("probe stopped process: %s", got)
	}
}

func TestProbeOfMissingProcess(t *testing.T) {
	if got := (Identity{PID: 0}).Probe(); got != "unknown" {
		t.Fatalf("pid 0: %s", got)
	}
	self, err := Of(os.Getpid())
	if err != nil || self.Probe() != "live" {
		t.Fatalf("self: %+v, %v", self, err)
	}
}
