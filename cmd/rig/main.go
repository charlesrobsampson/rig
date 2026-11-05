package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "rig",
		Short:         "Run AI coding agents in parallel, each isolated",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}
