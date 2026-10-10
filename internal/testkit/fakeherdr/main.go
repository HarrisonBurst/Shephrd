// Command fakeherdr stands in for herdr in tests. Panes live in
// $HOME/.fakeherdr/panes.json; `pane run` starts the command the way a
// terminal would, detached, and `pane close` kills it.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shephrd/internal/proc"
)

type pane struct {
	ID      string `json:"pane_id"`
	Tab     string `json:"tab_id"`
	PID     int    `json:"pid"`
	Start   string `json:"start"`
	Closed  bool   `json:"closed"`
	Command string `json:"command"`
	Label   string `json:"label"`
	Runs    int    `json:"runs"`
}

var dir = filepath.Join(os.Getenv("HOME"), ".fakeherdr")

func main() {
	args := os.Args[1:]
	panes := load()
	find := func(id string) *pane {
		for i := range panes {
			if panes[i].ID == id && !panes[i].Closed {
				return &panes[i]
			}
		}
		fmt.Println(`{"error":{"code":"pane_not_found","message":"no such pane"}}`)
		os.Exit(1)
		return nil
	}
	switch strings.Join(args[:2], " ") {
	case "workspace list":
		fmt.Println(`{"result":{"workspaces":[{"workspace_id":"w1","label":"tests"}]}}`)
	case "tab create":
		n := strconv.Itoa(len(panes) + 1)
		label := ""
		if i := slices.Index(args, "--label"); i >= 0 {
			label = args[i+1]
		}
		panes = append(panes, pane{ID: "p" + n, Tab: "t" + n, Label: label})
		save(panes)
		fmt.Printf(`{"result":{"tab":{"tab_id":"t%s"},"root_pane":{"pane_id":"p%s"}}}`+"\n", n, n)
	case "pane list":
		fmt.Println(`{"result":{"panes":[]}}`)
	case "pane run":
		p := find(args[2])
		for started := time.Now(); time.Since(started) < 10*time.Second; time.Sleep(25 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, "pause-before-run")); os.IsNotExist(err) {
				break
			}
		}
		cmd := exec.Command("sh", "-c", strings.Join(args[3:], " "))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		log, _ := os.OpenFile(filepath.Join(dir, p.ID+".log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		cmd.Stdout, cmd.Stderr = log, log
		input := filepath.Join(dir, p.ID+".stdin")
		if err := syscall.Mkfifo(input, 0o600); err != nil && !os.IsExist(err) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		stdin, err := os.OpenFile(input, os.O_RDWR, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer stdin.Close()
		cmd.Stdin = stdin
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		p.PID, p.Command = cmd.Process.Pid, strings.Join(args[3:], " ")
		if id, err := proc.Of(p.PID); err == nil {
			p.Start = id.Start
		}
		p.Runs++
		save(panes)
	case "tab rename":
		for i := range panes {
			if panes[i].Tab == args[2] {
				panes[i].Label = strings.Join(args[3:], " ")
			}
		}
		save(panes)
	case "pane get":
		p := find(args[2])
		fmt.Printf(`{"result":{"pane":{"pane_id":%q,"tab_id":%q}}}`+"\n", p.ID, p.Tab)
	case "pane close":
		fmt.Fprintln(os.Stderr, "pane.close is forbidden")
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "unsupported", args)
		os.Exit(2)
	}
}

func load() []pane {
	var panes []pane
	body, _ := os.ReadFile(filepath.Join(dir, "panes.json"))
	json.Unmarshal(body, &panes)
	changed := false
	for i := range panes {
		p := &panes[i]
		if !p.Closed && p.PID != 0 && p.Start != "" && (proc.Identity{PID: p.PID, Start: p.Start}).Probe() == "exited" {
			p.Closed = true
			changed = true
		}
	}
	if changed {
		save(panes)
	}
	return panes
}

func save(panes []pane) {
	os.MkdirAll(dir, 0o700)
	body, _ := json.Marshal(panes)
	os.WriteFile(filepath.Join(dir, "panes.json"), body, 0o600)
}
