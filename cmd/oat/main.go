package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Root-IO-Labs/open-agent-teams/internal/cli"
	"github.com/Root-IO-Labs/open-agent-teams/internal/errors"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, errors.Format(err))
		os.Exit(1)
	}
}

func run() error {
	c, err := cli.New()
	if err != nil {
		return err
	}

	// Ctrl-C / SIGTERM cancels ctx, which in turn SIGKILLs any in-flight
	// git / gh / tmux subprocess spawned by a CLI command. Without this the
	// user's ^C would only stop the Go code — a long-running `git clone` or
	// `gh run list` would keep running until it exited on its own.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c.SetContext(ctx)

	return c.Execute(os.Args[1:])
}
