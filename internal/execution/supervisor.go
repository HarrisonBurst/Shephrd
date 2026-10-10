package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
}

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
	return append([]string{"PATH=" + s.Path, "HOME=" + s.Home, "SHEPHRD_CONFIG=" + s.Config, "SHEPHRD_RUN_TOKEN=" + token}, s.Account...)
}

// Supervise is the body of `shephrd _run`: it records its own identity,
// runs the harness to exit, and reports the exit. The harness gets only the
// environment the spec names, never the supervisor's other variables.
func Supervise(runDir string) int {
	body, err := os.ReadFile(filepath.Join(runDir, "run.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "read run.json:", err)
		return 1
	}
	var spec RunSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "decode run.json:", err)
		return 1
	}
	syscall.Setpgid(0, 0)
	token := os.Getenv("SHEPHRD_RUN_TOKEN")
	if token == "" {
		if token, err = readToken(runDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	self, err := proc.Of(os.Getpid())
	if err != nil {
		fmt.Fprintln(os.Stderr, "identify supervisor:", err)
		return 1
	}
	if err := spec.shephrd(token, "_started", "--pid", strconv.Itoa(self.PID), "--start", self.Start); err != nil {
		fmt.Fprintln(os.Stderr, "record start:", err)
		return 1
	}
	log, err := os.OpenFile(LogPath(runDir), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open session log:", err)
		return 1
	}
	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Workspace
	cmd.Env = spec.environment(token)
	for key, value := range spec.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdout, cmd.Stderr = log, log
	status := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			status = exit.ExitCode()
		} else {
			fmt.Fprintf(log, "\nshephrd: harness failed to start: %v\n", err)
			status = 127
		}
	}
	log.Close()
	record := ExitRecord{Status: status, Time: time.Now().UTC().Format(time.RFC3339)}
	if spec.Harness == "codex" {
		record.Session = codexSession(LogPath(runDir))
	}
	out, _ := json.Marshal(record)
	os.WriteFile(ExitPath(runDir), out, 0o600)
	args := []string{"_exited", "--status", strconv.Itoa(status)}
	if record.Session != "" {
		args = append(args, "--session", record.Session)
	}
	for attempt := 0; attempt < 5; attempt++ {
		if err := spec.shephrd(token, args...); err == nil {
			return 0
		} else {
			fmt.Fprintln(os.Stderr, "record exit:", err)
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return 1
}

func (s RunSpec) shephrd(token string, args ...string) error {
	cmd := exec.Command(s.Shephrd, args...)
	cmd.Env = s.environment(token)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
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
