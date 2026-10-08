package main

import (
	"fmt"
	"os"

	forgegithub "shephrd/internal/forge/github"
)

func main() {
	if err := forgegithub.Run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
