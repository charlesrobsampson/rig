package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newPushCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "push <ticket-id> [service]",
		Short: "Publish a ticket's work the way each service's push mode says",
		Long: `Run the push step of a ticket that is in the push phase. Each service is published
by its push mode: none (nothing, just report), push-branch, merge (into the base
branch) or pr (push and open a pull request). Everything runs from your own checkout of
the repository with your own git and gh logins; no credential ever enters a container.

Services pushed by an earlier run are left alone, so after a failure fix the problem and
run it again: only what failed is retried. The ticket moves to done once every service
has been pushed.`,
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.PushRunParams{ID: args[0]}
			if len(args) == 2 {
				p.Service = args[1]
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.PushRunResult
			if err := c.Call(cmd.Context(), api.MethodPushRun, p, &res); err != nil {
				return err
			}
			failed := 0
			for _, s := range res.Services {
				if s.Error != "" {
					failed++
				}
			}
			if a.json {
				if err := printJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			} else {
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "SERVICE\tMODE\tRESULT\tDETAIL")
				for _, s := range res.Services {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Service, s.Mode, pushResultWord(s), pushDetail(s))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				if res.Done {
					fmt.Fprintf(cmd.OutOrStdout(), "\n%s: every service is pushed; the ticket is done\n", p.ID)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d service(s) were not pushed; fix the problem and run rig push %s again (services already pushed are skipped)", failed, p.ID)
			}
			return nil
		},
	}
}

func pushResultWord(s api.PushServiceResult) string {
	switch {
	case s.Error != "":
		return "failed"
	case s.Skipped:
		return "already pushed"
	case s.Pushed:
		return "pushed"
	}
	return "nothing to push"
}

func pushDetail(s api.PushServiceResult) string {
	switch {
	case s.Error != "":
		return s.Error
	case s.PRURL != "":
		return s.PRURL
	case s.Note != "":
		return s.Note
	case s.Ref != "":
		return s.Ref
	}
	return ""
}
