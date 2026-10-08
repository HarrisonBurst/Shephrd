package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"shephrd/internal/adapter"
)

const protocolInputMaxBytes = 256 * 1024

func protocolCommand() *cobra.Command {
	command := commandGroup("protocol", "Check authored event output without loading configuration or state")
	var role, file string
	validate := &cobra.Command{
		Use:         "validate",
		Short:       "Read-only format preflight for exact authored envelopes",
		Long:        "Validate authored assistant output, not native harness JSON records. Reads stdin or --file (at most 256 KiB). Uses the ingestion parser, including compound ordering. Success is format-only: no artifact acceptance, notification, input acknowledgement, lifecycle authorization or delivery proof. Current identity, checkpoint freshness and task-specific artifact fences remain ingestion checks. Preflight does not guarantee later model compliance.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{standaloneAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			parse := adapter.ParseEventCandidateSet
			switch role {
			case "worker":
			case "subdriver", "coordinator":
				parse = adapter.ParseSubdriverCandidateSet
			default:
				return fmt.Errorf("--role must be worker or subdriver")
			}
			input := cmd.InOrStdin()
			if file != "-" {
				opened, err := os.Open(file)
				if err != nil {
					return err
				}
				defer opened.Close()
				input = opened
			}
			body, err := io.ReadAll(io.LimitReader(input, protocolInputMaxBytes+1))
			if err != nil {
				return err
			}
			var diagnostic *adapter.Diagnostic
			count := 0
			if len(body) > protocolInputMaxBytes {
				diagnostic = &adapter.Diagnostic{Code: adapter.DiagnosticFramingInvalid, Phase: "preflight", Message: "authored output exceeds 262144 bytes; input was not validated"}
			} else {
				events, found, rejected := parse(string(body))
				diagnostic = rejected
				if !found {
					diagnostic = &adapter.Diagnostic{Code: adapter.DiagnosticFramingInvalid, Phase: "preflight", Message: "no event candidate found; supply exact <shephrd-event> JSON envelopes"}
				}
				count = len(events)
			}
			if diagnostic != nil {
				bounded := diagnostic.Bounded()
				diagnostic = &bounded
			}
			jsonOutput, _ := cmd.Flags().GetBool("json")
			if jsonOutput {
				result := struct {
					Valid      bool                `json:"valid"`
					Scope      string              `json:"scope"`
					Role       string              `json:"role"`
					EventCount int                 `json:"event_count"`
					Diagnostic *adapter.Diagnostic `json:"diagnostic,omitempty"`
				}{diagnostic == nil, "format-only", role, count, diagnostic}
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
					return err
				}
			}
			if diagnostic != nil {
				return fmt.Errorf("%s: %s", diagnostic.Code, diagnostic.Error())
			}
			if !jsonOutput {
				fmt.Fprintf(cmd.OutOrStdout(), "Valid %s format (%d events); no lifecycle action or artifact acceptance.\n", role, count)
			}
			return nil
		},
	}
	validate.Flags().StringVar(&role, "role", "worker", "Envelope contract: worker or subdriver (legacy role: coordinator)")
	validate.Flags().StringVar(&file, "file", "-", "Exact authored output file, or - for stdin")
	command.AddCommand(validate)
	return command
}
