package main

import (
	"context"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/client"
	"github.com/charlesrobsampson/rig/internal/clock"
)

var version = "dev"

// deps are the seams tests replace: how the daemon gets started, and the clock.
type deps struct {
	spawner client.Spawner // nil means start a detached `rig daemon run`
	clock   clock.Clock    // nil means the real clock
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, deps{}))
}

// run executes one rig invocation and returns its exit code (docs/formats.md §10).
func run(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	root := newRootCmd(d)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		printError(stderr, err)
		return exitCode(err)
	}
	return 0
}

type app struct {
	deps       deps
	configPath string
	json       bool
}

func newRootCmd(d deps) *cobra.Command {
	a := &app{deps: d}
	root := &cobra.Command{
		Use:           "rig",
		Short:         "Run AI coding agents in parallel, each isolated",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          usageArgs(cobra.NoArgs),
		RunE:          func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })
	root.PersistentFlags().StringVar(&a.configPath, "config", "", "global config file (default $RIG_CONFIG, then ~/.config/rig/config.json)")
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print machine-readable JSON")
	root.AddCommand(newConfigCmd(a), newDaemonCmd(a), newListCmd(a), newEventsCmd(a), newNewCmd(a))
	return root
}
