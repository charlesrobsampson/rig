package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newJournalCmd(a *app) *cobra.Command {
	var p api.JournalListParams
	cmd := &cobra.Command{
		Use:   "journal [ticket-id]",
		Short: "Show the records agents left in their tickets' journals",
		Long: `List journal records, oldest first: what agents reported about their work and their
environment, stamped by rig with the ticket, service, time and source (container, host,
or rig itself when it refused a badly written record). Without a ticket, every ticket's
records are listed. "rig journal --kind feedback" collects what agents said got in their
way.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if p.Limit < 0 {
				return usageError(fmt.Errorf("--limit must be 0 (all) or more"))
			}
			if len(args) == 1 {
				p.Ticket = args[0]
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.JournalListResult
			if err := c.Call(cmd.Context(), api.MethodJournalList, p, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			writeJournalTable(cmd, res.Records)
			return nil
		},
	}
	cmd.Flags().StringVar(&p.Kind, "kind", "", "only records of this kind (for example feedback)")
	cmd.Flags().StringVar(&p.Service, "service", "", "only records from this service")
	cmd.Flags().IntVar(&p.Limit, "limit", 0, "only the newest n records (0 means all)")
	return cmd
}

// journalTextWidth keeps one record on one line of the table; --json has the full text.
const journalTextWidth = 100

func writeJournalTable(cmd *cobra.Command, records []api.JournalRecord) {
	out := cmd.OutOrStdout()
	if len(records) == 0 {
		fmt.Fprintln(out, "no records")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tTICKET\tSERVICE\tSOURCE\tKIND\tTEXT")
	for _, r := range records {
		text, _, more := strings.Cut(r.Text, "\n")
		if len(text) > journalTextWidth {
			text, more = text[:journalTextWidth], true
		}
		if more {
			text += " …"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.At.Local().Format("2006-01-02 15:04:05"), r.Ticket, r.Service, r.Source, r.Kind, text)
	}
	tw.Flush()
}
