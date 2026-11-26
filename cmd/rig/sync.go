package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newSyncCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "sync <ticket> [service]",
		Short: "Bring the base branch's new commits into a ticket's branch",
		Long: `Merge the base branch (as it is in the main repository) into each service's branch.
A clean merge is committed, the agent is told, and a reviewed ticket starts a new review
round on it. A conflict is left in the checkout for the agent to resolve, with the files
named. A ticket in test or review goes back to implement. rig list shows when a base has
moved on ("behind main 5, conflicts 2"), and rig push refuses a branch that would conflict.`,
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.SyncParams{ID: args[0]}
			if len(args) == 2 {
				p.Service = args[1]
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.SyncResult
			if err := c.Call(cmd.Context(), api.MethodTicketSync, p, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tBASE\tBEHIND\tRESULT")
			for _, s := range res.Services {
				result := "up to date"
				switch {
				case len(s.Conflicts) > 0:
					result = "conflicts, left to the agent: " + strings.Join(s.Conflicts, ", ")
				case s.Merged != "":
					result = "merged " + s.Merged[:min(12, len(s.Merged))]
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.Service, s.Base, s.Behind, result)
			}
			tw.Flush()
			if res.ReviewStarted {
				fmt.Fprintln(cmd.OutOrStdout(), "a review round started on the merged work")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "phase: %s\n", res.Phase)
			return nil
		},
	}
}
