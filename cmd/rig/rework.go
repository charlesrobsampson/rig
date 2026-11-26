package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newReworkCmd(a *app) *cobra.Command {
	var finding, lens string
	cmd := &cobra.Command{
		Use:   "rework <ticket-id> --finding <text> [--lens <lens>]",
		Short: "Send a ticket in test, review or push back to implement with a finding",
		Long: `Send a ticket back to implement because you found something the review missed.

The finding is the reason of the move and is typed to the agents, and rig records a
review.missed entry in the ticket's timeline (with --lens, the kind of reviewer that
should have caught it), which is the evidence for which reviewers to add. Allowed from
test, review and push; use rig phase for any other move.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.TransitionResult
			if err := c.Call(cmd.Context(), api.MethodTicketRework, api.ReworkParams{ID: args[0], Finding: finding, Lens: lens}, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s -> %s\n", res.Summary.ID, res.From, res.To)
			return nil
		},
	}
	cmd.Flags().StringVar(&finding, "finding", "", "what the review missed (required)")
	cmd.Flags().StringVar(&lens, "lens", "", "the kind of reviewer that should have caught it, such as security")
	cmd.MarkFlagRequired("finding")
	return cmd
}
