package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"shephrd/internal/config"
	"shephrd/internal/fault"
	"shephrd/internal/plugin"
	"shephrd/internal/version"
)

// A Host runs host operations on one machine: in process on the home
// host, or through `shephrd agent` over SSH on a worker host. Both run the
// same handlers with the same JSON requests and responses.
type Host interface {
	Name() string
	Call(ctx context.Context, op string, req, resp any) error
}

// HostEnv is what a machine knows about itself, including the harness
// providers declared in its own configuration.
type HostEnv struct {
	Host         string
	DataDir      string
	Config       string
	Self         string
	Path         string
	Home         string
	Account      []string
	Harness      func(context.Context, string, HarnessRequest) (HarnessCommand, error)
	Plugins      *plugin.Registry
	Presentation string
	Getenv       config.Getenv
}

type HarnessCall struct {
	Name    string         `json:"name"`
	Request HarnessRequest `json:"request"`
}

type HostInfo struct {
	Host          string   `json:"host"`
	Version       string   `json:"version"`
	Protocol      int      `json:"protocol"`
	DataDir       string   `json:"data_dir"`
	Harnesses     []string `json:"harnesses"`
	Presentations []string `json:"presentations"`
	Presentation  string   `json:"presentation,omitempty"`
	Shephrd       string   `json:"shephrd"`
}

func LocalEnv(cfg *config.Config, getenv config.Getenv) (HostEnv, error) {
	self, err := os.Executable()
	if err != nil {
		return HostEnv{}, err
	}
	return HostEnv{Host: cfg.Host, DataDir: cfg.DataDir, Config: cfg.Path, Self: self, Path: getenv("PATH"), Home: getenv("HOME"), Account: config.Account(getenv),
		Presentation: cfg.Presentation, Getenv: getenv}, nil
}

type Local struct {
	Env HostEnv
}

func (l *Local) Name() string { return l.Env.Host }

func (l *Local) Call(ctx context.Context, op string, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	out, err := handle(ctx, l.Env, op, body)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if resp == nil {
		return nil
	}
	return json.Unmarshal(encoded, resp)
}

type Remote struct {
	Host   string
	Target string
	Getenv config.Getenv
}

func (r *Remote) Name() string { return r.Host }

type agentHeader struct {
	V       int    `json:"v"`
	Version string `json:"version"`
	Op      string `json:"op"`
}

type agentResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *fault.Error    `json:"error,omitempty"`
	State  string          `json:"state,omitempty"`
}

// Call runs one operation on the worker host. The operation is named in
// the SSH command and its request travels on stdin, so nothing secret is
// visible in a process list. An unreachable host is host_unreachable.
func (r *Remote) Call(ctx context.Context, op string, req, resp any) error {
	header, _ := json.Marshal(agentHeader{V: version.Protocol, Version: version.String(), Op: op})
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", r.Target, string(header))
	cmd.Env = append([]string{"PATH=" + r.Getenv("PATH"), "HOME=" + r.Getenv("HOME")}, config.Account(r.Getenv)...)
	if sock := r.Getenv("SSH_AUTH_SOCK"); sock != "" {
		cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+sock)
	}
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return &fault.Error{Kind: "host_unreachable", Message: fmt.Sprintf("host %s (%s): %v: %s", r.Host, r.Target, err, strings.TrimSpace(stderr.String()))}
	}
	var out agentResponse
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return fault.New("host_unreachable", "host %s answered invalidly: %v", r.Host, err)
	}
	if out.Error != nil {
		if out.State != "" {
			return &WorkspaceError{State: out.State, Err: out.Error}
		}
		return out.Error
	}
	if resp == nil {
		return nil
	}
	return json.Unmarshal(out.Result, resp)
}

// Hosts resolves a host name: this machine in process, or a configured
// worker host over SSH.
func Hosts(cfg *config.Config, getenv config.Getenv) func(string) (Host, error) {
	return func(name string) (Host, error) {
		if name == cfg.Host {
			env, err := LocalEnv(cfg, getenv)
			if err != nil {
				return nil, err
			}
			return &Local{Env: env}, nil
		}
		h, ok := cfg.Hosts[name]
		if !ok {
			return nil, fault.New("unknown_host", "host %q is not configured", name)
		}
		return &Remote{Host: name, Target: h.SSH, Getenv: getenv}, nil
	}
}

// Agent is the body of `shephrd agent`, the forced command a worker host
// gives the home host's key. It runs host operations only.
func Agent(ctx context.Context, env HostEnv, command string, stdin io.Reader, stdout io.Writer) int {
	var header agentHeader
	respond := func(result any, err error) int {
		out := agentResponse{}
		if err != nil {
			var werr *WorkspaceError
			if errors.As(err, &werr) {
				out.Error, out.State = werr.Err, werr.State
			} else {
				out.Error = fault.As(err)
			}
		} else if out.Result, err = json.Marshal(result); err != nil {
			out.Error = fault.As(err)
		}
		json.NewEncoder(stdout).Encode(out)
		return 0
	}
	if err := json.Unmarshal([]byte(command), &header); err != nil {
		return respond(nil, fault.New("usage", "the agent takes one host operation"))
	}
	if header.V != version.Protocol || header.Version != version.String() {
		return respond(nil, fault.New("version_mismatch", "home runs %s (protocol %d); host %s runs %s (protocol %d)", header.Version, header.V, env.Host, version.String(), version.Protocol))
	}
	body, err := io.ReadAll(io.LimitReader(stdin, 64<<20))
	if err != nil {
		return respond(nil, err)
	}
	return respond(handle(ctx, env, header.Op, body))
}
