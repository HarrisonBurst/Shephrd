package process

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestPOSIXDetachCreatesSession(t *testing.T) {
	cmd := exec.Command("true")
	Detach(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("Detach did not create a session")
	}
	if err := ContainSelf(); err != nil {
		t.Fatal(err)
	}
}

func TestPOSIXAliveRejectsInvalidPIDs(t *testing.T) {
	if !Alive(syscall.Getpid()) {
		t.Fatal("current process is not alive")
	}
	for _, pid := range []int{-1, 0} {
		if Alive(pid) {
			t.Fatalf("Alive(%d) = true", pid)
		}
	}
}

func TestPOSIXStopTerminatesSessionProcessGroup(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "/bin/sleep 60")
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	if !Alive(pid) || !groupAlive(pid) {
		t.Fatalf("process group %d did not start", pid)
	}
	if err := Stop(pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatalf("process group %d was not reaped", pid)
	}
	if Alive(pid) || groupAlive(pid) {
		t.Fatalf("process group %d remains alive", pid)
	}
}
