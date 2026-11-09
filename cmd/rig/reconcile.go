package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newReconcileCmd(a *app) *cobra.Command {
	var fix, yes bool
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Compare state.json with what really exists, and remove orphans",
		Long: `Compare what state.json says exists (checkouts, containers, networks, agent
terminal panes) with what really does, and list every difference as drift. The daemon does
this on its own at start and every minute; this runs it now.

Drift is only reported, never repaired by guessing: a container that is gone stays gone
until you start the ticket again. The one thing rig can remove is an orphan: a container or
network carrying rig's labels that belongs to no ticket, or to a ticket that is stopped.

  --fix   remove the orphans (asks first unless --yes). Containers and networks without
          rig's labels are never touched.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.ReconcileResult
			if err := c.Call(cmd.Context(), api.MethodReconcileRun, api.ReconcileParams{}, &res); err != nil {
				return err
			}
			if fix {
				var orphans []api.DriftItem
				for _, d := range res.Drift {
					if d.Fixable {
						orphans = append(orphans, d)
					}
				}
				if len(orphans) > 0 && !a.json {
					fmt.Fprintln(cmd.ErrOrStderr(), "These rig-labelled objects belong to nothing and would be removed:")
					for _, d := range orphans {
						fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", d.Detail)
					}
					if !yes && !confirm(cmd, fmt.Sprintf("Remove %d orphan(s)? [y/N] ", len(orphans))) {
						return fmt.Errorf("aborted")
					}
				}
				if len(orphans) > 0 {
					if err := c.Call(cmd.Context(), api.MethodReconcileRun, api.ReconcileParams{Fix: true}, &res); err != nil {
						return err
					}
				}
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			writeReconcile(cmd.OutOrStdout(), cmd.ErrOrStderr(), res, fix)
			if len(res.Failures) > 0 {
				return exitError{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "remove orphaned rig containers and networks")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

func writeReconcile(out, errOut io.Writer, res api.ReconcileResult, fixed bool) {
	for _, n := range res.Notes {
		fmt.Fprintf(errOut, "note: %s\n", n)
	}
	if len(res.Fixed) > 0 {
		fmt.Fprintf(out, "removed: %s\n", strings.Join(res.Fixed, ", "))
	}
	for _, f := range res.Failures {
		fmt.Fprintf(errOut, "could not %s\n", f)
	}
	if len(res.Drift) == 0 {
		fmt.Fprintln(out, "no drift: state.json matches what exists")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TICKET\tSERVICE\tWHAT\tDETAIL")
	fixable := 0
	for _, d := range res.Drift {
		svc := d.Service
		if svc == "" {
			svc = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Ticket, svc, d.Kind, d.Detail)
		if d.Fixable {
			fixable++
		}
	}
	tw.Flush()
	if fixable > 0 && !fixed {
		fmt.Fprintf(out, "\n%d orphan(s) can be removed with: rig reconcile --fix\n", fixable)
	}
}
