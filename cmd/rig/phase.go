package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newPhaseCmd(a *app) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "phase <ticket-id> <phase>",
		Short: "Move a ticket to another phase",
		Long: `Move a ticket through plan -> implement -> test -> review -> push -> done.

A forward move goes one step at a time and needs the approvals that guard it (see
rig gates); a move back to any earlier phase needs --reason and makes the approvals for
the work being redone count again. Every move is logged.`,
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.PhaseSetParams{ID: args[0], Reason: reason}
			if err := p.To.UnmarshalText([]byte(args[1])); err != nil {
				return usageError(err)
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.TransitionResult
			if err := c.Call(cmd.Context(), api.MethodPhaseSet, p, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s -> %s\n", res.Summary.ID, res.From, res.To)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why (required when moving backward)")
	return cmd
}
