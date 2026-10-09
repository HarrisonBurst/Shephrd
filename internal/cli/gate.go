package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"shephrd/internal/config"
	domain "shephrd/internal/model"
	"shephrd/internal/wakewatch"
)

const gateMaxInputBytes = 256 * 1024

var gateCommands = []string{
	"repo list", "repo context",
	"subdriver handoff", "subdriver reply", "subdriver inspect", "subdriver request", "subdriver event", "subdriver ls",
	"subdriver diagnose", "subdriver recover", "subdriver resume",
	"task inspect", "task obligations",
	"wake ack",
}

var gateRefusedFlags = []string{"driver-generation", "legacy-coordinator-json", "all-drivers"}

var gateRefusedEnvironment = []string{
	"PI_SESSION_ID", "SHEPHRD_WORKER",
	"SHEPHRD_SUBDRIVER_ID", "SHEPHRD_SUBDRIVER_GENERATION", "SHEPHRD_SUBDRIVER_TOKEN",
	"SHEPHRD_COORDINATOR_ID", "SHEPHRD_COORDINATOR_GENERATION", "SHEPHRD_COORDINATOR_TOKEN",
}

type gatePlan struct {
	path       string
	argv       []string
	help       bool
	stdinIndex int
}

type gateOwnerValue struct {
	pflag.Value
	owner    string
	mismatch bool
}

func (v *gateOwnerValue) Set(value string) error {
	v.mismatch = v.mismatch || value != v.owner
	return v.Value.Set(value)
}

type gateAuditRecord struct {
	Time       time.Time `json:"time"`
	DriverID   string    `json:"driver_id"`
	Command    string    `json:"command"`
	Decision   string    `json:"decision"`
	ExitStatus int       `json:"exit_status"`
	ErrorKind  string    `json:"error_kind,omitempty"`
}

type exitStatus int

func (s exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(s)) }

func ExitStatus(err error) (int, bool) {
	status, ok := err.(exitStatus)
	return int(status), ok
}

func gateCommand() *cobra.Command {
	var owner string
	command := &cobra.Command{
		Use:   "gate",
		Short: "Run one allowlisted command for a remote non-Pi main driver as an SSH forced command",
		Long: `Run exactly one allowlisted Shephrd command for the fixed main driver named by
--driver-id, intended as an SSH authorized_keys forced command. The request is
SSH_ORIGINAL_COMMAND holding one JSON array of strings: the Shephrd argv with an
optional leading "shephrd". Nothing is shell-parsed. The gate forces --json and
the owner, queues sub-driver handoffs, refuses identity- and scope-changing flags,
reads stdin only for --request-file -, passes the command's output and exit
status through, and appends one audit line to <data_dir>/gate/audit.jsonl.`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{standaloneAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if status := runGate(cmd, strings.TrimSpace(owner), os.Getenv("SSH_ORIGINAL_COMMAND")); status != 0 {
				return exitStatus(status)
			}
			return nil
		},
	}
	command.Flags().StringVar(&owner, "driver-id", "", "Non-Pi main driver owner every request runs as")
	return command
}

func runGate(cmd *cobra.Command, owner, request string) int {
	stderr := cmd.ErrOrStderr()
	refuse := func(err error) int {
		writeError(stderr, err)
		return 1
	}
	cfg, err := config.Load()
	if err != nil {
		return refuse(err)
	}
	audit, err := openGateAudit(cfg.DataDir)
	if err != nil {
		return refuse(err)
	}
	defer audit.Close()
	record := gateAuditRecord{DriverID: owner, Decision: "refused", ExitStatus: 1}
	defer func() {
		record.Time = time.Now().UTC()
		body, _ := json.Marshal(record)
		_, _ = audit.Write(append(body, '\n'))
	}()
	refuseRecorded := func(err error) int {
		record.ErrorKind = ErrorKind(err)
		return refuse(err)
	}
	if err := gateOwnerAllowed(owner); err != nil {
		return refuseRecorded(err)
	}
	plan, err := planGate(owner, request)
	record.Command = plan.path
	if err != nil {
		return refuseRecorded(err)
	}
	if plan.stdinIndex > 0 {
		path, err := gateRequestFile(cmd.InOrStdin())
		if err != nil {
			return refuseRecorded(err)
		}
		defer os.Remove(path)
		plan.argv[plan.stdinIndex] = "--request-file=" + path
	}
	record.Decision, record.ExitStatus = "allowed", 0
	child := New()
	child.SetArgs(plan.argv)
	child.SetIn(strings.NewReader(""))
	child.SetOut(cmd.OutOrStdout())
	child.SetErr(stderr)
	if err := child.Execute(); err != nil {
		record.ExitStatus = 1
		writeError(stderr, err)
	}
	return record.ExitStatus
}

func gateOwnerAllowed(owner string) error {
	if owner == "" {
		return domain.Failure("driver_context_required", "--driver-id is required for gate")
	}
	if !wakewatch.Eligible(owner) {
		return domain.Failure("gate_owner_refused", "gate serves non-Pi main drivers only; %s belongs to Pi or a sub-driver", owner)
	}
	for _, name := range gateRefusedEnvironment {
		if os.Getenv(name) != "" {
			return domain.Failure("gate_environment_refused", "gate refuses to run with %s set; it would change the forced owner identity", name)
		}
	}
	return nil
}

func planGate(owner, request string) (gatePlan, error) {
	invalid := func(format string, args ...any) (gatePlan, error) {
		return gatePlan{}, domain.Failure("gate_request_invalid", format, args...)
	}
	if len(request) > gateMaxInputBytes {
		return invalid("request exceeds %d bytes", gateMaxInputBytes)
	}
	var elements []*string
	if err := json.Unmarshal([]byte(request), &elements); err != nil || elements == nil {
		return invalid("request must be one JSON array of strings")
	}
	argv := make([]string, len(elements))
	for index, element := range elements {
		if element == nil {
			return invalid("request must be one JSON array of strings")
		}
		argv[index] = *element
	}
	if len(argv) > 0 && argv[0] == "shephrd" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return invalid("request argv is empty")
	}
	for _, argument := range argv {
		if strings.ContainsRune(argument, 0) {
			return invalid("request argument contains a NUL byte")
		}
	}
	command, _, err := New().Find(argv)
	if err != nil {
		return gatePlan{}, domain.Failure("gate_command_refused", "command is not allowlisted for gate")
	}
	names := strings.Fields(strings.TrimPrefix(command.CommandPath(), "shephrd"))
	plan := gatePlan{path: strings.Join(names, " ")}
	if !slices.Contains(gateCommands, plan.path) || !slices.Equal(argv[:len(names)], names) {
		return plan, domain.Failure("gate_command_refused", "%q is not allowlisted for gate", plan.path)
	}
	command.InitDefaultHelpFlag()
	flags := command.Flags()
	ownerFlag := command.Flag("driver-id")
	ownerValue := &gateOwnerValue{owner: owner}
	if ownerFlag != nil {
		ownerValue.Value = ownerFlag.Value
		ownerFlag.Value = ownerValue
	}
	if err := command.ParseFlags(argv[len(names):]); err != nil {
		return plan, domain.Failure("gate_request_invalid", "%v", err)
	}
	for _, name := range gateRefusedFlags {
		if flags.Changed(name) {
			return plan, domain.Failure("gate_flag_refused", "--%s is not permitted through gate", name)
		}
	}
	if ownerValue.mismatch {
		return plan, domain.Failure("gate_owner_mismatch", "request --driver-id conflicts with the gate owner")
	}
	if plan.help, _ = flags.GetBool("help"); plan.help {
		plan.argv = append(names, "--help", "--json")
		return plan, nil
	}
	args := flags.Args()
	if err := command.ValidateArgs(args); err != nil {
		return plan, domain.Failure("gate_request_invalid", "%v", err)
	}
	if ownerFlag != nil {
		_ = flags.Set("driver-id", owner)
	}
	_ = flags.Set("json", "true")
	if plan.path == "subdriver handoff" {
		_ = flags.Set("queue", "true")
	}
	plan.argv = slices.Clone(names)
	flags.Visit(func(flag *pflag.Flag) {
		if flag.Name == "request-file" && flag.Value.String() == "-" {
			plan.stdinIndex = len(plan.argv)
		}
		plan.argv = append(plan.argv, "--"+flag.Name+"="+flag.Value.String())
	})
	if len(args) > 0 {
		plan.argv = append(append(plan.argv, "--"), args...)
	}
	return plan, nil
}

func gateRequestFile(stdin io.Reader) (string, error) {
	file, err := os.CreateTemp("", "shephrd-gate-request-*")
	if err != nil {
		return "", err
	}
	written, err := io.Copy(file, io.LimitReader(stdin, gateMaxInputBytes+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil && written > gateMaxInputBytes {
		err = domain.Failure("gate_request_invalid", "stdin request file exceeds %d bytes", gateMaxInputBytes)
	}
	if err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

func openGateAudit(dataDir string) (*os.File, error) {
	dir := filepath.Join(dataDir, "gate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create gate audit directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "audit.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open gate audit log: %w", err)
	}
	return file, nil
}

func writeError(w io.Writer, err error) {
	_ = json.NewEncoder(w).Encode(ErrorResponse(err))
}
