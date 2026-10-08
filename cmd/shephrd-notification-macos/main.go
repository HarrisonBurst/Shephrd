package main

import (
	"fmt"
	"os"

	"shephrd/internal/notification/macos"
)

func main() {
	if err := macos.Run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
