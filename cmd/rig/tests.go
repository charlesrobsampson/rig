package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newTestsCmd(a *app) *cobra.Command {
	var done, undone string
	var signOff, override bool
	cmd := &cobra.Command{
		Use:   "tests <ticket-id>",
		Short: "Show the manual tests for a ticket, tick steps off, and sign them off",
		Long: `Print the steps from the ticket's manual-tests.md with their placeholders replaced by
the running ticket's addresses ({{svc.url}}, {{svc.port}}, {{svc.internal_url}},
{{dep.host}}, {{ticket.id}}), so the curl lines can be pasted as they are. Steps a
human has already ticked off are marked [x]. If the file has a mistake, every problem
is listed.

  --done <step>    tick a step off
  --undone <step>  tick it back on (this withdraws a sign-off)
  --sign-off       sign the manual tests off, which approves manual_test_signoff;
                   every step must be ticked, or add --override to sign off anyway`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ticket := args[0]
			changes := 0
			for _, set := range []bool{done != "", undone != "", signOff} {
				if set {
					changes++
				}
			}
			switch {
			case changes > 1:
				return usageError(fmt.Errorf("use only one of --done, --undone and --sign-off"))
			case override && !signOff:
				return usageError(fmt.Errorf("--override only goes with --sign-off"))
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			out := cmd.OutOrStdout()

			switch {
			case done != "" || undone != "":
				p := api.TestsStepParams{Ticket: ticket, Step: done, Done: true}
				if undone != "" {
					p.Step, p.Done = undone, false
				}
				var res api.TestsStepResult
				if err := c.Call(cmd.Context(), api.MethodTestsStep, p, &res); err != nil {
					return err
				}
				if a.json {
					return printJSON(out, res)
				}
				state := "done"
				if !res.Done {
					state = "not done"
				}
				fmt.Fprintf(out, "%s: step %s marked %s\n", ticket, res.Step, state)
				if !res.Done && !res.SignedOff && undone != "" {
					fmt.Fprintln(out, "(any earlier sign-off no longer holds)")
				}
				return nil
			case signOff:
				var res api.TestsSignoffResult
				if err := c.Call(cmd.Context(), api.MethodTestsSignoff, api.TestsSignoffParams{Ticket: ticket, Override: override}, &res); err != nil {
					return err
				}
				if a.json {
					return printJSON(out, res)
				}
				fmt.Fprintf(out, "%s: manual tests signed off\n", ticket)
				if len(res.Unchecked) > 0 {
					fmt.Fprintf(out, "overridden, steps not done: %s\n", strings.Join(res.Unchecked, ", "))
				}
				if res.Approved {
					fmt.Fprintln(out, "manual_test_signoff approved")
				}
				return nil
			}

			var res api.TestsRenderResult
			if err := c.Call(cmd.Context(), api.MethodTestsRender, api.TestsRenderParams{Ticket: ticket}, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(out, res)
			}
			writeTests(out, ticket, res)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&done, "done", "", "tick this step off")
	f.StringVar(&undone, "undone", "", "tick this step back on (withdraws a sign-off)")
	f.BoolVar(&signOff, "sign-off", false, "sign the manual tests off (approves manual_test_signoff)")
	f.BoolVar(&override, "override", false, "with --sign-off: sign off even though steps are not done")
	return cmd
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
