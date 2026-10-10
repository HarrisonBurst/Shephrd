package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/execution"
	"shephrd/internal/fault"
	"shephrd/internal/plugin"
	"shephrd/internal/store"
)

const forwardAttempts = 3

// forward sends a request to the home host over SSH and relays its answer.
// A dropped connection is retried with the same key, so a mutation that
// already committed returns its stored result; when every attempt fails
// the outcome is unknown and the exit status is 3.
func forward(ctx context.Context, cfg *config.Config, req Request, as string, stdin io.Reader, stdout, stderr io.Writer, getenv config.Getenv) int {
	if as != "" {
		return writeError(stderr, fault.New("as_not_allowed", "over SSH the caller's identity comes from its key; --as is refused"))
	}
	var input []byte
	if slices.Contains(req.Argv, "-") {
		var err error
		if input, err = io.ReadAll(io.LimitReader(stdin, 2<<20)); err != nil {
			return writeError(stderr, err)
		}
	}
	envelope, err := json.Marshal(Request{V: req.V, Argv: req.Argv, Key: req.Key, RunToken: req.RunToken})
	if err != nil {
		return writeError(stderr, err)
	}
	streaming := len(req.Argv) > 0 && (req.Argv[0] == "events" && slices.Contains(req.Argv, "--follow") ||
		req.Argv[0] == "inbox" && len(req.Argv) > 1 && req.Argv[1] == "wait")
	var detail string
	for attempt := 0; attempt < forwardAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return writeError(stderr, &fault.Error{Kind: "transport_failed", Message: "interrupted", Unknown: true})
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", cfg.Home, string(envelope))
		cmd.Env = append([]string{"PATH=" + getenv("PATH"), "HOME=" + getenv("HOME")}, config.Account(getenv)...)
		if sock := getenv("SSH_AUTH_SOCK"); sock != "" {
			cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+sock)
		}
		cmd.Stdin = bytes.NewReader(input)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if streaming {
			cmd.Stdout = stdout
		}
		err := cmd.Run()
		code := 0
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			code = exit.ExitCode()
		case err != nil:
			code = 255
		}
		if code != 255 {
			stdout.Write(out.Bytes())
			stderr.Write(errOut.Bytes())
			return code
		}
		detail = strings.TrimSpace(errOut.String())
		if streaming {
			break
		}
	}
	return writeError(stderr, &fault.Error{Kind: "transport_failed", Unknown: true,
		Message: "could not reach the home host " + cfg.Home + ": " + detail + "; the request may have taken effect. Retry with the same --key, or read the state"})
}

// serve is the SSH forced command on the home host. The key fixes who the
// caller is: --as for a driver or plugin, --host for sessions running on a
// worker host, which identify themselves with their run token.
func serve(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv config.Getenv) int {
	origin := Origin{}
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--as":
			origin.As = args[i+1]
		case "--host":
			origin.Host = args[i+1]
		default:
			return writeError(stderr, fault.New("usage", "serve takes --as driver:<name>, --as plugin:<name> or --host <name>"))
		}
	}
	if (origin.As == "") == (origin.Host == "") || len(args)%2 != 0 {
		return writeError(stderr, fault.New("usage", "serve takes exactly one of --as or --host"))
	}
	var req Request
	if err := json.Unmarshal([]byte(getenv("SSH_ORIGINAL_COMMAND")), &req); err != nil {
		return writeError(stderr, fault.New("usage", "the SSH command must be one Shephrd request: %v", err))
	}
	req.CallToken, req.Stdin = "", stdin
	resp, err := Dispatch(ctx, origin, req, getenv, stdout)
	if err != nil {
		refused(ctx, origin, req, err, getenv)
		return writeError(stderr, err)
	}
	if resp.Streamed {
		return 0
	}
	return writeResponse(stdout, resp)
}

// refused records a refused remote request: who, which command, and why,
// never its arguments or input.
func refused(ctx context.Context, origin Origin, req Request, err error, getenv config.Getenv) {
	cfg, cfgErr := config.Load(getenv)
	if cfgErr != nil {
		return
	}
	db, dbErr := store.Open(ctx, cfg.Store)
	if dbErr != nil {
		return
	}
	defer db.Close()
	caller := origin.As
	if caller == "" {
		caller = "host:" + origin.Host
	}
	path := []string{}
	for _, arg := range req.Argv {
		if strings.HasPrefix(arg, "-") || len(path) == 2 {
			break
		}
		path = append(path, arg)
	}
	db.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Emit("request.refused", caller, 0, 0, 0, map[string]string{
			"caller": caller, "command": strings.Join(path, " "), "kind": fault.As(err).Kind,
		})
		return err
	})
}

// agent is the SSH forced command on a worker host: host operations only.
func agent(ctx context.Context, stdin io.Reader, stdout io.Writer, getenv config.Getenv) int {
	cfg, err := config.Load(getenv)
	if err != nil {
		json.NewEncoder(stdout).Encode(map[string]any{"error": fault.As(err)})
		return 0
	}
	env, err := execution.LocalEnv(&cfg, getenv)
	if err != nil {
		json.NewEncoder(stdout).Encode(map[string]any{"error": fault.As(err)})
		return 0
	}
	reg := plugin.Load(&cfg, reservedNames())
	env.Harness, env.Plugins = execution.Harnesses(reg, getenv), reg
	return execution.Agent(ctx, env, getenv("SSH_ORIGINAL_COMMAND"), stdin, stdout)
}

func reservedNames() []string {
	a := &app{}
	newRoot(a)
	return a.reserved
}
