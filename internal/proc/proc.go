package proc

import (
	"errors"
	"syscall"
	"time"
)

// Identity names one process: a PID is reused, a PID with its start time is not.
type Identity struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
}

func Of(pid int) (Identity, error) {
	start, err := startTime(pid)
	if err != nil {
		return Identity{}, err
	}
	return Identity{PID: pid, Start: start}, nil
}

// Probe answers live, exited or unknown for a recorded identity.
func (id Identity) Probe() string {
	if id.PID <= 0 {
		return "unknown"
	}
	start, err := startTime(id.PID)
	switch {
	case errors.Is(err, errNoProcess):
		return "exited"
	case err != nil:
		return "unknown"
	case start != id.Start:
		return "exited"
	}
	return "live"
}

// GroupAlive reports whether any process remains in the group.
func GroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// StopGroup terminates a process group, then kills it after the grace
// period. It reports true only once no process remains in the group.
func StopGroup(pgid int, grace time.Duration) bool {
	if pgid <= 1 {
		return false
	}
	if !GroupAlive(pgid) {
		return true
	}
	syscall.Kill(-pgid, syscall.SIGTERM)
	if waitGone(pgid, grace) {
		return true
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
	return waitGone(pgid, 5*time.Second)
}

func waitGone(pgid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !GroupAlive(pgid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !GroupAlive(pgid)
}

var errNoProcess = errors.New("no such process")
