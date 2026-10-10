package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/execution"
	"shephrd/internal/fault"
	"shephrd/internal/plugin"
	"shephrd/internal/store"
)

const Protocol = 1

type Request struct {
	V         int       `json:"v"`
	Argv      []string  `json:"argv"`
	Key       string    `json:"key"`
	RunToken  string    `json:"run_token,omitempty"`
	CallToken string    `json:"call_token,omitempty"`
	Stdin     io.Reader `json:"-"`
}

type Warning struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type Response struct {
	Result   json.RawMessage
	Warnings []Warning
	Streamed bool
}

func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv config.Getenv) int {
	if len(args) == 2 && args[0] == "_run" {
		return execution.Supervise(args[1])
	}
	if isHelp(args) {
		root := newRoot(nil)
		root.SetArgs(args)
		root.SetOut(stdout)
		root.SetErr(stderr)
		if err := root.Execute(); err != nil {
			return writeError(stderr, usage(err))
		}
		return 0
	}
	argv, as, key, err := extractEnvelopeFlags(args)
	if err != nil {
		return writeError(stderr, err)
	}
	if key == "" {
		key = uuid.NewString()
	}
	req := Request{V: Protocol, Argv: argv, Key: key, RunToken: getenv("SHEPHRD_RUN_TOKEN"), CallToken: getenv("SHEPHRD_CALL_TOKEN"), Stdin: stdin}
	return Run(ctx, Origin{Local: true, As: as}, req, getenv, stdout, stderr)
}

func Run(ctx context.Context, origin Origin, req Request, getenv config.Getenv, stdout, stderr io.Writer) int {
	resp, err := Dispatch(ctx, origin, req, getenv, stdout)
	if err != nil {
		return writeError(stderr, err)
	}
	if resp.Streamed {
		return 0
	}
	return writeResponse(stdout, resp)
}

func Dispatch(ctx context.Context, origin Origin, req Request, getenv config.Getenv, stream io.Writer) (Response, error) {
	if req.V != Protocol {
		return Response{}, fault.New("version_mismatch", "request protocol %d, this release speaks %d", req.V, Protocol)
	}
	if isHelp(req.Argv) {
		return Response{}, fault.New("usage", "help is rendered by the local binary")
	}
	if req.Key == "" || len(req.Key) > 128 {
		return Response{}, fault.New("usage", "a request needs a key of 1 to 128 bytes")
	}
	if req.Stdin == nil {
		req.Stdin = strings.NewReader("")
	}
	for _, arg := range req.Argv {
		if arg == "--as" || strings.HasPrefix(arg, "--as=") || arg == "--key" || strings.HasPrefix(arg, "--key=") {
			return Response{}, fault.New("usage", "%s belongs to the request envelope, not argv", strings.SplitN(arg, "=", 2)[0])
		}
	}
	a := &app{ctx: ctx, origin: origin, req: req, getenv: getenv, stream: stream}
	defer a.close()
	root := newRoot(a)
	if len(req.Argv) > 0 && !strings.HasPrefix(req.Argv[0], "-") && !slices.Contains(a.reserved, req.Argv[0]) {
		result, err := a.pluginCommand(req.Argv[0], req.Argv[1:])
		if err != nil {
			return Response{}, fault.As(err)
		}
		a.daemonWarning()
		return Response{Result: result, Warnings: a.warnings}, nil
	}
	root.SetArgs(req.Argv)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.ExecuteContext(ctx); err != nil {
		var fe *fault.Error
		if errors.As(err, &fe) {
			return Response{}, fe
		}
		return Response{}, usage(err)
	}
	if a.streamed {
		return Response{Streamed: true}, nil
	}
	a.daemonWarning()
	if a.result == nil {
		return Response{}, fault.New("usage", "%q needs a subcommand", strings.Join(req.Argv, " "))
	}
	return Response{Result: a.result, Warnings: a.warnings}, nil
}

type app struct {
	ctx      context.Context
	origin   Origin
	req      Request
	getenv   config.Getenv
	cfg      *config.Config
	caller   *Caller
	db       *store.Store
	plugins  *plugin.Registry
	reserved []string
	stdin    []byte
	stdinUse bool
	stream   io.Writer
	streamed bool
	result   json.RawMessage
	warnings []Warning
}

func (a *app) close() {
	if a.db != nil {
		a.db.Close()
	}
}

func (a *app) config() (*config.Config, error) {
	if a.cfg == nil {
		cfg, err := config.Load(a.getenv)
		if err != nil {
			return nil, err
		}
		a.cfg = &cfg
	}
	return a.cfg, nil
}

func (a *app) store() (*store.Store, error) {
	if a.db == nil {
		cfg, err := a.config()
		if err != nil {
			return nil, err
		}
		if a.db, err = store.Open(a.ctx, cfg.Store); err != nil {
			return nil, err
		}
	}
	return a.db, nil
}

func (a *app) registry() (*plugin.Registry, error) {
	if a.plugins == nil {
		cfg, err := a.config()
		if err != nil {
			return nil, err
		}
		a.plugins = plugin.Load(cfg, a.reserved)
	}
	return a.plugins, nil
}

func (a *app) gate(point string, task int64, action any) error {
	reg, err := a.registry()
	if err != nil {
		return err
	}
	err = reg.Gate(a.ctx, point, action, a.getenv)
	blocked, ok := plugin.AsBlocked(err)
	if !ok {
		return err
	}
	caller, err := a.identity()
	if err != nil {
		return err
	}
	db, err := a.store()
	if err != nil {
		return err
	}
	if err := db.Write(a.ctx, func(tx *store.Tx) error {
		_, err := tx.Emit("plugin.blocked", caller.String(), task, 0, 0, map[string]string{
			"plugin": blocked.Plugin, "point": blocked.Point, "reason": blocked.Reason,
		})
		return err
	}); err != nil {
		return err
	}
	return blocked.Fault()
}

func (a *app) run(fn func(cmd *cobra.Command, args []string) (any, error)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		value, err := fn(cmd, args)
		if err != nil {
			return fault.As(err)
		}
		if raw, ok := value.(json.RawMessage); ok {
			a.result = raw
			return nil
		}
		a.result, err = json.Marshal(value)
		return err
	}
}

func (a *app) digest() string {
	digest := sha256.New()
	json.NewEncoder(digest).Encode(a.req.Argv)
	digest.Write(a.stdin)
	return hex.EncodeToString(digest.Sum(nil))
}

// mutate runs a mutation in one transaction, idempotent by key. A run's
// mutations count only while it is its task's current run.
func (a *app) mutate(fn func(tx *store.Tx, caller Caller) (any, error)) (any, error) {
	caller, err := a.identity()
	if err != nil {
		return nil, err
	}
	db, err := a.store()
	if err != nil {
		return nil, err
	}
	if err := a.requireCurrentRun(caller, strings.Join(a.req.Argv, " ")); err != nil {
		return nil, err
	}
	return db.Mutate(a.ctx, caller.Scope(), a.req.Key, a.digest(), func(tx *store.Tx) (any, error) {
		if caller.Kind == "run" {
			if err := coord.RequireCurrent(tx, caller); err != nil {
				return nil, err
			}
		}
		return fn(tx, caller)
	})
}

// once makes a command with external effects idempotent by key: a repeat
// returns the first success's result instead of acting again.
func (a *app) once(fn func(caller Caller) (any, error)) (any, error) {
	caller, err := a.identity()
	if err != nil {
		return nil, err
	}
	db, err := a.store()
	if err != nil {
		return nil, err
	}
	if err := a.requireCurrentRun(caller, strings.Join(a.req.Argv, " ")); err != nil {
		return nil, err
	}
	digest := a.digest()
	var storedDigest, stored string
	err = db.QueryRowContext(a.ctx, `SELECT digest, result FROM idempotency WHERE caller = ? AND key = ?`, caller.Scope(), a.req.Key).Scan(&storedDigest, &stored)
	switch {
	case err == nil && storedDigest == digest:
		return json.RawMessage(stored), nil
	case err == nil:
		return nil, fault.New("key_conflict", "key %q was already used for a different request", a.req.Key)
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	result, err := fn(caller)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	_, err = db.ExecContext(a.ctx, `INSERT OR IGNORE INTO idempotency (caller, key, digest, result, time) VALUES (?, ?, ?, ?, ?)`,
		caller.Scope(), a.req.Key, digest, string(out), store.Timestamp(time.Now()))
	return json.RawMessage(out), err
}

func (a *app) text(field, value string, limit int) (string, error) {
	if value == "-" {
		if a.stdinUse {
			return "", fault.New("usage", "only one field can be read from stdin")
		}
		a.stdinUse = true
		body, err := io.ReadAll(io.LimitReader(a.req.Stdin, int64(limit)+1))
		if err != nil {
			return "", err
		}
		a.stdin = body
		value = string(body)
	}
	if len(value) > limit {
		return "", fault.New("too_large", "%s is larger than %d bytes", field, limit)
	}
	return value, nil
}

func writeResponse(w io.Writer, resp Response) int {
	out := resp.Result
	if len(resp.Warnings) > 0 {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(resp.Result, &object); err != nil {
			return 1
		}
		object["warnings"], _ = json.Marshal(resp.Warnings)
		out, _ = json.Marshal(object)
	}
	w.Write(append(out, '\n'))
	return 0
}

func writeError(w io.Writer, err error) int {
	fe := fault.As(err)
	body, _ := json.Marshal(map[string]*fault.Error{"error": fe})
	w.Write(append(body, '\n'))
	if fe.Unknown {
		return 3
	}
	return 1
}

func usage(err error) *fault.Error {
	return fault.New("usage", "%s", err.Error()).WithNext("help")
}

func isHelp(args []string) bool {
	if len(args) == 0 || args[0] == "help" {
		return true
	}
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func extractEnvelopeFlags(args []string) (argv []string, as, key string, err error) {
	argv = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		if name != "--as" && name != "--key" {
			argv = append(argv, args[i])
			continue
		}
		if !inline {
			if i+1 == len(args) {
				return nil, "", "", fault.New("usage", "%s needs a value", name)
			}
			i++
			value = args[i]
		}
		if name == "--as" {
			as = value
		} else {
			key = value
		}
	}
	return argv, as, key, nil
}
