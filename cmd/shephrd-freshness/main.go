package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"shephrd/internal/freshness"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	path, jsonOutput, help, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, "usage: shephrd-freshness [--json] <shephrd-executable>")
		return 2
	}
	if help {
		fmt.Fprintln(stdout, "usage: shephrd-freshness [--json] <shephrd-executable>")
		return 0
	}
	report := freshness.Inspect(path)
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "%s: %s\n", report.Status, report.Reason)
		fmt.Fprintf(stdout, "invoked: %s\nresolved: %s\n", report.InvokedPath, report.ResolvedPath)
		if report.Repository != nil {
			fmt.Fprintf(stdout, "repository: %s\nHEAD: %s dirty=%t\n", report.Repository.Path, report.Repository.Head, report.Repository.Dirty)
		}
	}
	return report.ExitCode
}

func parseArgs(args []string) (string, bool, bool, error) {
	var path string
	var jsonOutput bool
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonOutput = true
		case "-h", "--help":
			if len(args) != 1 {
				return "", false, false, fmt.Errorf("--help cannot be combined with other arguments")
			}
			return "", false, true, nil
		default:
			if strings.HasPrefix(arg, "-") {
				return "", false, false, fmt.Errorf("unknown flag %s", arg)
			}
			if path != "" {
				return "", false, false, fmt.Errorf("expected exactly one executable path")
			}
			path = arg
		}
	}
	if path == "" {
		return "", false, false, fmt.Errorf("executable path is required")
	}
	return path, jsonOutput, false, nil
}
