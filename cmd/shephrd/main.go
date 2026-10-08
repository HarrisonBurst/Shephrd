package main

import (
	"encoding/json"
	"fmt"
	"os"

	"shephrd/internal/cli"
)

func main() {
	if err := cli.New().Execute(); err != nil {
		if cli.IsJSON(os.Args[1:]) {
			response := map[string]any{"error": err.Error()}
			if kind := cli.ErrorKind(err); kind != "" {
				response["error_kind"] = kind
			}
			for key, value := range cli.ErrorEvidence(err) {
				if key != "error" && key != "error_kind" {
					response[key] = value
				}
			}
			_ = json.NewEncoder(os.Stderr).Encode(response)
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}
