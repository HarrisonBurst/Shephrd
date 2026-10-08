package control

import "syscall"

func waitForChildProcess(pid int) {
	var status syscall.WaitStatus
	for {
		if _, err := syscall.Wait4(pid, &status, 0, nil); err == syscall.EINTR {
			continue
		}
		return
	}
}
