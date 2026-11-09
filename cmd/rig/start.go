package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newStartCmd(a *app) *cobra.Command {
	var sandbox string
	cmd := &cobra.Command{
		Use:   "start <ticket-id>",
		Short: "Provision a ticket's checkouts",
		Long: `Provision one checkout of each service's repository inside the ticket directory:
an isolated clone for services whose sandbox is "container" (the default), a git
worktree for "host". Starting is safe to repeat: checkouts that exist are kept.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.TicketStartParams{ID: args[0]}
			if sandbox != "" {
				if err := p.Sandbox.UnmarshalText([]byte(sandbox)); err != nil {
					return usageError(fmt.Errorf("--sandbox: %w", err))
				}
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.TicketStartResult
			if err := c.Call(cmd.Context(), api.MethodTicketStart, p, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tCHECKOUT\tBRANCH\tSTATUS\tPATH")
			for _, co := range res.Checkouts {
				note := "existing"
				if co.Created {
					note = "created"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", co.Service, co.Kind, co.Branch, note, co.Path)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&sandbox, "sandbox", "", "override the sandbox for every service: container or host")
	return cmd
}
