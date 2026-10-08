package main

import (
	"fmt"
	"os"

	"shephrd/internal/driverdelivery/webhook"
)

func main() {
	if err := webhook.Run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
