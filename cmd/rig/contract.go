package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newContractCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "contract",
		Short: "Review proposed changes to a ticket's service contract",
		Long: `A multi-service ticket's contract.md holds what its services have agreed on. An
agent that needs a change proposes it; you decide here. Accepting merges the change
into the agreed section and tells every agent; rejecting needs a reason, which goes
back to the proposer.`,
	}
	cmd.AddCommand(newContractListCmd(a), newContractAcceptCmd(a), newContractRejectCmd(a), newContractAckCmd(a))
	return cmd
}

func newContractListCmd(a *app) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list <ticket-id>",
		Short: "List the contract's proposals (open ones unless --all)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.ContractListResult
			if err := c.Call(cmd.Context(), api.MethodContractList, api.ContractListParams{Ticket: args[0]}, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			w := cmd.OutOrStdout()
			shown := 0
			for _, p := range res.Proposals {
				if p.Status != "open" && !all {
					continue
				}
				shown++
				fmt.Fprintf(w, "%s  from %s  [%s]\n  what: %s\n  why:  %s\n", p.ID, p.From, p.Status, indent(p.What), indent(p.Why))
				if p.Rejected != "" {
					fmt.Fprintf(w, "  rejected: %s\n", p.Rejected)
				}
			}
			if shown == 0 {
				fmt.Fprintln(w, "no open proposals")
			}
			for _, pb := range res.Problems {
				fmt.Fprintf(cmd.ErrOrStderr(), "contract.md %s\n", pb)
			}
			if res.Violation != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n  review it, then run: rig contract ack %s\n", res.Violation, args[0])
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include accepted and rejected proposals")
	return cmd
}

func newContractAcceptCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "accept <ticket-id> <proposal-id>",
		Short: "Accept a proposal: it becomes part of the agreed contract",
		Args:  usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return resolveContract(a, cmd, api.MethodContractAccept, api.ContractResolveParams{Ticket: args[0], Proposal: args[1]})
		},
	}
}

func newContractRejectCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "reject <ticket-id> <proposal-id> <reason>",
		Short: "Reject a proposal; the reason goes back to the agent that made it",
		Args:  usageArgs(cobra.MinimumNArgs(3)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return resolveContract(a, cmd, api.MethodContractReject, api.ContractResolveParams{Ticket: args[0], Proposal: args[1], Reason: strings.Join(args[2:], " ")})
		},
	}
}

func newContractAckCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "ack <ticket-id>",
		Short: "Acknowledge a direct edit of the agreed section",
		Long: `When the agreed section of contract.md is edited directly instead of through a
proposal, rig flags the ticket. After you have reviewed the edit (keep it or revert
it), this clears the flag.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			if err := c.Call(cmd.Context(), api.MethodContractAck, api.ContractAckParams{Ticket: args[0]}, nil); err != nil {
				return err
			}
			if !a.json {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: contract violation acknowledged\n", args[0])
			}
			return nil
		},
	}
}

func resolveContract(a *app, cmd *cobra.Command, method string, p api.ContractResolveParams) error {
	c, err := a.connect(cmd.Context(), true)
	if err != nil {
		return err
	}
	defer c.Close()
	var res api.ContractResolveResult
	if err := c.Call(cmd.Context(), method, p, &res); err != nil {
		return err
	}
	if a.json {
		return printJSON(cmd.OutOrStdout(), res)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s: %s %s\n", p.Ticket, res.Proposal, res.Outcome)
	return nil
}

func indent(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n        ") }
