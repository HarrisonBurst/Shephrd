package control

import (
	"os"
	"os/exec"
	"time"

	"shephrd/internal/process"
)

type clockCapability struct {
	now   func() time.Time
	sleep func(time.Duration)
}

type environmentCapability struct {
	get        func(string) string
	list       func() []string
	executable func() (string, error)
}

type processCapability struct {
	alive       func(int) bool
	stop        func(int) error
	detach      func(*exec.Cmd)
	containSelf func() error
}

func (s Service) now() time.Time {
	if s.clock.now != nil {
		return s.clock.now()
	}
	return time.Now()
}

func (s Service) sleep(duration time.Duration) {
	if s.clock.sleep != nil {
		s.clock.sleep(duration)
		return
	}
	time.Sleep(duration)
}

func (s Service) getenv(name string) string {
	if s.environment.get != nil {
		return s.environment.get(name)
	}
	return os.Getenv(name)
}

func (s Service) environ() []string {
	if s.environment.list != nil {
		return s.environment.list()
	}
	return os.Environ()
}

func (s Service) executable() (string, error) {
	if s.environment.executable != nil {
		return s.environment.executable()
	}
	return os.Executable()
}

func (s Service) processAlive(pid int) bool {
	if s.processes.alive != nil {
		return s.processes.alive(pid)
	}
	return process.Alive(pid)
}

func (s Service) stopProcess(pid int) error {
	if s.processes.stop != nil {
		return s.processes.stop(pid)
	}
	return process.Stop(pid)
}

func (s Service) detachProcess(command *exec.Cmd) {
	if s.processes.detach != nil {
		s.processes.detach(command)
		return
	}
	process.Detach(command)
}

func (s Service) containProcess() error {
	if s.processes.containSelf != nil {
		return s.processes.containSelf()
	}
	return process.ContainSelf()
}
