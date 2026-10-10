package testkit

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var (
	buildMu  sync.Mutex
	buildDir string
	built    = map[string]string{}
)

func Main(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func Binary(t testing.TB) string {
	t.Helper()
	return Tool(t, "./cmd/shephrd", "shephrd")
}

// Tool builds a main package from this module once per test process.
func Tool(t testing.TB, pkg, name string) string {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if path, ok := built[pkg]; ok {
		return path
	}
	if buildDir == "" {
		dir, err := os.MkdirTemp("", "shephrd-testkit-")
		if err != nil {
			t.Fatal(err)
		}
		buildDir = dir
	}
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(buildDir, name)
	cmd := exec.Command("go", "build", "-o", path, pkg)
	cmd.Dir = filepath.Join(filepath.Dir(file), "..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	built[pkg] = path
	return path
}

func GitRepo(t testing.TB, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	Git(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	Git(t, dir, "add", "README.md")
	Git(t, dir, "commit", "-q", "-m", "Initial commit")
	return dir
}

func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Shephrd Test", "-c", "user.email=test@shephrd.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

type Env struct {
	t    testing.TB
	Bin  string
	Home string
	Vars map[string]string
	Path []string
}

func NewEnv(t testing.TB) *Env {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{t: t, Bin: Binary(t), Home: home, Vars: map[string]string{}}
	e.Path = []string{filepath.Dir(e.Bin)}
	t.Cleanup(e.killRuns)
	return e
}

// killRuns stops any session a test left running, so none outlives it,
// and makes read-only workspaces removable again.
func (e *Env) killRuns() {
	defer filepath.WalkDir(e.Home, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0o755)
		}
		return nil
	})
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.Home, ".local", "state", "shephrd", "shephrd.db")+"?mode=ro")
	if err != nil {
		return
	}
	defer db.Close()
	rows, err := db.Query(`SELECT pid FROM runs WHERE pid IS NOT NULL AND liveness != 'exited'`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var pid int
		if rows.Scan(&pid) == nil && pid > 1 {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
}

// StartDaemon runs shephrd daemon until the returned stop or the test ends.
func (e *Env) StartDaemon() func() {
	e.t.Helper()
	cmd := exec.Command(e.Bin, "daemon")
	cmd.Env = e.environ()
	log, err := os.OpenFile(filepath.Join(e.Home, "daemon.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cmd.Process.Signal(syscall.SIGTERM)
			cmd.Wait()
			log.Close()
		})
	}
	e.t.Cleanup(stop)
	e.Eventually("the daemon to hold its lock", func() bool {
		r := e.Run("", "repo", "list")
		return r.Code == 0 && !strings.Contains(r.Stdout, "daemon_not_running")
	})
	return stop
}

// DaemonLog returns what the daemon logged.
func (e *Env) DaemonLog() string {
	body, _ := os.ReadFile(filepath.Join(e.Home, "daemon.log"))
	return string(body)
}

func (e *Env) environ() []string {
	env := []string{"PATH=" + strings.Join(append(e.Path, os.Getenv("PATH")), ":"), "HOME=" + e.Home}
	for key, value := range e.Vars {
		env = append(env, key+"="+value)
	}
	return env
}

// SSHTarget is what one SSH target reaches: a forced command on another
// simulated machine.
type SSHTarget struct {
	Command []string `json:"command"`
	Home    string   `json:"home"`
}

// InstallSSH puts the fake ssh on PATH and declares the targets this
// machine can reach.
func (e *Env) InstallSSH(targets map[string]SSHTarget) {
	e.t.Helper()
	body, err := os.ReadFile(Tool(e.t, "./internal/testkit/fakessh", "fakessh"))
	if err != nil {
		e.t.Fatal(err)
	}
	dir := filepath.Join(e.Home, "sshbin")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "ssh"), body, 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.Path = append([]string{dir}, e.Path...)
	encoded, _ := json.Marshal(targets)
	os.MkdirAll(filepath.Join(e.Home, ".fakessh", "down"), 0o755)
	os.MkdirAll(filepath.Join(e.Home, ".fakessh", "drop"), 0o755)
	if err := os.WriteFile(filepath.Join(e.Home, ".fakessh", "targets.json"), encoded, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// SSHDown makes a target unreachable from this machine, or reachable again.
func (e *Env) SSHDown(target string, down bool) {
	path := filepath.Join(e.Home, ".fakessh", "down", target)
	if down {
		os.WriteFile(path, nil, 0o600)
	} else {
		os.Remove(path)
	}
}

// SSHDropOnce makes the next connection to a target drop after its command ran.
func (e *Env) SSHDropOnce(target string) {
	os.WriteFile(filepath.Join(e.Home, ".fakessh", "drop", target), nil, 0o600)
}

// InstallHarnesses puts the fake harness on PATH as claude, codex and pi.
func (e *Env) InstallHarnesses() {
	e.t.Helper()
	body, err := os.ReadFile(Tool(e.t, "./internal/testkit/fakeharness", "fakeharness"))
	if err != nil {
		e.t.Fatal(err)
	}
	dir := filepath.Join(e.Home, "fakebin")
	os.MkdirAll(dir, 0o755)
	for _, name := range []string{"claude", "codex", "pi"} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o755); err != nil {
			e.t.Fatal(err)
		}
	}
	e.Path = append([]string{dir}, e.Path...)
}

// Script sets what the fake harness does on each run of each task.
func (e *Env) Script(script map[string][][]map[string]any) {
	e.t.Helper()
	body, err := json.Marshal(script)
	if err != nil {
		e.t.Fatal(err)
	}
	dir := filepath.Join(e.Home, ".fakeharness")
	os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, "script.json"), body, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// Calls returns what the fake harness recorded, in order.
func (e *Env) Calls() []map[string]any {
	body, _ := os.ReadFile(filepath.Join(e.Home, ".fakeharness", "calls.jsonl"))
	var calls []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var call map[string]any
		if json.Unmarshal([]byte(line), &call) == nil {
			calls = append(calls, call)
		}
	}
	return calls
}

// Eventually polls until cond holds, failing the test after a timeout.
func (e *Env) Eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	var summary []string
	for _, call := range e.Calls() {
		delete(call, "env")
		delete(call, "brief")
		line, _ := json.Marshal(call)
		summary = append(summary, string(line))
	}
	e.t.Fatalf("timed out waiting for %s\nharness calls:\n%s", what, strings.Join(summary, "\n"))
}

// Task returns a task as task show reports it.
func (e *Env) Task(ref string, as ...string) map[string]any {
	e.t.Helper()
	return e.OK(append([]string{"task", "show", ref}, as...)...)["task"].(map[string]any)
}

// WaitState waits until a task reaches a state, and its run has exited.
func (e *Env) WaitState(ref, state string) map[string]any {
	e.t.Helper()
	var task map[string]any
	e.Eventually(ref+" "+state, func() bool {
		shown := e.Run("", "task", "show", ref)
		if shown.Code != 0 {
			return false
		}
		task = shown.JSON(e.t)["task"].(map[string]any)
		if task["state"] != state {
			return false
		}
		log := e.Run("", "task", "log", ref)
		if log.Code != 0 {
			return true
		}
		run := log.JSON(e.t)["run"].(map[string]any)
		return run["liveness"] == "exited"
	})
	return task
}

func (e *Env) ConfigPath() string {
	return filepath.Join(e.Home, ".config", "shephrd", "config.toml")
}

type Result struct {
	Code   int
	Stdout string
	Stderr string
}

func (e *Env) Run(stdin string, args ...string) Result {
	e.t.Helper()
	cmd := exec.Command(e.Bin, args...)
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if exit, ok := err.(*exec.ExitError); ok {
		result.Code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatalf("run shephrd %s: %v", strings.Join(args, " "), err)
	}
	return result
}

func (e *Env) OK(args ...string) map[string]any {
	e.t.Helper()
	r := e.Run("", args...)
	if r.Code != 0 {
		e.t.Fatalf("shephrd %s: exit %d\n%s", strings.Join(args, " "), r.Code, r.Stderr)
	}
	return r.JSON(e.t)
}

func (e *Env) Refused(kind string, args ...string) map[string]any {
	e.t.Helper()
	r := e.Run("", args...)
	return r.Refusal(e.t, kind)
}

func (r Result) JSON(t testing.TB) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, r.Stdout)
	}
	return out
}

func (r Result) Refusal(t testing.TB, kind string) map[string]any {
	t.Helper()
	if r.Code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", r.Code, r.Stdout, r.Stderr)
	}
	if r.Stdout != "" {
		t.Fatalf("refusal wrote stdout: %s", r.Stdout)
	}
	var out struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.Stderr), &out); err != nil || out.Error == nil {
		t.Fatalf("stderr is not an error envelope: %v\n%s", err, r.Stderr)
	}
	if out.Error["kind"] != kind {
		t.Fatalf("error kind %v, want %s: %s", out.Error["kind"], kind, r.Stderr)
	}
	return out.Error
}

// WritePlugin writes a plugin directory whose executable is bin/fake, the
// scriptable fake plugin, followed by the given manifest lines.
func WritePlugin(t testing.TB, dir, name, manifest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(Tool(t, "./internal/testkit/fakeplugin", "fakeplugin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "fake"), body, 0o755); err != nil {
		t.Fatal(err)
	}
	head := "name = \"" + name + "\"\nversion = \"1.0.0\"\nprotocol = 1\nexec = [\"bin/fake\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(head+manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *Env) AppendConfig(text string) {
	e.t.Helper()
	file, err := os.OpenFile(e.ConfigPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("\n" + text + "\n"); err != nil {
		e.t.Fatal(err)
	}
}
