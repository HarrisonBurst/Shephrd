package testkit

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
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
}

func NewEnv(t testing.TB) *Env {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Env{t: t, Bin: Binary(t), Home: home, Vars: map[string]string{}}
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
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + e.Home}
	for key, value := range e.Vars {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
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
