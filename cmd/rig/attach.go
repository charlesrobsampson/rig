package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newAttachCmd(a *app) *cobra.Command {
	var print bool
	cmd := &cobra.Command{
		Use:   "attach <ticket-id> [service]",
		Short: "Jump to a service's agent in your terminal backend",
		Long: `Open the terminal pane where a service's agent runs. The service can be left out
when the ticket has only one. With --print (or --json) the command is shown instead of run.`,
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.AgentAttachParams{Ticket: args[0]}
			if len(args) == 2 {
				p.Service = args[1]
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.AgentAttachResult
			if err := c.Call(cmd.Context(), api.MethodAgentAttach, p, &res); err != nil {
				return err
			}
			switch {
			case a.json:
				return printJSON(cmd.OutOrStdout(), res)
			case print:
				fmt.Fprintln(cmd.OutOrStdout(), strings.Join(res.Command, " "))
				return nil
			}
			run := a.deps.exec
			if run == nil {
				run = execReplace
			}
			return run(res.Command)
		},
	}
	cmd.Flags().BoolVar(&print, "print", false, "print the command instead of running it")
	return cmd
}

// execReplace hands the terminal to the command, so attach behaves as if it had been
// typed directly.
func execReplace(argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("cannot run %q: %w", argv[0], err)
	}
	return syscall.Exec(path, argv, os.Environ())
}
