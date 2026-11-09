package main

import (
	"context"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

// agentEventTimeout bounds how long a hook can hold the agent up.
const agentEventTimeout = 3 * time.Second

// newAgentEventCmd is what an agent's hooks run to report its state. It must never
// fail or stall the agent: some hook events treat a non-zero exit as "block this
// action", so every problem (no daemon, a bad argument, a missing variable) ends in
// exit 0 and silence.
func newAgentEventCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "agent-event <ready|working|blocked|idle|exited>",
		Short: "Report an agent's state (run by agent hooks)",
		Long: `Tell the daemon what the agent of $RIG_TICKET/$RIG_SERVICE is doing. Agent hooks run
this; it always exits 0 and never starts the daemon, so a hook cannot break the agent.`,
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ticket, service := os.Getenv("RIG_TICKET"), os.Getenv("RIG_SERVICE")
			if len(args) != 1 || ticket == "" || service == "" {
				return nil
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), agentEventTimeout)
			defer cancel()
			c, err := a.connect(ctx, false)
			if err != nil {
				return nil
			}
			defer c.Close()
			_ = c.Call(ctx, api.MethodAgentEvent, api.AgentEventParams{Ticket: ticket, Service: service, State: args[0]}, nil)
			return nil
		},
	}
}
