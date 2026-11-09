package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newTestsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "tests <ticket-id>",
		Short: "Show the manual tests for a ticket, with live addresses filled in",
		Long: `Print the steps from the ticket's manual-tests.md with their placeholders replaced by
the running ticket's addresses ({{svc.url}}, {{svc.port}}, {{svc.internal_url}},
{{dep.host}}, {{ticket.id}}), so the curl lines can be pasted as they are. Steps a
human has already ticked off are marked [x]. If the file has a mistake, every problem
is listed.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.TestsRenderResult
			if err := c.Call(cmd.Context(), api.MethodTestsRender, api.TestsRenderParams{Ticket: args[0]}, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			writeTests(cmd.OutOrStdout(), args[0], res)
			return nil
		},
	}
}

// writeTests formats the rendered steps; it is the one place that decides how they look.
func writeTests(out io.Writer, ticket string, res api.TestsRenderResult) {
	fmt.Fprintf(out, "Manual tests for %s\n", ticket)
	if res.Preamble != "" {
		fmt.Fprintf(out, "\n%s\n", res.Preamble)
	}
	if len(res.Steps) == 0 {
		fmt.Fprintln(out, "\nno steps yet: the agent writes them as `### <id>: <title>` sections in manual-tests.md")
		return
	}
	for _, s := range res.Steps {
		box := "[ ]"
		if s.Done {
			box = "[x]"
		}
		fmt.Fprintf(out, "\n%s %s: %s\n", box, s.ID, s.Title)
		for _, line := range strings.Split(s.Body, "\n") {
			if line == "" {
				fmt.Fprintln(out)
				continue
			}
			fmt.Fprintf(out, "    %s\n", line)
		}
	}
	if res.SignedOff {
		fmt.Fprintln(out, "\nsigned off")
	}
}
