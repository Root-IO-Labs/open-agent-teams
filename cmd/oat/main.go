package main

import (
	"fmt"
	"os"

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

	return c.Execute(os.Args[1:])
}
