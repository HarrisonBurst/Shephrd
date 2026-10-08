package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
)

func TestDarwinExitedUnreapedSessionLeaderRequiresReapBeforeStopSucceeds(t *testing.T) {
	exited := make(chan os.Signal, 1)
	signal.Notify(exited, syscall.SIGCHLD)
	defer signal.Stop(exited)

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	releaseR, releaseW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `printf 'ready\n' >&3; IFS= read -r _ <&4`)
	cmd.ExtraFiles = []*os.File{readyW, releaseR}
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	readyW.Close()
	releaseR.Close()
	t.Cleanup(func() {
		readyR.Close()
		releaseW.Close()
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
		_ = Stop(pid)
	})

	var ready string
	if _, err := fmt.Fscan(readyR, &ready); err != nil {
		t.Fatal(err)
	}
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Fatalf("session leader pgid=%d err=%v want=%d", pgid, err, pid)
	}
	if _, err := releaseW.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	releaseW.Close()
	for {
		_, err := syscall.Getpgid(pid)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		<-exited
	}
	if !Alive(pid) {
		t.Fatal("exited-unreaped process is not visible by direct PID")
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("group probe=%v want=%v", err, syscall.EPERM)
	}
	if err := Stop(pid); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("Stop before reap=%v want=%v", err, syscall.EPERM)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if Alive(pid) {
		t.Fatal("direct process remains after reap")
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("group probe after reap=%v want=%v", err, syscall.ESRCH)
	}
	if err := Stop(pid); err != nil {
		t.Fatalf("Stop after reap=%v", err)
	}
}
