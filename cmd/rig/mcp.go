package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/mcp"
)

func newMCPCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve rig to a supervising agent over MCP (stdio)",
		Long: `Run a Model Context Protocol server on standard input and output, so a supervising
agent (Claude Code, for one) can watch and nudge rig's coding agents. Add it with:

  claude mcp add rig -- rig mcp

Tools: rig_list_tickets, rig_get_ticket, rig_read_artifact, rig_send_message,
rig_request_transition and rig_propose_contract_change. Each is one daemon call. There
are no tools to approve a gate, accept a contract change, sign off tests, push or stop:
those stay with you. Protocol messages go to standard output, so nothing else is printed
there. See docs/mcp.md.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			srv := mcp.NewServer(mcp.Options{Daemon: daemonCaller{a}, Version: version, Stderr: cmd.ErrOrStderr()})
			return srv.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

// daemonCaller makes one daemon call per tool call, starting the daemon if it is not
// running, so the server survives daemon restarts.
type daemonCaller struct{ a *app }

func (d daemonCaller) Call(ctx context.Context, method string, params, result any) error {
	c, err := d.a.connect(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Call(ctx, method, params, result)
}
