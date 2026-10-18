package proc

import (
	"errors"
	"syscall"
)

func GroupAlive(pgid int) bool {
	if pgid <= 1 {
		return true
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
