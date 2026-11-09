package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/contract"
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
	cmd.AddCommand(newContractListCmd(a), newContractAcceptCmd(a), newContractRejectCmd(a), newContractAckCmd(a), newContractExportCmd(a), newContractCheckCmd(a))
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

// contractSource is where `export` and `check` read contract.md from: a ticket, through
// the daemon, or a file, so a service's own CI can use them without a rig daemon.
type contractSource struct{ file string }

func (c *contractSource) flags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&c.file, "file", "", "read this contract.md instead of a ticket's")
}

func (c contractSource) read(a *app, cmd *cobra.Command, args []string) (md, name string, err error) {
	switch {
	case c.file != "" && len(args) > 0:
		return "", "", usageError(fmt.Errorf("give a ticket id or --file, not both"))
	case c.file != "":
		raw, err := os.ReadFile(c.file)
		if err != nil {
			return "", "", err
		}
		return string(raw), c.file, nil
	case len(args) == 0:
		return "", "", usageError(fmt.Errorf("give a ticket id or --file <contract.md>"))
	}
	cl, err := a.connect(cmd.Context(), true)
	if err != nil {
		return "", "", err
	}
	defer cl.Close()
	var res api.TicketFileResult
	if err := cl.Call(cmd.Context(), api.MethodTicketFile, api.TicketFileParams{Ticket: args[0], Name: "contract.md"}, &res); err != nil {
		return "", "", err
	}
	return res.Content, args[0] + "/contract.md", nil
}

// specOf parses the contract and reports every problem with its line number; a contract
// with any problem is an error (exit 2), so a build using it cannot pass on a half-read one.
func specOf(cmd *cobra.Command, md, name string) (contract.Spec, error) {
	doc := contract.Parse(md)
	spec := doc.Spec()
	problems := append([]contract.Problem(nil), doc.Problems...)
	problems = append(problems, spec.Problems...)
	if len(problems) == 0 {
		return spec, nil
	}
	for _, p := range problems {
		if p.Line > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s:%d: %s\n", name, p.Line, p.Message)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", name, p.Message)
		}
	}
	return spec, usageError(fmt.Errorf("%s has %d problem(s); fix them first", name, len(problems)))
}

func newContractExportCmd(a *app) *cobra.Command {
	var src contractSource
	var format, output string
	cmd := &cobra.Command{
		Use:   "export [ticket-id]",
		Short: "Emit the contract's machine-readable endpoints and schemas (OpenAPI)",
		Long: `Turn the agreed contract's fenced endpoint and schema blocks into something a
service's test suite can check itself against. The contract is a ticket's contract.md, or
any file with --file (so a service's own CI can run this without a rig daemon).

  --format openapi       an OpenAPI 3.0 document as YAML (default)
  --format openapi-json  the same as JSON
  --format json          the parsed endpoints and schemas

A malformed block is reported with its line number, nothing is written, and the exit code
is 2, so a build never runs against a half-read contract. A contract with no blocks gives
an empty document. See docs/contract-tests.md for the block format and an example.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			md, name, err := src.read(a, cmd, args)
			if err != nil {
				return err
			}
			spec, err := specOf(cmd, md, name)
			if err != nil {
				return err
			}
			var out []byte
			switch format {
			case "openapi":
				out, err = spec.MarshalOpenAPIYAML()
			case "openapi-json":
				out, err = spec.MarshalOpenAPIJSON()
			case "json":
				out, err = json.MarshalIndent(spec, "", "  ")
				out = append(out, '\n')
			default:
				return usageError(fmt.Errorf("--format must be openapi, openapi-json or json, not %q", format))
			}
			if err != nil {
				return err
			}
			if len(spec.Endpoints) == 0 && len(spec.Schemas) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: %s has no endpoint or schema blocks\n", name)
			}
			if output != "" {
				return os.WriteFile(output, out, 0o644)
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	}
	src.flags(cmd)
	cmd.Flags().StringVar(&format, "format", "openapi", "openapi, openapi-json or json")
	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file instead of standard output")
	return cmd
}

func newContractCheckCmd(a *app) *cobra.Command {
	var src contractSource
	cmd := &cobra.Command{
		Use:   "check [ticket-id]",
		Short: "Check that the contract's machine-readable blocks are well formed",
		Long: `Read a ticket's contract.md (or --file) and report every malformed endpoint or schema
block with its line number. Exit 0 when everything is readable, 2 when not: put it in a
service's CI so a broken contract fails the build.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			md, name, err := src.read(a, cmd, args)
			if err != nil {
				return err
			}
			spec, err := specOf(cmd, md, name)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: ok, %d endpoint(s), %d schema(s)\n", name, len(spec.Endpoints), len(spec.Schemas))
			return nil
		},
	}
	src.flags(cmd)
	return cmd
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
