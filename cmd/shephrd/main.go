package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"shephrd/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}
