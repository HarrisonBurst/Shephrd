package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/fault"
)

const maxResponse = 1 << 20

var Timeouts = map[string]time.Duration{
	"intercept":            5 * time.Second,
	"event":                time.Minute,
	"command":              10 * time.Minute,
	"provide.router":       5 * time.Second,
	"provide.harness":      10 * time.Second,
	"provide.presentation": 30 * time.Second,
	"provide.delivery":     30 * time.Second,
	"provide.forge":        2 * time.Minute,
}

type Call struct {
	Kind    string
	Type    string
	Body    any
	Token   string
	Getenv  config.Getenv
	Timeout time.Duration
}

func (r *Registry) Call(ctx context.Context, p *Plugin, call Call) (json.RawMessage, error) {
	if p.Unavailable != "" {
		return nil, fmt.Errorf("unavailable: %s", p.Unavailable)
	}
	exe, err := inside(p.root, p.Dir, p.Manifest.Exec[0])
	if err != nil {
		return nil, err
	}
	timeout := call.Timeout
	if timeout == 0 {
		key := call.Kind
		if call.Kind == "provide" {
			key += "." + call.Type
		}
		timeout = Timeouts[key]
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := json.Marshal(map[string]any{
		"protocol": Protocol,
		"kind":     call.Kind,
		"plugin":   map[string]any{"name": p.Name, "options": options(p.Options)},
		"body":     call.Body,
	})
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, exe, p.Manifest.Exec[1:]...)
	cmd.Dir = p.Dir
	cmd.Env = []string{
		"PATH=" + call.Getenv("PATH"),
		"HOME=" + call.Getenv("HOME"),
		"SHEPHRD_PLUGIN=" + p.Name,
		"SHEPHRD_CONFIG=" + r.cfg.Path,
	}
	cmd.Env = append(cmd.Env, config.Account(call.Getenv)...)
	if call.Token != "" {
		cmd.Env = append(cmd.Env, "SHEPHRD_CALL_TOKEN="+call.Token)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = bytes.NewReader(request)
	stdout := &limitedBuffer{limit: maxResponse}
	cmd.Stdout = stdout
	logFile, err := r.openLog(p.Name)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	cmd.Stderr = logFile
	err = cmd.Run()
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("timed out after %s", timeout)
	case stdout.overflow:
		return nil, fmt.Errorf("response is larger than %d bytes", maxResponse)
	case err != nil:
		return nil, fmt.Errorf("exited: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &object); err != nil {
		return nil, fmt.Errorf("response is not one JSON object: %v", err)
	}
	return stdout.Bytes(), nil
}

func (r *Registry) openLog(name string) (*os.File, error) {
	dir := filepath.Join(r.cfg.DataDir, "plugins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, name+".log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

func (r *Registry) LogPath(name string) string {
	return filepath.Join(r.cfg.DataDir, "plugins", name+".log")
}

func (r *Registry) Gate(ctx context.Context, point string, body any, getenv config.Getenv) error {
	for _, p := range r.Hooks(point) {
		out, err := r.Call(ctx, p, Call{Kind: "intercept", Body: map[string]any{"point": point, "action": body}, Getenv: getenv})
		if err != nil {
			return fault.New("plugin_failed", "plugin %s failed at %s: %v", p.Name, point, err).WithNext("plugin", "status")
		}
		var answer struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(out, &answer); err != nil {
			return fault.New("plugin_failed", "plugin %s answered %s invalidly: %v", p.Name, point, err)
		}
		switch answer.Decision {
		case "allow":
		case "block":
			return &Blocked{Plugin: p.Name, Point: point, Reason: answer.Reason}
		default:
			return fault.New("plugin_failed", "plugin %s answered %s with decision %q", p.Name, point, answer.Decision)
		}
	}
	return nil
}

type Blocked struct {
	Plugin string
	Point  string
	Reason string
}

func (b *Blocked) Error() string {
	return fmt.Sprintf("plugin %s blocked %s: %s", b.Plugin, b.Point, b.Reason)
}

func (b *Blocked) Fault() *fault.Error {
	return fault.New("plugin_blocked", "%s", b.Error())
}

func AsBlocked(err error) (*Blocked, bool) {
	var b *Blocked
	return b, errors.As(err, &b)
}

func options(o map[string]any) map[string]any {
	if o == nil {
		return map[string]any{}
	}
	return o
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.overflow = true
		return 0, errors.New("response too large")
	}
	return b.Buffer.Write(p)
}
