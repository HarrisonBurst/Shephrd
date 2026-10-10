// Command fakessh stands in for ssh in tests. Each simulated machine has
// its own HOME; $HOME/.fakessh/targets.json maps an SSH target to the
// forced command its authorized_keys would run and the HOME of the machine
// it reaches. A file in $HOME/.fakessh/down/<target> makes the target
// unreachable; one in $HOME/.fakessh/drop/<target> runs the command and
// then drops the connection once.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type target struct {
	Command []string `json:"command"`
	Home    string   `json:"home"`
}

func main() {
	args := os.Args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if args[0] == "-o" {
			args = args[1:]
		}
		args = args[1:]
	}
	if len(args) == 0 {
		fail("usage: ssh target command")
	}
	name, command := args[0], strings.Join(args[1:], " ")
	dir := filepath.Join(os.Getenv("HOME"), ".fakessh")
	if _, err := os.Stat(filepath.Join(dir, "down", name)); err == nil {
		fail("ssh: connect to host " + name + ": Connection refused")
	}
	var targets map[string]target
	body, _ := os.ReadFile(filepath.Join(dir, "targets.json"))
	json.Unmarshal(body, &targets)
	t, ok := targets[name]
	if !ok {
		fail("ssh: Could not resolve hostname " + name)
	}
	cmd := exec.Command(t.Command[0], t.Command[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.Home, "SSH_ORIGINAL_COMMAND=" + command}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	drop := filepath.Join(dir, "drop", name)
	if _, statErr := os.Stat(drop); statErr == nil {
		os.Remove(drop)
		fail("Connection to " + name + " closed by remote host.")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	if err != nil {
		fail(err.Error())
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(255)
}
