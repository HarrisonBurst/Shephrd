package proc

import (
	"errors"
	"os"
	"strconv"
	"syscall"
)

func GroupAlive(pgid int) bool {
	if pgid <= 1 {
		return true
	}
	switch err := syscall.Kill(-pgid, 0); {
	case errors.Is(err, syscall.ESRCH):
		return false
	case err != nil:
		return true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		state, group, _, err := linuxStat(pid)
		if errors.Is(err, errNoProcess) {
			continue
		}
		if err != nil {
			return true
		}
		if group == pgid && state != "Z" && state != "X" {
			return true
		}
	}
	return false
}
