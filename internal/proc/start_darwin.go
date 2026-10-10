package proc

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

func startTime(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if errors.Is(err, unix.EIO) && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return "", errNoProcess
	}
	if err == nil && info.Proc.P_pid != int32(pid) {
		return "", errNoProcess
	}
	if err != nil {
		return "", err
	}
	if info.Proc.P_stat == 5 {
		return "", errNoProcess
	}
	start := info.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", start.Sec, start.Usec), nil
}
