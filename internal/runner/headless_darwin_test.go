package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"

	"shephrd/internal/process"
)

func TestStopHeadlessProcessRechecksExitedTreeAfterReaping(t *testing.T) {
	cmd, pid, _ := startExitedUnreapedHeadlessProcess(t, false)
	initialErr := process.Stop(pid)
	if !errors.Is(initialErr, syscall.EPERM) {
		t.Fatalf("initial Stop=%v want=%v", initialErr, syscall.EPERM)
	}
	var calls atomic.Int32
	err := stopHeadlessProcessWith(cmd, func(got int) error {
		if got != pid {
			return fmt.Errorf("stop pid=%d want=%d", got, pid)
		}
		if calls.Add(1) == 1 {
			return initialErr
		}
		if process.Alive(pid) {
			return errors.New("post-reap Stop called while direct child is alive")
		}
		if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("post-reap group probe=%v want=%v", err, syscall.ESRCH)
		}
		return process.Stop(pid)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Stop calls=%d want=2", got)
	}
}

func TestTimedRepairStopRechecksExitedTreeAfterReaping(t *testing.T) {
	cmd, pid, _ := startExitedUnreapedHeadlessProcess(t, false)
	initialErr := process.Stop(pid)
	if !errors.Is(initialErr, syscall.EPERM) {
		t.Fatalf("initial Stop=%v want=%v", initialErr, syscall.EPERM)
	}
	stopped := make(chan error, 1)
	stopped <- initialErr
	var calls atomic.Int32
	err := waitHeadlessProcessStop(cmd, stopped, func(got int) error {
		calls.Add(1)
		if got != pid {
			return fmt.Errorf("stop pid=%d want=%d", got, pid)
		}
		if process.Alive(pid) {
			return errors.New("timed post-reap Stop called while direct child is alive")
		}
		if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("timed post-reap group probe=%v want=%v", err, syscall.ESRCH)
		}
		return process.Stop(pid)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("post-reap Stop calls=%d want=1", got)
	}
}

func TestHeadlessProcessStopsLiveDescendantsAfterLeaderExit(t *testing.T) {
	cmd, pid, descendant := startExitedUnreapedHeadlessProcess(t, true)
	if err := stopHeadlessProcess(cmd); err != nil {
		t.Fatal(err)
	}
	assertHeadlessProcessTreeGone(t, pid, descendant)
}

func TestHeadlessPostReapStopsFailClosedForLiveInaccessibleGroups(t *testing.T) {
	for _, timed := range []bool{false, true} {
		name := "ordinary"
		if timed {
			name = "timed-repair"
		}
		t.Run(name, func(t *testing.T) {
			cmd, pid, descendant := startExitedUnreapedHeadlessProcess(t, true)
			inaccessible := errors.New("live process group is inaccessible")
			var calls atomic.Int32
			stop := func(got int) error {
				call := calls.Add(1)
				if got != pid {
					return fmt.Errorf("stop pid=%d want=%d", got, pid)
				}
				if !timed && call == 1 {
					return syscall.EPERM
				}
				if process.Alive(pid) {
					return errors.New("post-reap Stop called while direct child is alive")
				}
				if !process.Alive(descendant) {
					return errors.New("controlled descendant exited before inaccessible-group recheck")
				}
				return inaccessible
			}
			var err error
			if timed {
				stopped := make(chan error, 1)
				stopped <- syscall.EPERM
				err = waitHeadlessProcessStop(cmd, stopped, stop)
			} else {
				err = stopHeadlessProcessWith(cmd, stop)
			}
			if !errors.Is(err, inaccessible) {
				t.Fatalf("stop error=%v want=%v", err, inaccessible)
			}
			if err := process.Stop(pid); err != nil {
				t.Fatalf("cleanup live process group: %v", err)
			}
			assertHeadlessProcessTreeGone(t, pid, descendant)
		})
	}
}

func startExitedUnreapedHeadlessProcess(t *testing.T, withDescendant bool) (*exec.Cmd, int, int) {
	t.Helper()
	exited := make(chan os.Signal, 1)
	signal.Notify(exited, syscall.SIGCHLD)

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	releaseR, releaseW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	script := `printf 'ready\n' >&3; IFS= read -r _ <&4`
	if withDescendant {
		script = `/bin/sleep 60 3>&- 4>&- & printf '%s\n' "$!" >&3; IFS= read -r _ <&4`
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.ExtraFiles = []*os.File{readyW, releaseR}
	process.Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	readyW.Close()
	releaseR.Close()
	t.Cleanup(func() {
		signal.Stop(exited)
		readyR.Close()
		releaseW.Close()
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
		_ = process.Stop(pid)
	})

	var ready string
	if _, err := fmt.Fscan(readyR, &ready); err != nil {
		t.Fatal(err)
	}
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Fatalf("session leader pgid=%d err=%v want=%d", pgid, err, pid)
	}
	descendant := 0
	if withDescendant {
		descendant, err = strconv.Atoi(ready)
		if err != nil {
			t.Fatal(err)
		}
		if pgid, err := syscall.Getpgid(descendant); err != nil || pgid != pid {
			t.Fatalf("descendant pgid=%d err=%v want=%d", pgid, err, pid)
		}
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
	signal.Stop(exited)
	if !process.Alive(pid) {
		t.Fatal("exited-unreaped leader is not visible by direct PID")
	}
	groupErr := syscall.Kill(-pid, 0)
	if withDescendant {
		if groupErr != nil && !errors.Is(groupErr, syscall.EPERM) {
			t.Fatalf("live descendant group probe=%v", groupErr)
		}
		if !process.Alive(descendant) {
			t.Fatal("descendant exited before cleanup")
		}
	} else if !errors.Is(groupErr, syscall.EPERM) {
		t.Fatalf("zombie-only group probe=%v want=%v", groupErr, syscall.EPERM)
	}
	return cmd, pid, descendant
}

func assertHeadlessProcessTreeGone(t *testing.T, pid, descendant int) {
	t.Helper()
	if process.Alive(pid) {
		t.Fatal("direct child remains alive")
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group probe=%v want=%v", err, syscall.ESRCH)
	}
	if descendant > 0 && process.Alive(descendant) {
		t.Fatal("descendant remains alive")
	}
}
