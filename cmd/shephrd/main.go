package main

import (
	"encoding/json"
	"fmt"
	"os"

	"shephrd/internal/cli"
)

func main() {
	err := cli.New().Execute()
	if err == nil {
		return
	}
	if status, ok := cli.ExitStatus(err); ok {
		os.Exit(status)
	}
	if cli.IsJSON(os.Args[1:]) {
		_ = json.NewEncoder(os.Stderr).Encode(cli.ErrorResponse(err))
	} else {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	os.Exit(1)
}
