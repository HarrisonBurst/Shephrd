package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"shephrd/internal/fault"
	"shephrd/internal/gitcmd"
	"shephrd/internal/plugin"
	"shephrd/internal/proc"
	"shephrd/internal/version"
)

type (
	VerifyRequest struct {
		Repo   string `json:"repo"`
		Path   string `json:"path"`
		Branch string `json:"branch"`
		Base   string `json:"base"`
	}
	SetupRequest struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		Log     string `json:"log"`
	}
	ReadOnlyRequest struct {
		Path     string `json:"path"`
		ReadOnly bool   `json:"read_only"`
	}
	LaunchRequest struct {
		RunDir string  `json:"run_dir"`
		Brief  string  `json:"brief"`
		Spec   RunSpec `json:"spec"`
		Token  string  `json:"token"`
	}
	RunDirRequest struct {
		RunDir string `json:"run_dir"`
		Limit  int64  `json:"limit,omitempty"`
	}
	LogResponse struct {
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	}
	ExitResponse struct {
		Record ExitRecord `json:"record"`
		Found  bool       `json:"found"`
	}
	StopRequest struct {
		PGID int `json:"pgid"`
	}
	RemoveRequest struct {
		Repo   string   `json:"repo"`
		Path   string   `json:"path"`
		Branch string   `json:"branch"`
		Base   string   `json:"base"`
		Force  bool     `json:"force"`
		Sealed []string `json:"sealed"`
	}
	PinRequest struct {
		Dir   string            `json:"dir"`
		Files map[string][]byte `json:"files"`
	}
	InspectRequest struct {
		Path          string `json:"path"`
		DefaultBranch string `json:"default_branch"`
	}
	InspectResponse struct {
		Path          string `json:"path"`
		DefaultBranch string `json:"default_branch"`
	}
	AncestryRequest struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		Commit string `json:"commit"`
	}
	StateResponse struct {
		State string `json:"state"`
	}
	PushRequest struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		Commit string `json:"commit"`
	}
	PushResponse struct {
		Remote string `json:"remote"`
	}
)

func decode[T any](body []byte) (T, error) {
	var req T
	if err := json.Unmarshal(body, &req); err != nil {
		return req, fault.New("usage", "malformed host operation: %v", err)
	}
	return req, nil
}

// handle runs one host operation on this machine.
func handle(ctx context.Context, env HostEnv, op string, body []byte) (any, error) {
	switch op {
	case "info":
		harnesses := []string{}
		for name, binary := range map[string]string{"claude-code": "claude", "codex": "codex", "pi": "pi"} {
			if _, err := exec.LookPath(binary); err == nil {
				harnesses = append(harnesses, name)
			}
		}
		if env.Plugins != nil {
			for _, p := range env.Plugins.All() {
				if p.Manifest != nil && slices.Contains(p.Manifest.Provide, plugin.Provide{Type: "harness"}) {
					harnesses = append(harnesses, p.Name)
				}
			}
		}
		slices.Sort(harnesses)
		presentations := []string{"headless"}
		if env.Plugins != nil {
			for _, p := range env.Plugins.All() {
				if p.Manifest != nil && slices.Contains(p.Manifest.Provide, plugin.Provide{Type: "presentation"}) {
					presentations = append(presentations, p.Name)
				}
			}
		}
		return HostInfo{Host: env.Host, Version: version.String(), Protocol: version.Protocol, DataDir: env.DataDir,
			Harnesses: harnesses, Presentations: presentations, Presentation: env.Presentation, Shephrd: env.Self}, nil
	case "harness":
		req, err := decode[HarnessCall](body)
		if err != nil {
			return nil, err
		}
		if env.Harness == nil {
			return nil, fault.New("unknown_harness", "host %s resolves no harnesses", env.Host)
		}
		return env.Harness(ctx, req.Name, req.Request)
	case "prepare_workspace":
		spec, err := decode[WorkspaceSpec](body)
		if err != nil {
			return nil, err
		}
		base, err := PrepareWorkspace(spec)
		return map[string]string{"base": base}, err
	case "verify_workspace":
		req, err := decode[VerifyRequest](body)
		if err != nil {
			return nil, err
		}
		return struct{}{}, VerifyWorkspace(req.Repo, req.Path, req.Branch, req.Base)
	case "setup":
		req, err := decode[SetupRequest](body)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(req.Log), 0o700); err != nil {
			return nil, err
		}
		return struct{}{}, RunSetup(req.Path, req.Command, req.Log, []string{"PATH=" + env.Path, "HOME=" + env.Home})
	case "read_only":
		req, err := decode[ReadOnlyRequest](body)
		if err != nil {
			return nil, err
		}
		return struct{}{}, SetReadOnly(req.Path, req.ReadOnly)
	case "launch":
		req, err := decode[LaunchRequest](body)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(req.RunDir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(BriefPath(req.RunDir), []byte(req.Brief), 0o600); err != nil {
			return nil, err
		}
		req.Spec.Shephrd, req.Spec.Config, req.Spec.Path, req.Spec.Home, req.Spec.Account = env.Self, env.Config, env.Path, env.Home, env.Account
		if env.Presentation == "" || env.Presentation == "headless" {
			id, err := Launch(req.RunDir, req.Spec, req.Token)
			return LaunchResponse{PID: id.PID, Start: id.Start}, err
		}
		endpoint, err := present(ctx, env, req)
		return LaunchResponse{Endpoint: endpoint}, err
	case "presentation":
		req, err := decode[PresentationRequest](body)
		if err != nil {
			return nil, err
		}
		return presentation(ctx, env, req)
	case "probe":
		id, err := decode[proc.Identity](body)
		if err != nil {
			return nil, err
		}
		return StateResponse{State: id.Probe()}, nil
	case "stop":
		req, err := decode[StopRequest](body)
		if err != nil {
			return nil, err
		}
		state := "unknown"
		if proc.StopGroup(req.PGID, 5*time.Second) {
			state = "exited"
		}
		return StateResponse{State: state}, nil
	case "exit_record":
		req, err := decode[RunDirRequest](body)
		if err != nil {
			return nil, err
		}
		record, found := ReadExit(req.RunDir)
		return ExitResponse{Record: record, Found: found}, nil
	case "read_log":
		req, err := decode[RunDirRequest](body)
		if err != nil {
			return nil, err
		}
		text, truncated, err := ReadLog(req.RunDir, req.Limit)
		return LogResponse{Text: text, Truncated: truncated}, err
	case "remove_workspace":
		req, err := decode[RemoveRequest](body)
		if err != nil {
			return nil, err
		}
		return struct{}{}, RemoveWorkspace(req.Repo, req.Path, req.Branch, req.Base, req.Force, req.Sealed)
	case "classify":
		req, err := decode[VerifyRequest](body)
		if err != nil {
			return nil, err
		}
		return StateResponse{State: classify(req)}, nil
	case "pin_inputs":
		req, err := decode[PinRequest](body)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(req.Dir, 0o755); err != nil {
			return nil, err
		}
		for name, content := range req.Files {
			target := filepath.Join(req.Dir, filepath.Base(name))
			os.Chmod(target, 0o644)
			if err := os.WriteFile(target, content, 0o444); err != nil {
				return nil, err
			}
		}
		return struct{}{}, nil
	case "inspect_repo":
		req, err := decode[InspectRequest](body)
		if err != nil {
			return nil, err
		}
		path, branch, err := InspectRepo(req.Path, req.DefaultBranch)
		return InspectResponse{Path: path, DefaultBranch: branch}, err
	case "seal":
		req, err := decode[SealRequest](body)
		if err != nil {
			return nil, err
		}
		return Seal(req)
	case "land_direct":
		req, err := decode[LandRequest](body)
		if err != nil {
			return nil, err
		}
		return LandDirect(req, filepath.Join(env.DataDir, "scratch"))
	case "remote_url":
		req, err := decode[PushRequest](body)
		if err != nil {
			return nil, err
		}
		remote, err := gitcmd.Run(req.Repo, "remote", "get-url", "origin")
		if err != nil {
			return nil, fault.New("landing_failed", "repository %s has no origin", req.Repo)
		}
		return PushResponse{Remote: remote}, nil
	case "push_branch":
		req, err := decode[PushRequest](body)
		if err != nil {
			return nil, err
		}
		remote, err := gitcmd.Run(req.Repo, "remote", "get-url", "origin")
		if err != nil {
			return nil, fault.New("landing_failed", "repository %s has no origin to push to", req.Repo)
		}
		if _, err := gitcmd.Run(req.Repo, "push", "--quiet", "--force", "origin", req.Commit+":refs/heads/"+req.Branch); err != nil {
			return nil, fault.New("landing_failed", "push %s: %v", req.Branch, err)
		}
		return PushResponse{Remote: remote}, nil
	case "prove_ancestry":
		req, err := decode[AncestryRequest](body)
		if err != nil {
			return nil, err
		}
		proven, err := ProveAncestry(req.Repo, req.Branch, req.Commit)
		return map[string]bool{"proven": proven}, err
	}
	return nil, fault.New("usage", "unknown host operation %q", op)
}

func classify(req VerifyRequest) string {
	_, statErr := os.Lstat(req.Path)
	if req.Branch == "" {
		if statErr == nil {
			return "held"
		}
		return "none"
	}
	_, branchErr := gitcmd.Run(req.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+req.Branch)
	if errors.Is(statErr, fs.ErrNotExist) && branchErr != nil {
		return "none"
	}
	if statErr == nil && VerifyWorkspace(req.Repo, req.Path, req.Branch, "") == nil {
		return "held"
	}
	return "unknown"
}
