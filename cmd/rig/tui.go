package main

import (
	"context"
	"os"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/tui"
)

// rootRun is bare `rig`: the TUI on a terminal, the same table as `rig list` anywhere
// else (a pipe, a script, --json), so output stays scriptable.
func (a *app) rootRun(cmd *cobra.Command, _ []string) error {
	interactive := !a.json
	if interactive {
		if a.deps.isTerminal != nil {
			interactive = a.deps.isTerminal()
		} else {
			interactive = term.IsTerminal(os.Stdout.Fd()) && term.IsTerminal(os.Stdin.Fd())
		}
	}
	if !interactive {
		return newListCmd(a).RunE(cmd, nil)
	}
	opts := tui.Options{Source: daemonSource{a}, Version: version}
	if a.deps.runTUI != nil {
		return a.deps.runTUI(cmd.Context(), opts)
	}
	return tui.Run(cmd.Context(), opts)
}

// daemonSource reads the fleet from the daemon, starting it when it is not running, so
// bare `rig` works from a cold machine and a daemon that dies comes back.
type daemonSource struct{ a *app }

func (s daemonSource) Tickets(ctx context.Context) ([]api.TicketSummary, error) {
	c, err := s.a.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var res api.TicketListResult
	if err := c.Call(ctx, api.MethodTicketList, nil, &res); err != nil {
		return nil, err
	}
	return res.Tickets, nil
}
