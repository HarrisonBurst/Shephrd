// Command fakeharness stands in for claude, codex and pi in tests. It finds
// its task in the brief it is given and plays the actions scripted for
// that task's next run in $HOME/.fakeharness/script.json. Run
// interactively, it reads the brief its prompt points at and then waits,
// as a harness UI does, until it is ended.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type action struct {
	Work    string            `json:"work"`
	Report  []string          `json:"report"`
	Shephrd []string          `json:"shephrd"`
	Write   map[string]string `json:"write"`
	Git     []string          `json:"git"`
	Sleep   string            `json:"sleep"`
	Exit    *int              `json:"exit"`
}

var (
	briefTask        = regexp.MustCompile(`# Shephrd brief: (t_\d+)`)
	briefDeliverable = regexp.MustCompile(`\*\*Deliverable:\*\* (\w+)`)
	briefPointer     = regexp.MustCompile(`Read your Shephrd brief at (\S+) and follow it`)
)

func main() {
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".fakeharness")
	os.MkdirAll(dir, 0o700)
	harness := filepath.Base(os.Args[0])
	interactive := !slices.Contains(os.Args, "-p") && !slices.Contains(os.Args, "--print") && !slices.Contains(os.Args, "exec")
	task, brief := "", ""
	for _, arg := range os.Args[1:] {
		if m := briefPointer.FindStringSubmatch(arg); m != nil {
			body, _ := os.ReadFile(m[1])
			brief = string(body)
		}
		if briefTask.MatchString(arg) {
			brief = arg
		}
	}
	if m := briefTask.FindStringSubmatch(brief); m != nil {
		task = m[1]
	}
	countFile := filepath.Join(dir, task+".runs")
	body, _ := os.ReadFile(countFile)
	index, _ := strconv.Atoi(strings.TrimSpace(string(body)))
	os.WriteFile(countFile, []byte(strconv.Itoa(index+1)), 0o600)

	args := make([]string, 0, len(os.Args)-1)
	for _, arg := range os.Args[1:] {
		if briefTask.MatchString(arg) {
			arg = "<brief>"
		}
		args = append(args, arg)
	}
	cwd, _ := os.Getwd()
	record(dir, map[string]any{"harness": harness, "task": task, "run": index, "args": args, "cwd": cwd, "env": os.Environ(), "brief": brief, "interactive": interactive})
	if harness == "codex" && !contains(args, "resume") {
		session := fmt.Sprintf("codex-%s-%d", task, time.Now().UnixNano())
		if interactive {
			sessions := filepath.Join(home, ".codex", "sessions", "2026", "01", "01")
			os.MkdirAll(sessions, 0o700)
			meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": session, "cwd": cwd}})
			os.WriteFile(filepath.Join(sessions, "rollout-"+session+".jsonl"), append(meta, '\n'), 0o600)
		} else {
			fmt.Printf(`{"type":"thread.started","thread_id":%q}`+"\n", session)
		}
	}

	var script map[string][][]action
	if body, err := os.ReadFile(filepath.Join(dir, "script.json")); err == nil {
		json.Unmarshal(body, &script)
	}
	runs := script[task]
	var actions []action
	switch m := briefDeliverable.FindStringSubmatch(brief); {
	case index < len(runs):
		actions = runs[index]
	case m != nil && m[1] == "code":
		actions = []action{{Work: "Do the work"}, {Report: []string{"result", "done"}}}
	case m != nil && m[1] == "report":
		actions = []action{{Write: map[string]string{"report.md": "# Findings\n"}}, {Report: []string{"result", "done", "--file", "report.md"}}}
	default:
		actions = []action{{Report: []string{"result", "done"}}}
	}
	for _, a := range actions {
		switch {
		case a.Work != "":
			name := fmt.Sprintf("work-%s-%d-%d.txt", task, index, time.Now().UnixNano())
			os.WriteFile(filepath.Join(cwd, name), []byte(a.Work+"\n"), 0o644)
			git := []string{"-c", "user.name=Fake", "-c", "user.email=fake@shephrd.invalid", "-c", "commit.gpgsign=false"}
			run(dir, task, "git", append(git, "add", name), cwd)
			run(dir, task, "git", append(git, "commit", "-q", "-m", a.Work), cwd)
		case a.Report != nil:
			run(dir, task, "shephrd", append([]string{"report"}, a.Report...), cwd)
		case a.Shephrd != nil:
			run(dir, task, "shephrd", a.Shephrd, cwd)
		case a.Write != nil:
			for path, content := range a.Write {
				full := filepath.Join(cwd, path)
				os.MkdirAll(filepath.Dir(full), 0o755)
				err := os.WriteFile(full, []byte(content), 0o644)
				record(dir, map[string]any{"task": task, "write": path, "error": fmt.Sprint(err)})
			}
		case a.Git != nil:
			run(dir, task, "git", append([]string{"-c", "user.name=Fake", "-c", "user.email=fake@shephrd.invalid", "-c", "commit.gpgsign=false"}, a.Git...), cwd)
		case a.Sleep != "":
			d, _ := time.ParseDuration(a.Sleep)
			time.Sleep(d)
		case a.Exit != nil:
			os.Exit(*a.Exit)
		}
	}
	if interactive {
		fmt.Println("turn finished; waiting for input")
		time.Sleep(time.Hour)
	}
}

func run(dir, task, name string, args []string, cwd string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	stderr := ""
	if exit, ok := err.(*exec.ExitError); ok {
		stderr = string(exit.Stderr)
	}
	record(dir, map[string]any{"task": task, "command": append([]string{name}, args...), "stdout": string(out), "stderr": stderr, "error": fmt.Sprint(err)})
}

func record(dir string, entry map[string]any) {
	line, _ := json.Marshal(entry)
	file, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	file.Write(append(line, '\n'))
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
