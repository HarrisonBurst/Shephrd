package execution

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shephrd/internal/proc"
)

// RunSpec is what the launcher hands the supervisor in the run directory.
// The run token travels only in the supervisor's environment.
type RunSpec struct {
	Command   []string          `json:"command"`
	Env       map[string]string `json:"env,omitempty"`
	Workspace string            `json:"workspace"`
	Harness   string            `json:"harness"`
	Shephrd   string            `json:"shephrd"`
	Config    string            `json:"config"`
	Path      string            `json:"path"`
	Home      string            `json:"home"`
	Account   []string          `json:"account,omitempty"`
	// Interactive runs the harness's own UI on the supervisor's terminal.
	Interactive bool   `json:"interactive,omitempty"`
	Label       string `json:"label,omitempty"`
}

// TurnGrace is how long an interactive harness may keep writing after its
// turn is reported, before the supervisor ends it.
var TurnGrace = 5 * time.Second

type ExitRecord struct {
	Status  int    `json:"status"`
	Session string `json:"session,omitempty"`
	Time    string `json:"time"`
}

func LogPath(runDir string) string   { return filepath.Join(runDir, "session.log") }
func ExitPath(runDir string) string  { return filepath.Join(runDir, "exit.json") }
func BriefPath(runDir string) string { return filepath.Join(runDir, "brief.md") }

// Launch starts the supervisor for a prepared run in its own session, so
// the supervisor and the harness form one process group.
func Launch(runDir string, spec RunSpec, token string) (proc.Identity, error) {
	body, err := json.Marshal(spec)
	if err != nil {
		return proc.Identity{}, err
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), body, 0o600); err != nil {
		return proc.Identity{}, err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return proc.Identity{}, err
	}
	defer devnull.Close()
	supervisorLog, err := os.OpenFile(filepath.Join(runDir, "supervisor.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return proc.Identity{}, err
	}
	defer supervisorLog.Close()
	cmd := exec.Command(spec.Shephrd, "_run", runDir)
	cmd.Env = spec.environment(token)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, supervisorLog, supervisorLog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return proc.Identity{}, err
	}
	go cmd.Wait()
	return proc.Of(cmd.Process.Pid)
}

func (s RunSpec) environment(token string) []string {
	return append([]string{"PATH=" + filepath.Dir(s.Shephrd) + ":" + s.Path, "HOME=" + s.Home, "SHEPHRD_CONFIG=" + s.Config, "SHEPHRD_RUN_TOKEN=" + token}, s.Account...)
}

// Supervise is the body of `shephrd _run`: it records its own identity,
// runs the harness to exit, and reports the exit. The harness gets only the
// environment the spec names, never the supervisor's other variables.
func Supervise(runDir string) int {
	body, err := os.ReadFile(filepath.Join(runDir, "run.json"))
	if err != nil {
		return failSupervise(runDir, "read run.json: "+err.Error())
	}
	var spec RunSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return failSupervise(runDir, "decode run.json: "+err.Error())
	}
	syscall.Setpgid(0, 0)
	token := os.Getenv("SHEPHRD_RUN_TOKEN")
	if token == "" {
		if token, err = readToken(runDir); err != nil {
			return failSupervise(runDir, err.Error())
		}
	}
	self, err := proc.Of(os.Getpid())
	if err != nil {
		return failSupervise(runDir, "identify supervisor: "+err.Error())
	}
	if err := spec.shephrd(token, "_started", "--pid", strconv.Itoa(self.PID), "--start", self.Start); err != nil {
		return failSupervise(runDir, "record start: "+err.Error())
	}
	log, err := os.OpenFile(LogPath(runDir), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return failSupervise(runDir, "open session log: "+err.Error())
	}
	if len(spec.Command) == 0 {
		log.Close()
		return failSupervise(runDir, "harness provider returned no command")
	}
	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Workspace
	cmd.Env = spec.environment(token)
	for key, value := range spec.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if spec.Interactive {
		fmt.Fprintf(log, "This run used %s interactively in a terminal; its conversation is in the harness's own session.\n", spec.Harness)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		fmt.Printf("\033]0;%s\007", spec.Label)
	}
	started := time.Now()
	status := 0
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(log, "\nshephrd: harness failed to start: %v\n", err)
		status = 127
	} else {
		done := make(chan struct{})
		if spec.Interactive {
			go spec.endTurn(token, cmd.Process, done)
		}
		err := cmd.Wait()
		close(done)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			status = exit.ExitCode()
		}
	}
	log.Close()
	record := ExitRecord{Status: status, Time: time.Now().UTC().Format(time.RFC3339)}
	switch {
	case spec.Harness == "codex" && spec.Interactive:
		record.Session = codexInteractiveSession(spec.Home, spec.Workspace, started)
	case spec.Harness == "codex":
		record.Session = codexSession(LogPath(runDir))
	}
	hold := false
	if spec.Interactive {
		ended, err := spec.turnEnded(token)
		hold = err != nil || !ended
		if hold {
			fmt.Printf("\nShephrd run ended without a completed report. Read its log: shephrd task log %s\nPress Enter to leave this terminal.\n", filepath.Base(filepath.Dir(runDir)))
		}
	}
	if err := writeExitRecord(runDir, record); err != nil {
		fmt.Fprintln(os.Stderr, "write exit record:", err)
		return 1
	}
	if hold {
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	return 0
}

// shephrd runs one of the supervisor's own commands, keeping its output
// out of the terminal the harness uses.
func (s RunSpec) shephrd(token string, args ...string) error {
	_, err := s.shephrdOutput(token, args...)
	return err
}

func (s RunSpec) shephrdOutput(token string, args ...string) ([]byte, error) {
	cmd := exec.Command(s.Shephrd, args...)
	cmd.Env = s.environment(token)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("shephrd %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (s RunSpec) turnEnded(token string) (bool, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		out, err := s.shephrdOutput(token, "_turn")
		if err == nil {
			var turn struct {
				Ended bool `json:"ended"`
			}
			if err := json.Unmarshal(out, &turn); err == nil {
				return turn.Ended, nil
			} else {
				last = err
			}
		} else {
			last = err
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
		}
	}
	return false, last
}

// endTurn ends an interactive harness once its turn is reported, or once
// the run is no longer current, after a short grace to finish writing.
func (s RunSpec) endTurn(token string, harness *os.Process, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-time.After(2 * time.Second):
		}
		out, err := s.shephrdOutput(token, "_turn")
		var turn struct {
			Ended bool `json:"ended"`
		}
		if err != nil || json.Unmarshal(out, &turn) != nil || !turn.Ended {
			continue
		}
		select {
		case <-done:
			return
		case <-time.After(TurnGrace):
		}
		harness.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			harness.Kill()
		}
		return
	}
}

func failSupervise(runDir, message string) int {
	fmt.Fprintln(os.Stderr, message)
	if err := writeExitRecord(runDir, ExitRecord{Status: 1, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		fmt.Fprintln(os.Stderr, "write exit record:", err)
	}
	return 1
}

func writeExitRecord(runDir string, record ExitRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return os.WriteFile(ExitPath(runDir), body, 0o600)
}

func ReadExit(runDir string) (ExitRecord, bool) {
	body, err := os.ReadFile(ExitPath(runDir))
	if err != nil {
		return ExitRecord{}, false
	}
	var record ExitRecord
	return record, json.Unmarshal(body, &record) == nil
}

// ReadLog returns up to limit bytes from the end of a run's session log.
func ReadLog(runDir string, limit int64) (string, bool, error) {
	file, err := os.Open(LogPath(runDir))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	offset := max(info.Size()-limit, 0)
	buf := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return "", false, err
	}
	return string(buf), offset > 0, nil
}
