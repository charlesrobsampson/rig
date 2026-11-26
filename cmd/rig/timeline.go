package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

func newTimelineCmd(a *app) *cobra.Command {
	var since int
	cmd := &cobra.Command{
		Use:   "timeline <ticket-id>",
		Short: "Show a ticket's timeline: every change, once, in order",
		Long: `Print the ticket's timeline, oldest first: when it started and stopped, each phase
move, gate approval and request, every journal record, message, review round and
decision, sync and push, numbered from 1 without gaps. --since <seq> prints only the
entries after that number, so a reader can pick up where it stopped. --json prints each
entry with its full data.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if since < 0 {
				return usageError(fmt.Errorf("--since must be 0 (everything) or a seq"))
			}
			c, err := a.connect(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.TimelineListResult
			if err := c.Call(cmd.Context(), api.MethodTimelineList, api.TimelineListParams{Ticket: args[0], Since: since}, &res); err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), res)
			}
			writeTimelineTable(cmd, res.Entries)
			return nil
		},
	}
	cmd.Flags().IntVar(&since, "since", 0, "only entries after this seq")
	return cmd
}

func writeTimelineTable(cmd *cobra.Command, entries []api.TimelineEntry) {
	out := cmd.OutOrStdout()
	if len(entries) == 0 {
		fmt.Fprintln(out, "no entries")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SEQ\tAT\tKIND\tBY\tWHAT")
	for _, e := range entries {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", e.Seq, e.At.Local().Format("2006-01-02 15:04:05"), e.Kind, e.By, timelineSummary(e))
	}
	tw.Flush()
}

// timelineSummary is an entry's data on one line: key=value pairs in key order, the
// service first when the entry names one.
func timelineSummary(e api.TimelineEntry) string {
	var data map[string]any
	if json.Unmarshal(e.Data, &data) != nil {
		return ""
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v := data[k]
		switch x := v.(type) {
		case string:
			if x == "" {
				continue
			}
			v = strings.ReplaceAll(x, "\n", " ")
		case []any:
			if len(x) == 0 {
				continue
			}
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, " ")
}
