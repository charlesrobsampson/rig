package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func newGatesCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "gates <ticket-id>",
		Short: "Show the approvals a ticket needs",
		Long: `List every approval each service requires before the ticket can advance, and which
have been given. The gate marked "<- needed to leave <phase>" is the one holding the
ticket in its current phase.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.GateListResult
			if err := c.Call(cmd.Context(), api.MethodGateList, api.GateListParams{ID: args[0]}, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			writeGateTable(cmd, res)
			return nil
		},
	}
}

func writeGateTable(cmd *cobra.Command, res api.GateListResult) {
	out := cmd.OutOrStdout()
	if len(res.Gates) == 0 {
		fmt.Fprintf(out, "no approvals are required (phase %s)\n", res.Phase)
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "GATE\tSERVICE\tGUARDS\tSTATUS\t")
	for _, g := range res.Gates {
		status := "pending"
		if g.Approved {
			status = fmt.Sprintf("approved by %s at %s", g.By, g.At.Local().Format("2006-01-02 15:04"))
		}
		note := ""
		if g.Current {
			note = "<- needed to leave " + string(res.Phase)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s -> %s\t%s\t%s\n", g.Gate, g.Service, g.From, g.To, status, note)
	}
	tw.Flush()
}

func newApproveCmd(a *app) *cobra.Command {
	var service string
	cmd := &cobra.Command{
		Use:   "approve <ticket-id> <gate>",
		Short: "Approve a gate so the ticket can advance",
		Long: `Record your approval of a gate: plan_approval (plan -> implement),
manual_test_signoff (test -> review), or review_approval (review -> push). Without
--service the approval covers every service that requires the gate. Only a human can
approve; agents can request a phase change but never grant a gate.`,
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.GateApproveParams{ID: args[0], Service: service}
			if err := p.Gate.UnmarshalText([]byte(args[1])); err != nil {
				return usageError(err)
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.GateListResult
			if err := c.Call(cmd.Context(), api.MethodGateApprove, p, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			printApproval(cmd, p.Gate, res)
			return nil
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "approve for this service only (default: every service that requires the gate)")
	return cmd
}

func printApproval(cmd *cobra.Command, gate domain.Gate, res api.GateListResult) {
	var approved, pending []string
	for _, g := range res.Gates {
		if g.Gate != gate {
			continue
		}
		if g.Approved {
			approved = append(approved, g.Service)
		} else {
			pending = append(pending, g.Service)
		}
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s approved for: %s\n", gate, strings.Join(approved, ", "))
	if len(pending) > 0 {
		fmt.Fprintf(out, "still needed from: %s\n", strings.Join(pending, ", "))
	} else {
		fmt.Fprintf(out, "every service has approved %s\n", gate)
	}
}
