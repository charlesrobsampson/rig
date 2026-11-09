package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		Use:   "agent-event <ready|working|blocked|idle|exited|cost>",
		Short: "Report an agent's state (run by agent hooks)",
		Long: `Tell the daemon what the agent of $RIG_TICKET/$RIG_SERVICE is doing. Agent hooks run
this; it always exits 0 and never starts the daemon, so a hook cannot break the agent.

"cost" is the agent's status line: it reads the harness's status JSON on standard input,
reports its running cost (cost.total_cost_usd) when there is one, and prints the amount as
the status line.`,
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ticket, service := os.Getenv("RIG_TICKET"), os.Getenv("RIG_SERVICE")
			if len(args) != 1 || ticket == "" || service == "" {
				return nil
			}
			p := api.AgentEventParams{Ticket: ticket, Service: service, State: args[0]}
			if args[0] == "cost" {
				usd, ok := readStatusCost(cmd.InOrStdin())
				if !ok {
					return nil // no cost in this status update: report nothing, not zero
				}
				p.CostUSD = &usd
				fmt.Fprintf(cmd.OutOrStdout(), "$%.2f\n", usd)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), agentEventTimeout)
			defer cancel()
			c, err := a.connect(ctx, false)
			if err != nil {
				return nil
			}
			defer c.Close()
			_ = c.Call(ctx, api.MethodAgentEvent, p, nil)
			return nil
		},
	}
}

// readStatusCost pulls cost.total_cost_usd out of the status JSON Claude Code feeds its
// status line command. ok is false when the input is not that JSON or has no cost.
func readStatusCost(in io.Reader) (usd float64, ok bool) {
	var status struct {
		Cost *struct {
			TotalCostUSD *float64 `json:"total_cost_usd"`
		} `json:"cost"`
	}
	if err := json.NewDecoder(io.LimitReader(in, 1<<20)).Decode(&status); err != nil || status.Cost == nil || status.Cost.TotalCostUSD == nil {
		return 0, false
	}
	return *status.Cost.TotalCostUSD, true
}
