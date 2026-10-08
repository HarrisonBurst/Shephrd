package main

import (
	"fmt"
	"os"

	"shephrd/internal/repository/scanner"
)

func main() {
	if err := scanner.Run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
