package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newMsgCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "msg <ticket-id> <service> <text|->",
		Short: "Type a message into a running agent",
		Long: `Type a message into the service's agent as if you had typed it and pressed Enter;
rig marks it with "[rig]". Use - as the text to read it from standard input.

Rig never types into an agent that is waiting on a permission prompt (the Enter would
approve it): that is refused and nothing is sent. A message to an agent that is still
starting waits for it to be ready. Every message is logged in the ticket's
messages.jsonl.`,
		Args: usageArgs(cobra.MinimumNArgs(3)),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.Join(args[2:], " ")
			if text == "-" {
				var in io.Reader = cmd.InOrStdin()
				raw, err := io.ReadAll(in)
				if err != nil {
					return err
				}
				text = strings.TrimRight(string(raw), "\n")
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.AgentMessageResult
			if err := c.Call(cmd.Context(), api.MethodAgentMessage, api.AgentMessageParams{Ticket: args[0], Service: args[1], Text: text}, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "sent to %s/%s\n", args[0], res.Service)
			return nil
		},
	}
}
