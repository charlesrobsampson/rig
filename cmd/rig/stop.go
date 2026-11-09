package main

import (
	"bufio"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newStopCmd(a *app) *cobra.Command {
	var p api.TicketStopParams
	var yes bool
	cmd := &cobra.Command{
		Use:   "stop <ticket-id>",
		Short: "Tear down a ticket's checkouts without losing work",
		Long: `Stop a ticket: import each isolated clone's branch into the main repository, then
remove the checkouts. Branches are always kept.

If any service still holds work that deleting would lose (uncommitted changes, or
commits that could not be imported), nothing is removed and each service is listed.
--force discards that work instead; it asks first unless --yes is given. --keep-checkouts
imports and marks the ticket stopped but leaves the directories in place.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.ID = args[0]
			if p.Force && !yes && !confirm(cmd, "--force discards uncommitted work in the checkouts of "+p.ID+". Continue? [y/N] ") {
				return fmt.Errorf("aborted")
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.TicketStopResult
			if err := c.Call(cmd.Context(), api.MethodTicketStop, p, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			if len(res.Services) > 0 {
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "SERVICE\tIMPORTED\tREMOVED")
				for _, s := range res.Services {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Service, yesNo(s.Imported), yesNo(s.Removed))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			if len(res.Services) == 0 && len(res.Resources) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s is already stopped\n", p.ID)
			}
			if len(res.Resources) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "\nremoved: %s\n", strings.Join(res.Resources, ", "))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&p.Force, "force", false, "discard uncommitted and unimportable work instead of refusing")
	f.BoolVar(&p.KeepCheckouts, "keep-checkouts", false, "import and mark stopped, but leave the checkout directories")
	f.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// confirm asks on stderr and reads one line from stdin; anything but y or yes, including
// no input at all, is a no, so scripts must say --yes.
func confirm(cmd *cobra.Command, question string) bool {
	fmt.Fprint(cmd.ErrOrStderr(), question)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
